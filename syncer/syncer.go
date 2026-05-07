package syncer

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/oss-sync/config"
	"github.com/oss-sync/db"
	"github.com/oss-sync/store"
)

// Syncer orchestrates the full or incremental sync run.
type Syncer struct {
	cfg       *config.Config
	src       store.Source
	dst       store.Destination
	database  *db.DB
	sessionID int64 // set during RunWithContext
}

type taskBatch struct {
	listed int
	tasks  []SyncTask
}

// EndpointCheckResult describes a successfully validated endpoint.
type EndpointCheckResult struct {
	Role     string
	Provider string
	Endpoint string
	Bucket   string
	Prefix   string
}

func mappingLabel(mapping config.PrefixMapping) string {
	return fmt.Sprintf("%s -> %s", mapping.SourcePrefix, mapping.DestPrefix)
}

func (s *Syncer) scope(mapping config.PrefixMapping) string {
	return s.cfg.ScopeForMapping(mapping)
}

func (s *Syncer) mapObjectKey(mapping config.PrefixMapping, sourceKey string) (string, error) {
	sourcePrefix := mapping.SourcePrefix
	relativeKey := sourceKey
	if sourcePrefix != "" {
		if !strings.HasPrefix(sourceKey, sourcePrefix) {
			return "", fmt.Errorf("source key %q does not match source.prefix %q", sourceKey, sourcePrefix)
		}
		relativeKey = strings.TrimPrefix(sourceKey, sourcePrefix)
	}
	return mapping.DestPrefix + relativeKey, nil
}

