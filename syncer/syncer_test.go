package syncer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/oss-sync/config"
	"github.com/oss-sync/db"
	"github.com/oss-sync/store"
	"golang.org/x/time/rate"
)

type fakeSource struct {
	objects []store.Object
	data    map[string][]byte
	err     error
	listErr error
}

func (s *fakeSource) ListPage(prefix, pageToken string, pageSize int) ([]store.Object, string, bool, error) {
	if s.listErr != nil {
		return nil, "", false, s.listErr
	}
	var filtered []store.Object
	for _, obj := range s.objects {
		if prefix == "" || len(obj.Key) >= len(prefix) && obj.Key[:len(prefix)] == prefix {
			filtered = append(filtered, obj)
		}
	}

	start := 0
	if pageToken != "" {
		offset, err := strconv.Atoi(pageToken)
		if err != nil {
			return nil, "", false, err
		}
		start = offset
	}
	if start >= len(filtered) {
		return nil, "", false, nil
	}

	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	nextToken := ""
	if end < len(filtered) {
		nextToken = strconv.Itoa(end)
	}
	return filtered[start:end], nextToken, end < len(filtered), nil
}

func (s *fakeSource) GetObjectStream(key string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(bytes.NewReader(s.data[key])), nil
}

type fakeDest struct {
	mu       sync.Mutex
	uploaded map[string][]byte
	err      error
}

