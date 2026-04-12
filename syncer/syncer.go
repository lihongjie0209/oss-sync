package syncer

import (
	"context"
	"fmt"
	"log"
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
		return store.NewOSSStore(ep.Endpoint, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket)
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
		return store.NewOSSStore(ep.Endpoint, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket)
	case "s3":
		return store.NewS3Store(ep.Endpoint, ep.Region, ep.AccessKeyID, ep.AccessKeySecret, ep.Bucket, ep.ForcePathStyle)
	default:
		return nil, fmt.Errorf("provider %q cannot be used as dest (obs | oss | s3)", ep.Provider)
	}
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

	// Record session start.
	sid, err := s.database.StartSession(mode)
	if err != nil {
		log.Printf("[WARN] could not record session: %v", err)
	}
	s.sessionID = sid

	var syncErr error
	switch mode {
	case "full":
		log.Println("[INFO] starting full sync")
		syncErr = s.listAndSync(ctx, time.Time{})
	case "incremental":
		log.Println("[INFO] starting incremental sync")
		syncErr = s.runIncremental(ctx)
	}

	// Persist session outcome.
	if sid != 0 {
		status := "completed"
		errMsg := ""
		if syncErr != nil {
			status = "failed"
			errMsg = syncErr.Error()
		}
		if ferr := s.database.FinishSession(sid, status, errMsg); ferr != nil {
			log.Printf("[WARN] could not finish session: %v", ferr)
		}
	}

	// Always print final stats so partial progress is visible.
	if stats, statsErr := s.database.Stats(); statsErr == nil {
		log.Printf("[INFO] final db stats: %v", stats)
	}
	return syncErr
}

func (s *Syncer) runIncremental(ctx context.Context) error {
	lastSync, err := s.database.GetLastSyncTime()
	if err != nil {
		return fmt.Errorf("get last sync time: %w", err)
	}
	if lastSync.IsZero() {
		log.Println("[INFO] no previous sync found, performing full scan")
	} else {
		log.Printf("[INFO] last sync at %s, syncing newer objects", lastSync.Format(time.RFC3339))
	}
	return s.listAndSync(ctx, lastSync)
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
func (s *Syncer) listAndSync(ctx context.Context, sinceTime time.Time) error {
	limiter := NewRateLimiter(s.cfg.Sync.RateLimitMbps)
	pool := NewWorkerPool(ctx, s.cfg.Sync.Concurrency, s.src, s.dst, s.database, limiter)
	defer pool.Close()

	var (
		pageToken string
		totalList int
		totalSync int
	)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		objects, nextToken, isTruncated, err := s.src.ListPage(
			s.cfg.Source.Prefix,
			pageToken,
			s.cfg.Sync.PageSize,
		)
		if err != nil {
			return fmt.Errorf("list page (token=%q): %w", pageToken, err)
		}

		totalList += len(objects)
		log.Printf("[INFO] listed %d objects (truncated=%v)", len(objects), isTruncated)

		// --- Pass 1: time filter + collect candidate keys for this page ---
		type candidate struct {
			key  string
			etag string
			size int64
			lm   string
		}
		var candidates []candidate
		var candidateKeys []string

		for _, obj := range objects {
			// Skip directory markers.
			if obj.Size == 0 && len(obj.Key) > 0 && obj.Key[len(obj.Key)-1] == '/' {
				continue
			}

			// Incremental: skip objects older than the baseline (started_at of last session).
			if !sinceTime.IsZero() && !obj.LastModified.After(sinceTime) {
				continue
			}

			candidates = append(candidates, candidate{
				key:  obj.Key,
				etag: obj.ETag,
				size: obj.Size,
				lm:   obj.LastModified.Format(time.RFC3339),
			})
			candidateKeys = append(candidateKeys, obj.Key)
		}

		// --- Pass 2: per-page ETag lookup (O(page_size), not O(total)) ---
		// Both modes: skip if ETag matches the last-synced record.
		// For full sync this is the only skip criterion.
		// For incremental this guards the overlap window.
		var syncedETags map[string]string
		if len(candidateKeys) > 0 {
			syncedETags, err = s.database.LoadETagsForKeys(candidateKeys)
			if err != nil {
				log.Printf("[WARN] load etags for page: %v", err)
				syncedETags = make(map[string]string)
			}
		}

		// --- Pass 3: build pending list and tasks, filtering by ETag ---
		var pending []db.SyncRecord
		var tasks []SyncTask

		for _, c := range candidates {
			if etag, ok := syncedETags[c.key]; ok && etag == c.etag {
				continue // content unchanged, skip
			}

			pending = append(pending, db.SyncRecord{
				Key:          c.key,
				ETag:         c.etag,
				Size:         c.size,
				LastModified: c.lm,
			})
			tasks = append(tasks, SyncTask{Key: c.key, Size: c.size})
		}

		// Single transaction per page — O(1) DB round-trips regardless of page size.
		if len(pending) > 0 {
			if err := s.database.BatchUpsertPending(pending); err != nil {
				log.Printf("[WARN] batch upsert pending: %v", err)
			}
		}

		for _, t := range tasks {
			if !pool.Submit(t) {
				return ctx.Err()
			}
			totalSync++
		}

		if !isTruncated {
			break
		}
		pageToken = nextToken
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
	iterErr := s.database.IterateStaleRecords(sinceTime, func(key string, size int64) error {
		if !pool.Submit(SyncTask{Key: key, Size: size}) {
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