// New creates a Syncer, choosing the right Source/Destination implementation
// based on the provider field in the config.
func New(cfg *config.Config) (*Syncer, error) {
	src, err := buildSource(cfg.Source)
	if err != nil {
		return nil, fmt.Errorf("init source (%s): %w", cfg.Source.Provider, err)
	}

	dst, err := buildDest(cfg.Dest)
	if err != nil {
		return nil, fmt.Errorf("init dest (%s): %w", cfg.Dest.Provider, err)
	}

	database, err := db.Open(cfg.Sync.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	return &Syncer{cfg: cfg, src: src, dst: dst, database: database}, nil
}

// buildSource creates a Source from an endpoint config.
func buildSource(ep config.EndpointConfig) (store.Source, error) {
	switch ep.Provider {
	case "oss":
		return store.NewOSSStore(ep.Endpoint, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket, ep.InsecureSkipVerify)
	case "s3":
		return store.NewS3Store(ep.Endpoint, ep.Region, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket, ep.ForcePathStyle)
	default:
		return nil, fmt.Errorf("provider %q cannot be used as source (oss | s3)", ep.Provider)
	}
}

// buildDest creates a Destination from an endpoint config.
func buildDest(ep config.EndpointConfig) (store.Destination, error) {
	switch ep.Provider {
	case "obs":
		return store.NewOBSStore(ep.Endpoint, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket)
	case "oss":
		return store.NewOSSStore(ep.Endpoint, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket, ep.InsecureSkipVerify)
	case "s3":
		return store.NewS3Store(ep.Endpoint, ep.Region, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket, ep.ForcePathStyle)
	default:
		return nil, fmt.Errorf("provider %q cannot be used as dest (obs | oss | s3)", ep.Provider)
	}
}

// TestSourceConfig validates the source endpoint can be initialized and listed.
func TestSourceConfig(ep config.EndpointConfig) (EndpointCheckResult, error) {
	src, err := buildSource(ep)
	if err != nil {
		return EndpointCheckResult{}, fmt.Errorf("init source (%s): %w", ep.Provider, err)
	}
	if err := testSourceStore(src, ep.Prefix); err != nil {
		return EndpointCheckResult{}, fmt.Errorf("test source (%s): %w", ep.Provider, err)
	}
	return EndpointCheckResult{
		Role:     "source",
		Provider: ep.Provider,
		Endpoint: ep.Endpoint,
		Bucket:   ep.Bucket,
		Prefix:   ep.Prefix,
	}, nil
}

// TestDestConfig validates the destination endpoint can be initialized and reached.
func TestDestConfig(ep config.EndpointConfig) (EndpointCheckResult, error) {
	dst, err := buildDest(ep)
	if err != nil {
		return EndpointCheckResult{}, fmt.Errorf("init dest (%s): %w", ep.Provider, err)
	}
	defer dst.Close()
	if err := testDestinationStore(dst); err != nil {
		return EndpointCheckResult{}, fmt.Errorf("test dest (%s): %w", ep.Provider, err)
	}
	return EndpointCheckResult{
		Role:     "dest",
		Provider: ep.Provider,
		Endpoint: ep.Endpoint,
		Bucket:   ep.Bucket,
		Prefix:   ep.Prefix,
	}, nil
}

func testSourceStore(src store.Source, prefix string) error {
	if _, _, _, err := src.ListPage(prefix, "", 1); err != nil {
		return fmt.Errorf("list source objects: %w", err)
	}
	return nil
}

func testDestinationStore(dst store.Destination) error {
	probeable, ok := dst.(store.ProbeableDestination)
	if !ok {
		return fmt.Errorf("destination %T does not support connectivity probes", dst)
	}
	if err := probeable.Probe(); err != nil {
		return err
	}
	return nil
}

// Close releases all resources.
func (s *Syncer) Close() {
	s.dst.Close()
	s.database.Close()
}

// Run executes the sync according to the configured mode.
func (s *Syncer) Run() error {
	return s.RunWithContext(context.Background())
}

// RunWithContext executes the sync and respects ctx cancellation.
func (s *Syncer) RunWithContext(ctx context.Context) error {
	mode := s.cfg.Sync.Mode
	if mode != "full" && mode != "incremental" {
		return fmt.Errorf("unknown sync mode: %s", mode)
	}

	var syncErr error
	for _, mapping := range s.cfg.PrefixMappings() {
		log.Printf("[INFO] processing mapping %s", mappingLabel(mapping))

		scope := s.scope(mapping)
		sid, err := s.database.StartSession(scope, mode)
		if err != nil {
			log.Printf("[WARN] could not record session for %s: %v", mappingLabel(mapping), err)
		}
		s.sessionID = sid

		switch mode {
		case "full":
			log.Printf("[INFO] starting full sync for %s", mappingLabel(mapping))
			syncErr = s.listAndSync(ctx, mapping, time.Time{})
		case "incremental":
			log.Printf("[INFO] starting incremental sync for %s", mappingLabel(mapping))
			syncErr = s.runIncremental(ctx, mapping)
		}

		if sid != 0 {
			status := "completed"
			errMsg := ""
			if syncErr != nil {
				status = "failed"
				errMsg = syncErr.Error()
			}
			if ferr := s.database.FinishSession(sid, status, errMsg); ferr != nil {
				log.Printf("[WARN] could not finish session for %s: %v", mappingLabel(mapping), ferr)
			}
		}
		if syncErr != nil {
			break
		}
	}

	// Always print final stats so partial progress is visible.
	if stats, statsErr := s.database.StatsForScopes(s.cfg.Scopes()); statsErr == nil {
		log.Printf("[INFO] final db stats: %v", stats)
	}
	return syncErr
}

func (s *Syncer) runIncremental(ctx context.Context, mapping config.PrefixMapping) error {
	lastSync, err := s.database.GetLastSyncTime(s.scope(mapping))
	if err != nil {
		return fmt.Errorf("get last sync time: %w", err)
	}
	if lastSync.IsZero() {
		log.Printf("[INFO] no previous sync found for %s, performing full scan", mappingLabel(mapping))
	} else {
		log.Printf("[INFO] last sync for %s at %s, syncing newer objects", mappingLabel(mapping), lastSync.Format(time.RFC3339))
	}
	return s.listAndSync(ctx, mapping, lastSync)
}

// listAndSync pages through source objects and queues eligible ones for syncing.
// sinceTime is zero for full sync; for incremental it is the started_at of the
// last completed session (see db.GetLastSyncTime for the rationale).
//
// ETag deduplication is applied in BOTH modes:
//   - Full sync   : ETag match is the only skip criterion (no time filter).
//   - Incremental : time filter is the primary criterion; ETag dedup additionally
//     skips objects re-listed due to the overlap window that haven't actually
//     changed since they were last synced, avoiding redundant uploads.
//
// Memory model: O(page_size) per iteration — ETags are fetched only for the
// current page's candidate keys.  No full-table map is kept in memory.
func (s *Syncer) listAndSync(ctx context.Context, mapping config.PrefixMapping, sinceTime time.Time) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	limiter := NewRateLimiter(s.cfg.Sync.RateLimitMbps)
	scope := s.scope(mapping)
	tracker := RegisterTracker(scope)
	pool := NewWorkerPool(ctx, s.cfg.Sync.Concurrency, scope, s.src, s.dst, s.database, limiter, tracker, s.cfg.Sync.RetryCount, s.cfg.Dest.Visibility)
	defer pool.Close()

	var (
		totalList int
		totalSync int
	)
	batches := make(chan taskBatch, 16)
	produceErr := make(chan error, 1)
	go func() {
		produceErr <- s.produceTaskBatches(ctx, mapping, sinceTime, scope, tracker, batches)
	}()

	for batch := range batches {
		totalList += batch.listed
		for _, t := range batch.tasks {
			if !pool.Submit(t) {
				cancel()
				return ctx.Err()
			}
			totalSync++
		}
	}
	if err := <-produceErr; err != nil {
		return err
	}

	log.Printf("[INFO] listing done: listed=%d queued=%d — waiting for workers…", totalList, totalSync)

	// Re-queue pending/failed records left from a previously interrupted run.
	//
	// IterateStaleRecords yields records with last_modified ≤ sinceTime, which is
	// strictly disjoint from the listing phase (which only processes objects with
	// LastModified > sinceTime).  No submitted-key tracking is needed.
	//
	// For full sync (sinceTime.IsZero()), IterateStaleRecords is a no-op:
	// every object is re-examined by the listing phase anyway.
	var requeued int
	iterErr := s.database.IterateStaleRecords(scope, sinceTime, func(sourceKey, destKey string, size int64) error {
		if !pool.Submit(SyncTask{SourceKey: sourceKey, DestKey: destKey, Size: size}) {
			return ctx.Err()
		}
		requeued++
		return nil
	})
	if iterErr != nil {
		log.Printf("[WARN] iterate stale records: %v", iterErr)
	}
	if requeued > 0 {
		log.Printf("[INFO] re-queued %d stale pending/failed records from previous run", requeued)
	}

	return nil
}