func (d *fakeDest) PutObjectFromStream(key string, body io.Reader, size int64) error {
	if d.err != nil {
		return d.err
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.uploaded == nil {
		d.uploaded = make(map[string][]byte)
	}
	d.uploaded[key] = payload
	return nil
}

func (d *fakeDest) Close() {}

type flakySource struct {
	failuresRemaining int
	data              map[string][]byte
}

func (s *flakySource) ListPage(prefix, pageToken string, pageSize int) ([]store.Object, string, bool, error) {
	return nil, "", false, nil
}

func (s *flakySource) GetObjectStream(key string) (io.ReadCloser, error) {
	if s.failuresRemaining > 0 {
		s.failuresRemaining--
		return nil, errors.New("temporary download failure")
	}
	return io.NopCloser(bytes.NewReader(s.data[key])), nil
}

type flakyDest struct {
	failuresRemaining int
	uploaded          map[string][]byte
}

func (d *flakyDest) PutObjectFromStream(key string, body io.Reader, size int64) error {
	if d.failuresRemaining > 0 {
		d.failuresRemaining--
		return errors.New("temporary upload failure")
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if d.uploaded == nil {
		d.uploaded = make(map[string][]byte)
	}
	d.uploaded[key] = payload
	return nil
}

func (d *flakyDest) Close() {}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	return database
}

func TestSyncerRunMapsSourceKeysToDestKeys(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	src := &fakeSource{
		objects: []store.Object{
			{Key: "images/raw/", Size: 0, LastModified: time.Now()},
			{Key: "images/raw/a.txt", Size: 3, ETag: "etag-a", LastModified: time.Now()},
			{Key: "images/raw/sub/b.txt", Size: 3, ETag: "etag-b", LastModified: time.Now()},
		},
		data: map[string][]byte{
			"images/raw/a.txt":     []byte("aaa"),
			"images/raw/sub/b.txt": []byte("bbb"),
		},
	}
	dst := &fakeDest{}
	cfg := &config.Config{
		Source: config.EndpointConfig{Prefix: "images/raw/"},
		Dest:   config.EndpointConfig{Prefix: "backup/2026/"},
		Sync: config.SyncConfig{
			Mode:        "full",
			Concurrency: 2,
			PageSize:    2,
			DBPath:      filepath.Join(t.TempDir(), "unused.db"),
		},
	}

	s := &Syncer{cfg: cfg, src: src, dst: dst, database: database}
	if err := s.RunWithContext(context.Background()); err != nil {
		t.Fatalf("run syncer: %v", err)
	}

	var uploaded []string
	for key := range dst.uploaded {
		uploaded = append(uploaded, key)
	}
	slices.Sort(uploaded)
	want := []string{"backup/2026/a.txt", "backup/2026/sub/b.txt"}
	if !slices.Equal(uploaded, want) {
		t.Fatalf("unexpected uploaded keys: got %v want %v", uploaded, want)
	}

	stats, err := database.StatsForScopes(cfg.Scopes())
	if err != nil {
		t.Fatalf("stats for scopes: %v", err)
	}
	if stats[db.StatusSynced] != 2 {
		t.Fatalf("expected 2 synced objects, got %v", stats)
	}
}

func TestSyncerUsesIndependentBaselinesPerMapping(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	now := time.Now().Add(-1 * time.Hour)
	src := &fakeSource{
		objects: []store.Object{
			{Key: "images/raw/a.txt", Size: 3, ETag: "etag-a", LastModified: now},
			{Key: "docs/readme.txt", Size: 4, ETag: "etag-doc", LastModified: now},
		},
		data: map[string][]byte{
			"images/raw/a.txt": []byte("aaa"),
			"docs/readme.txt":  []byte("read"),
		},
	}
	dst := &fakeDest{}
	cfg := &config.Config{
		Source: config.EndpointConfig{},
		Dest:   config.EndpointConfig{},
		Sync: config.SyncConfig{
			Mode:        "incremental",
			Concurrency: 2,
			PageSize:    10,
			Mappings: []config.PrefixMapping{
				{SourcePrefix: "images/raw/", DestPrefix: "backup/raw/"},
				{SourcePrefix: "docs/", DestPrefix: "backup/docs/"},
			},
		},
	}

	scopeWithHistory := cfg.ScopeForMapping(cfg.PrefixMappings()[0])
	sessionID, err := database.StartSession(scopeWithHistory, "incremental")
	if err != nil {
		t.Fatalf("start historical session: %v", err)
	}
	if err := database.FinishSession(sessionID, "completed", ""); err != nil {
		t.Fatalf("finish historical session: %v", err)
	}

	s := &Syncer{cfg: cfg, src: src, dst: dst, database: database}
	if err := s.RunWithContext(context.Background()); err != nil {
		t.Fatalf("run syncer: %v", err)
	}

	if _, ok := dst.uploaded["backup/raw/a.txt"]; ok {
		t.Fatalf("expected historical mapping object to be skipped by incremental baseline")
	}
	if got := string(dst.uploaded["backup/docs/readme.txt"]); got != "read" {
		t.Fatalf("expected docs mapping upload, got %q", got)
	}
}

func TestMapObjectKeyRejectsObjectsOutsideMapping(t *testing.T) {
	s := &Syncer{
		cfg: &config.Config{
			Source: config.EndpointConfig{},
			Dest:   config.EndpointConfig{},
		},
	}

	_, err := s.mapObjectKey(config.PrefixMapping{
		SourcePrefix: "images/raw/",
		DestPrefix:   "backup/raw/",
	}, "docs/readme.txt")
	if err == nil {
		t.Fatal("expected mapping error for object outside source prefix")
	}
}

func TestSyncerNewRunCloseAndProviderValidation(t *testing.T) {
	if _, err := buildSource(config.EndpointConfig{Provider: "invalid"}); err == nil {
		t.Fatal("expected invalid source provider error")
	}
	if _, err := buildDest(config.EndpointConfig{Provider: "invalid"}); err == nil {
		t.Fatal("expected invalid dest provider error")
	}

	if _, err := New(&config.Config{
		Source: config.EndpointConfig{Provider: "invalid"},
		Dest:   config.EndpointConfig{Provider: "obs"},
		Sync:   config.SyncConfig{DBPath: filepath.Join(t.TempDir(), "sync.db")},
	}); err == nil {
		t.Fatal("expected syncer.New to fail for invalid source provider")
	}

	database := testDB(t)
	dst := &fakeDest{}
	s := &Syncer{
		cfg: &config.Config{
			Sync: config.SyncConfig{Mode: "full", Concurrency: 1, PageSize: 10},
		},
		src: &fakeSource{
			objects: []store.Object{{Key: "hello.txt", Size: 5, ETag: "etag-1", LastModified: time.Now()}},
			data:    map[string][]byte{"hello.txt": []byte("hello")},
		},
		dst:      dst,
		database: database,
	}
	if err := s.Run(); err != nil {
		t.Fatalf("run wrapper: %v", err)
	}
	s.Close()
}

func TestWorkerProcessMarksFailuresAndSuccess(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	const scope = "scope-a"
	lastModified := time.Now().UTC().Format(time.RFC3339)

	if err := database.UpsertPending(scope, "dst/success.txt", "src/success.txt", "etag", 3, lastModified); err != nil {
		t.Fatalf("upsert success record: %v", err)
	}
	worker := Worker{
		scope:    scope,
		src:      &fakeSource{data: map[string][]byte{"src/success.txt": []byte("ok")}},
		dst:      &fakeDest{},
		database: database,
		limiter:  NewRateLimiter(0),
		tracker:  &TransferTracker{},
		retries:  3,
	}
	if err := worker.process(context.Background(), SyncTask{
		SourceKey: "src/success.txt",
		DestKey:   "dst/success.txt",
		Size:      2,
	}); err != nil {
		t.Fatalf("process success: %v", err)
	}
	record, err := database.GetRecord(scope, "dst/success.txt")
	if err != nil {
		t.Fatalf("get success record: %v", err)
	}
	if record.Status != db.StatusSynced {
		t.Fatalf("expected synced status, got %+v", record)
	}

	if err := database.UpsertPending(scope, "dst/download-fail.txt", "src/download-fail.txt", "etag", 3, lastModified); err != nil {
		t.Fatalf("upsert download fail record: %v", err)
	}
	worker.src = &fakeSource{err: errors.New("download failed")}
	if err := worker.process(context.Background(), SyncTask{
		SourceKey: "src/download-fail.txt",
		DestKey:   "dst/download-fail.txt",
		Size:      2,
	}); err == nil {
		t.Fatal("expected download failure")
	}
	record, err = database.GetRecord(scope, "dst/download-fail.txt")
	if err != nil {
		t.Fatalf("get failed record: %v", err)
	}
	if record.Status != db.StatusFailed {
		t.Fatalf("expected failed status after download error, got %+v", record)
	}

	if err := database.UpsertPending(scope, "dst/upload-fail.txt", "src/upload-fail.txt", "etag", 3, lastModified); err != nil {
		t.Fatalf("upsert upload fail record: %v", err)
	}
	worker.src = &fakeSource{data: map[string][]byte{"src/upload-fail.txt": []byte("ok")}}
	worker.dst = &fakeDest{err: errors.New("upload failed")}
	if err := worker.process(context.Background(), SyncTask{
		SourceKey: "src/upload-fail.txt",
		DestKey:   "dst/upload-fail.txt",
		Size:      2,
	}); err == nil {
		t.Fatal("expected upload failure")
	}
	record, err = database.GetRecord(scope, "dst/upload-fail.txt")
	if err != nil {
		t.Fatalf("get upload failed record: %v", err)
	}
	if record.Status != db.StatusFailed {
		t.Fatalf("expected failed status after upload error, got %+v", record)
	}
}

func TestWorkerProcessRetriesBeforeSuccess(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	const scope = "scope-retry"
	lastModified := time.Now().UTC().Format(time.RFC3339)
	if err := database.UpsertPending(scope, "dst/retry.txt", "src/retry.txt", "etag", 3, lastModified); err != nil {
		t.Fatalf("upsert retry record: %v", err)
	}

	worker := Worker{
		scope:    scope,
		src:      &flakySource{failuresRemaining: 2, data: map[string][]byte{"src/retry.txt": []byte("ok")}},
		dst:      &fakeDest{},
		database: database,
		limiter:  NewRateLimiter(0),
		tracker:  &TransferTracker{},
		retries:  3,
	}
	if err := worker.process(context.Background(), SyncTask{
		SourceKey: "src/retry.txt",
		DestKey:   "dst/retry.txt",
		Size:      2,
	}); err != nil {
		t.Fatalf("process with retries: %v", err)
	}

	record, err := database.GetRecord(scope, "dst/retry.txt")
	if err != nil {
		t.Fatalf("get retry record: %v", err)
	}
	if record.Status != db.StatusSynced {
		t.Fatalf("expected synced after retries, got %+v", record)
	}
}

func TestWorkerProcessMarksFailedAfterRetriesExhausted(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	const scope = "scope-retry-failed"
	lastModified := time.Now().UTC().Format(time.RFC3339)
	if err := database.UpsertPending(scope, "dst/retry-fail.txt", "src/retry-fail.txt", "etag", 3, lastModified); err != nil {
		t.Fatalf("upsert retry-fail record: %v", err)
	}

	worker := Worker{
		scope:    scope,
		src:      &fakeSource{data: map[string][]byte{"src/retry-fail.txt": []byte("ok")}},
		dst:      &flakyDest{failuresRemaining: 3},
		database: database,
		limiter:  NewRateLimiter(0),
		tracker:  &TransferTracker{},
		retries:  3,
	}
	if err := worker.process(context.Background(), SyncTask{
		SourceKey: "src/retry-fail.txt",
		DestKey:   "dst/retry-fail.txt",
		Size:      2,
	}); err == nil {
		t.Fatal("expected retries exhausted error")
	}

	record, err := database.GetRecord(scope, "dst/retry-fail.txt")
	if err != nil {
		t.Fatalf("get retry-fail record: %v", err)
	}
	if record.Status != db.StatusFailed {
		t.Fatalf("expected failed after retries exhausted, got %+v", record)
	}
}

func TestWrapReaderAndRateLimiter(t *testing.T) {
	src := bytes.NewBufferString("abcdef")
	unlimited := NewRateLimiter(0)
	if got := WrapReader(context.Background(), src, unlimited, nil); got != src {
		t.Fatal("expected unlimited limiter to return original reader")
	}
	limited := NewRateLimiter(1)
	if limited == nil || limited.Limit() == rate.Inf || limited.Burst() <= 0 {
		t.Fatalf("expected finite limiter with positive burst, got limit=%v burst=%d", limited.Limit(), limited.Burst())
	}

	tracker := &TransferTracker{}
	limitedReader := WrapReader(context.Background(), bytes.NewBufferString("abcdef"), rate.NewLimiter(1, 1), tracker)
	buf := make([]byte, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := &rateLimitedReader{
		ctx:     ctx,
		r:       bytes.NewBufferString("abcdef"),
		limiter: rate.NewLimiter(1, 1),
	}
	n, err := rr.Read(buf)
	if n != 1 {
		t.Fatalf("expected read to be capped by burst to 1 byte, got %d", n)
	}
	if err == nil {
		t.Fatal("expected canceled context error from rate limiter")
	}

	readBuf := make([]byte, 2)
	n, err = limitedReader.Read(readBuf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected limited reader error: %v", err)
	}
	if n == 0 {
		t.Fatal("expected limited reader to produce data")
	}
	if tracker.bytesTransferred.Load() == 0 {
		t.Fatal("expected tracker to record transferred bytes")
	}
}

func TestWorkerPoolCancelStopsSubmission(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	pool := NewWorkerPool(context.Background(), 1, "scope-a", &fakeSource{}, &fakeDest{}, database, NewRateLimiter(0), &TransferTracker{}, 3)
	pool.Cancel()
	for range 20 {
		if ok := pool.Submit(SyncTask{SourceKey: "src/a.txt", DestKey: "dst/a.txt", Size: 1}); !ok {
			pool.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected submit to fail after cancel")
}

func TestRunWithContextMarksFailedSessionOnListError(t *testing.T) {
	database := testDB(t)
	defer database.Close()

	cfg := &config.Config{
		Sync: config.SyncConfig{
			Mode:        "full",
			Concurrency: 1,
			PageSize:    10,
			Mappings: []config.PrefixMapping{
				{SourcePrefix: "broken/", DestPrefix: "dst/"},
			},
		},
	}
	s := &Syncer{
		cfg:      cfg,
		src:      &fakeSource{listErr: errors.New("list failed")},
		dst:      &fakeDest{},
		database: database,
	}

	if err := s.RunWithContext(context.Background()); err == nil {
		t.Fatal("expected list error")
	}

	stats, err := database.GetFullStatsForScopes(cfg.Scopes())
	if err != nil {
		t.Fatalf("get full stats after list failure: %v", err)
	}
	if stats.Session == nil || stats.Session.Status != "failed" {
		t.Fatalf("expected failed session after list error, got %+v", stats.Session)
	}
}
