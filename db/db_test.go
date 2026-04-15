package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sync.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	return database, path
}

func TestDBScopedRecordsAndAggregatedStats(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	const (
		scopeA = "scope-a"
		scopeB = "scope-b"
	)
	oldLM := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	newLM := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	if err := database.UpsertPending(scopeA, "dst/a.txt", "src/a.txt", "etag-a", 10, oldLM); err != nil {
		t.Fatalf("upsert pending scopeA: %v", err)
	}
	if err := database.UpsertPending(scopeB, "dst/b.txt", "src/b.txt", "etag-b", 20, newLM); err != nil {
		t.Fatalf("upsert pending scopeB: %v", err)
	}

	record, err := database.GetRecord(scopeA, "dst/a.txt")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if record == nil || record.SourceKey != "src/a.txt" || record.Status != StatusPending {
		t.Fatalf("unexpected record: %+v", record)
	}

	if err := database.MarkSynced(scopeA, "dst/a.txt"); err != nil {
		t.Fatalf("mark synced: %v", err)
	}
	if err := database.MarkFailed(scopeB, "dst/b.txt", "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	etags, err := database.LoadETagsForKeys(scopeA, []string{"dst/a.txt", "dst/missing.txt"})
	if err != nil {
		t.Fatalf("load etags: %v", err)
	}
	if got := etags["dst/a.txt"]; got != "etag-a" {
		t.Fatalf("unexpected etag: %q", got)
	}

	statsA, err := database.Stats(scopeA)
	if err != nil {
		t.Fatalf("stats scopeA: %v", err)
	}
	if statsA[StatusSynced] != 1 {
		t.Fatalf("expected 1 synced in scopeA, got %v", statsA)
	}

	stats, err := database.StatsForScopes([]string{scopeA, scopeB})
	if err != nil {
		t.Fatalf("stats for scopes: %v", err)
	}
	if stats[StatusSynced] != 1 || stats[StatusFailed] != 1 {
		t.Fatalf("unexpected aggregated stats: %v", stats)
	}

	full, err := database.GetFullStatsForScopes([]string{scopeA, scopeB})
	if err != nil {
		t.Fatalf("full stats: %v", err)
	}
	if full.Total != 2 || full.Synced != 1 || full.Failed != 1 {
		t.Fatalf("unexpected full stats: %+v", full)
	}
	if full.SyncedBytes != 10 || full.AvgSyncedSize != 10 || full.MaxSyncedSize != 10 {
		t.Fatalf("unexpected synced size stats: %+v", full)
	}
}

func TestRecentSyncedForScopesAndSizeStats(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	const (
		scopeA = "scope-a"
		scopeB = "scope-b"
	)
	lastModified := time.Now().UTC().Format(time.RFC3339)
	if err := database.UpsertPending(scopeA, "dst/a.txt", "src/a.txt", "etag-a", 10, lastModified); err != nil {
		t.Fatalf("upsert scopeA: %v", err)
	}
	if err := database.UpsertPending(scopeB, "dst/b.txt", "src/b.txt", "etag-b", 25, lastModified); err != nil {
		t.Fatalf("upsert scopeB: %v", err)
	}
	if err := database.UpsertPending(scopeB, "dst/c.txt", "src/c.txt", "etag-c", 5, lastModified); err != nil {
		t.Fatalf("upsert scopeB second: %v", err)
	}
	if err := database.MarkSynced(scopeA, "dst/a.txt"); err != nil {
		t.Fatalf("mark synced scopeA: %v", err)
	}
	if err := database.MarkSynced(scopeB, "dst/b.txt"); err != nil {
		t.Fatalf("mark synced scopeB: %v", err)
	}
	if err := database.MarkSynced(scopeB, "dst/c.txt"); err != nil {
		t.Fatalf("mark synced scopeB second: %v", err)
	}

	if _, err := database.conn.Exec(`
		UPDATE sync_records SET synced_at = '2026-04-14T07:00:01Z' WHERE scope = ? AND key = 'dst/a.txt';
		UPDATE sync_records SET synced_at = '2026-04-14T07:00:03Z' WHERE scope = ? AND key = 'dst/b.txt';
		UPDATE sync_records SET synced_at = '2026-04-14T07:00:02Z' WHERE scope = ? AND key = 'dst/c.txt';
	`, scopeA, scopeB, scopeB); err != nil {
		t.Fatalf("set deterministic synced_at values: %v", err)
	}

	full, err := database.GetFullStatsForScopes([]string{scopeA, scopeB})
	if err != nil {
		t.Fatalf("get full stats for scopes: %v", err)
	}
	if full.Synced != 3 || full.SyncedBytes != 40 || full.AvgSyncedSize != 13 || full.MaxSyncedSize != 25 {
		t.Fatalf("unexpected aggregate transfer stats: %+v", full)
	}

	recent, err := database.RecentSyncedForScopes([]string{scopeA, scopeB}, 2)
	if err != nil {
		t.Fatalf("recent synced for scopes: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 recent rows, got %d", len(recent))
	}
	if recent[0].Key != "dst/b.txt" || recent[1].Key != "dst/c.txt" {
		t.Fatalf("unexpected recent order: %+v", recent)
	}
}

func TestFailedRecordsForScopes(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	const (
		scopeA = "scope-a"
		scopeB = "scope-b"
	)
	if err := database.UpsertPending(scopeA, "dst/a.txt", "src/a.txt", "etag-a", 10, "2026-04-15T01:00:00Z"); err != nil {
		t.Fatalf("upsert scopeA: %v", err)
	}
	if err := database.UpsertPending(scopeB, "dst/b.txt", "src/b.txt", "etag-b", 20, "2026-04-15T02:00:00Z"); err != nil {
		t.Fatalf("upsert scopeB: %v", err)
	}
	if err := database.MarkFailed(scopeA, "dst/a.txt", "download failed"); err != nil {
		t.Fatalf("mark failed scopeA: %v", err)
	}
	if err := database.MarkFailed(scopeB, "dst/b.txt", "upload failed"); err != nil {
		t.Fatalf("mark failed scopeB: %v", err)
	}

	failed, err := database.FailedRecordsForScopes([]string{scopeA, scopeB}, 10)
	if err != nil {
		t.Fatalf("failed records for scopes: %v", err)
	}
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed records, got %d", len(failed))
	}
	if failed[0].Key != "dst/b.txt" || failed[0].ErrorMsg != "upload failed" {
		t.Fatalf("unexpected first failed record: %+v", failed[0])
	}
	if failed[1].Key != "dst/a.txt" || failed[1].ErrorMsg != "download failed" {
		t.Fatalf("unexpected second failed record: %+v", failed[1])
	}
}