func (s *Syncer) produceTaskBatches(
	ctx context.Context,
	mapping config.PrefixMapping,
	sinceTime time.Time,
	scope string,
	tracker *TransferTracker,
	batches chan<- taskBatch,
) error {
	defer close(batches)

	var pageToken string
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		objects, nextToken, isTruncated, err := s.src.ListPage(
			mapping.SourcePrefix,
			pageToken,
			s.cfg.Sync.PageSize,
		)
		if err != nil {
			return fmt.Errorf("list page (token=%q): %w", pageToken, err)
		}

		log.Printf("[INFO] listed %d objects (truncated=%v)", len(objects), isTruncated)

		type candidate struct {
			sourceKey string
			destKey   string
			etag      string
			size      int64
			lm        string
		}
		var candidates []candidate
		var candidateKeys []string

		for _, obj := range objects {
			if obj.Size == 0 && len(obj.Key) > 0 && obj.Key[len(obj.Key)-1] == '/' {
				continue
			}
			if !sinceTime.IsZero() && !obj.LastModified.After(sinceTime) {
				continue
			}

			destKey, err := s.mapObjectKey(mapping, obj.Key)
			if err != nil {
				return err
			}
			candidates = append(candidates, candidate{
				sourceKey: obj.Key,
				destKey:   destKey,
				etag:      obj.ETag,
				size:      obj.Size,
				lm:        obj.LastModified.Format(time.RFC3339),
			})
			candidateKeys = append(candidateKeys, destKey)
		}

		var syncedETags map[string]string
		if len(candidateKeys) > 0 {
			syncedETags, err = s.database.LoadETagsForKeys(scope, candidateKeys)
			if err != nil {
				log.Printf("[WARN] load etags for page: %v", err)
				syncedETags = make(map[string]string)
			}
		}

		var pending []db.SyncRecord
		var tasks []SyncTask
		for _, c := range candidates {
			if etag, ok := syncedETags[c.destKey]; ok && etag == c.etag {
				continue
			}

			pending = append(pending, db.SyncRecord{
				Scope:        scope,
				Key:          c.destKey,
				SourceKey:    c.sourceKey,
				ETag:         c.etag,
				Size:         c.size,
				LastModified: c.lm,
			})
			tasks = append(tasks, SyncTask{
				SourceKey: c.sourceKey,
				DestKey:   c.destKey,
				Size:      c.size,
			})
		}

		if len(pending) > 0 {
			if err := s.database.BatchUpsertPending(pending); err != nil {
				log.Printf("[WARN] batch upsert pending: %v", err)
			}
		}
		tracker.AddDiscovered(len(tasks))

		select {
		case batches <- taskBatch{listed: len(objects), tasks: tasks}:
		case <-ctx.Done():
			return ctx.Err()
		}

		if !isTruncated {
			tracker.MarkDiscoveryDone()
			return nil
		}
		pageToken = nextToken
	}
}