func TestBatchUpsertPendingAndGetFullStats(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	records := []SyncRecord{
		{Scope: "scope-a", Key: "dst/a.txt", SourceKey: "src/a.txt", ETag: "etag-a", Size: 10, LastModified: time.Now().UTC().Format(time.RFC3339)},
		{Scope: "scope-a", Key: "dst/b.txt", SourceKey: "src/b.txt", ETag: "etag-b", Size: 20, LastModified: time.Now().UTC().Format(time.RFC3339)},
	}
	if err := database.BatchUpsertPending(records); err != nil {
		t.Fatalf("batch upsert pending: %v", err)
	}

	stats, err := database.GetFullStats("scope-a")
	if err != nil {
		t.Fatalf("get full stats: %v", err)
	}
	if stats.Pending != 2 || stats.Total != 2 {
		t.Fatalf("unexpected full stats after batch upsert: %+v", stats)
	}

	if err := database.MarkSynced("scope-a", "dst/a.txt"); err != nil {
		t.Fatalf("mark synced after batch upsert: %v", err)
	}
	stats, err = database.GetFullStats("scope-a")
	if err != nil {
		t.Fatalf("get full stats after mark synced: %v", err)
	}
	if stats.Synced != 1 || stats.Pending != 1 {
		t.Fatalf("unexpected updated full stats: %+v", stats)
	}
}

func TestDBSessionsAndStaleRecordIteration(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	const scope = "scope-a"
	baseline := time.Now().Add(-30 * time.Minute).UTC()
	oldLM := baseline.Add(-1 * time.Minute).Format(time.RFC3339)
	newLM := baseline.Add(1 * time.Minute).Format(time.RFC3339)

	if err := database.UpsertPending(scope, "dst/old.txt", "src/old.txt", "etag-old", 10, oldLM); err != nil {
		t.Fatalf("upsert old pending: %v", err)
	}
	if err := database.UpsertPending(scope, "dst/new.txt", "src/new.txt", "etag-new", 20, newLM); err != nil {
		t.Fatalf("upsert new pending: %v", err)
	}
	if err := database.UpsertPending(scope, "dst/failed.txt", "src/failed.txt", "etag-failed", 30, oldLM); err != nil {
		t.Fatalf("upsert failed pending: %v", err)
	}
	if err := database.MarkFailed(scope, "dst/failed.txt", "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	var stale []string
	err := database.IterateStaleRecords(scope, baseline, func(sourceKey, key string, size int64) error {
		stale = append(stale, sourceKey+"->"+key)
		return nil
	})
	if err != nil {
		t.Fatalf("iterate stale records: %v", err)
	}
	if len(stale) != 2 {
		t.Fatalf("expected 2 stale records, got %v", stale)
	}

	sessionID, err := database.StartSession(scope, "incremental")
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := database.FinishSession(sessionID, "completed", ""); err != nil {
		t.Fatalf("finish session: %v", err)
	}

	lastSync, err := database.GetLastSyncTime(scope)
	if err != nil {
		t.Fatalf("get last sync time: %v", err)
	}
	if lastSync.IsZero() {
		t.Fatal("expected non-zero last sync time")
	}

	session, err := database.LatestSession(scope)
	if err != nil {
		t.Fatalf("latest session: %v", err)
	}
	if session == nil || session.Status != "completed" {
		t.Fatalf("unexpected latest session: %+v", session)
	}

	latest, err := database.LatestSessionForScopes([]string{scope})
	if err != nil {
		t.Fatalf("latest session for scopes: %v", err)
	}
	if latest == nil || latest.ID != sessionID {
		t.Fatalf("unexpected aggregated latest session: %+v", latest)
	}
}

func TestLatestSessionForScopesPrioritizesRunningThenFailed(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	completedID, err := database.StartSession("scope-completed", "full")
	if err != nil {
		t.Fatalf("start completed session: %v", err)
	}
	if err := database.FinishSession(completedID, "completed", ""); err != nil {
		t.Fatalf("finish completed session: %v", err)
	}

	failedID, err := database.StartSession("scope-failed", "full")
	if err != nil {
		t.Fatalf("start failed session: %v", err)
	}
	if err := database.FinishSession(failedID, "failed", "boom"); err != nil {
		t.Fatalf("finish failed session: %v", err)
	}

	session, err := database.LatestSessionForScopes([]string{"scope-completed", "scope-failed"})
	if err != nil {
		t.Fatalf("latest session for scopes: %v", err)
	}
	if session == nil || session.Status != "failed" {
		t.Fatalf("expected failed session to be prioritized, got %+v", session)
	}

	_, err = database.StartSession("scope-running", "incremental")
	if err != nil {
		t.Fatalf("start running session: %v", err)
	}
	session, err = database.LatestSessionForScopes([]string{"scope-completed", "scope-failed", "scope-running"})
	if err != nil {
		t.Fatalf("latest session for scopes with running: %v", err)
	}
	if session == nil || session.Status != "running" {
		t.Fatalf("expected running session to be prioritized, got %+v", session)
	}
}

func TestDBMigratesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()

	_, err = raw.Exec(`
		CREATE TABLE sync_records (
			key           TEXT PRIMARY KEY,
			etag          TEXT NOT NULL DEFAULT '',
			size          INTEGER NOT NULL DEFAULT 0,
			last_modified TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT 'pending',
			synced_at     TEXT,
			error_msg     TEXT
		);
		CREATE TABLE sync_sessions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			mode         TEXT    NOT NULL,
			started_at   TEXT    NOT NULL,
			finished_at  TEXT,
			status       TEXT    NOT NULL DEFAULT 'running',
			error_msg    TEXT
		);
		INSERT INTO sync_records (key, etag, size, last_modified, status) VALUES ('legacy-key', 'etag', 123, '2026-04-13T00:00:00Z', 'pending');
		INSERT INTO sync_sessions (mode, started_at, finished_at, status) VALUES ('full', '2026-04-13T00:00:00Z', '2026-04-13T00:01:00Z', 'completed');
	`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}

	database, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer database.Close()

	record, err := database.GetRecord("", "legacy-key")
	if err != nil {
		t.Fatalf("get migrated record: %v", err)
	}
	if record == nil || record.SourceKey != "legacy-key" {
		t.Fatalf("unexpected migrated record: %+v", record)
	}

	stats, err := database.Stats("")
	if err != nil {
		t.Fatalf("stats on migrated db: %v", err)
	}
	if stats[StatusPending] != 1 {
		t.Fatalf("unexpected migrated stats: %v", stats)
	}

	lastSync, err := database.GetLastSyncTime("")
	if err != nil {
		t.Fatalf("last sync on migrated db: %v", err)
	}
	if lastSync.IsZero() {
		t.Fatal("expected migrated completed session to produce last sync time")
	}
}

func TestGetLastSyncTimeAndStatsForEmptyScopes(t *testing.T) {
	database, _ := openTestDB(t)
	defer database.Close()

	lastSync, err := database.GetLastSyncTime("missing-scope")
	if err != nil {
		t.Fatalf("get last sync time for missing scope: %v", err)
	}
	if !lastSync.IsZero() {
		t.Fatalf("expected zero last sync for missing scope, got %v", lastSync)
	}

	stats, err := database.StatsForScopes(nil)
	if err != nil {
		t.Fatalf("stats for nil scopes: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("expected empty stats for nil scopes, got %v", stats)
	}

	fullStats, err := database.GetFullStatsForScopes(nil)
	if err != nil {
		t.Fatalf("full stats for nil scopes: %v", err)
	}
	if fullStats.Total != 0 || fullStats.Session != nil {
		t.Fatalf("unexpected full stats for nil scopes: %+v", fullStats)
	}
}
