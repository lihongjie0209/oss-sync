package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusPending = "pending"
	StatusSynced  = "synced"
	StatusFailed  = "failed"
)

type SyncRecord struct {
	Scope        string
	Key          string
	SourceKey    string
	ETag         string
	Size         int64
	LastModified string
	Status       string
	SyncedAt     string
	ErrorMsg     string
}

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	conn.SetMaxOpenConns(1) // sqlite WAL is safe with single writer

	d := &DB{conn: conn}
	if err := d.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

func (d *DB) migrate() error {
	if err := d.migrateSyncRecords(); err != nil {
		return err
	}
	return d.migrateSyncSessions()
}

func (d *DB) migrateSyncRecords() error {
	var count int
	if err := d.conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sync_records'`,
	).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return d.createSyncRecordsTable()
	}

	hasScope := false
	hasSourceKey := false
	rows, err := d.conn.Query(`PRAGMA table_info(sync_records)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			dataType  string
			notNull   int
			defaultV  sql.NullString
			pkOrdinal int
		)
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultV, &pkOrdinal); err != nil {
			return err
		}
		switch name {
		case "scope":
			hasScope = true
		case "source_key":
			hasSourceKey = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasScope && hasSourceKey {
		return nil
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("begin sync_records migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		ALTER TABLE sync_records RENAME TO sync_records_old;
		CREATE TABLE sync_records (
			scope         TEXT NOT NULL DEFAULT '',
			key           TEXT NOT NULL,
			source_key    TEXT NOT NULL DEFAULT '',
			etag          TEXT NOT NULL DEFAULT '',
			size          INTEGER NOT NULL DEFAULT 0,
			last_modified TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT 'pending',
			synced_at     TEXT,
			error_msg     TEXT,
			PRIMARY KEY (scope, key)
		);
		CREATE INDEX idx_sync_records_scope_status    ON sync_records(scope, status);
		CREATE INDEX idx_sync_records_scope_synced_at ON sync_records(scope, synced_at);
		INSERT INTO sync_records (scope, key, source_key, etag, size, last_modified, status, synced_at, error_msg)
		SELECT '', key, key, etag, size, last_modified, status, synced_at, error_msg
		FROM sync_records_old;
		DROP TABLE sync_records_old;
	`); err != nil {
		return fmt.Errorf("migrate sync_records schema: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sync_records migration: %w", err)
	}
	return nil
}

func (d *DB) createSyncRecordsTable() error {
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS sync_records (
			scope         TEXT NOT NULL DEFAULT '',
			key           TEXT NOT NULL,
			source_key    TEXT NOT NULL DEFAULT '',
			etag          TEXT NOT NULL DEFAULT '',
			size          INTEGER NOT NULL DEFAULT 0,
			last_modified TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT 'pending',
			synced_at     TEXT,
			error_msg     TEXT,
			PRIMARY KEY (scope, key)
		);
		CREATE INDEX IF NOT EXISTS idx_sync_records_scope_status    ON sync_records(scope, status);
		CREATE INDEX IF NOT EXISTS idx_sync_records_scope_synced_at ON sync_records(scope, synced_at);
	`)
	return err
}

func (d *DB) ensureSyncSessionsScope() error {
	rows, err := d.conn.Query(`PRAGMA table_info(sync_sessions)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			dataType  string
			notNull   int
			defaultV  sql.NullString
			pkOrdinal int
		)
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultV, &pkOrdinal); err != nil {
			return err
		}
		if name == "scope" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = d.conn.Exec(`
		ALTER TABLE sync_sessions ADD COLUMN scope TEXT NOT NULL DEFAULT '';
	`)
	return err
}

func (d *DB) migrateSyncSessions() error {
	var count int
	if err := d.conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sync_sessions'`,
	).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err := d.conn.Exec(`
			CREATE TABLE sync_sessions (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				scope        TEXT    NOT NULL DEFAULT '',
				mode         TEXT    NOT NULL,
				started_at   TEXT    NOT NULL,
				finished_at  TEXT,
				status       TEXT    NOT NULL DEFAULT 'running',
				error_msg    TEXT
			);
			CREATE INDEX idx_sync_sessions_scope_status_id ON sync_sessions(scope, status, id DESC);
		`)
		return err
	}

	if err := d.ensureSyncSessionsScope(); err != nil {
		return err
	}
	_, err := d.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_sync_sessions_scope_status_id ON sync_sessions(scope, status, id DESC)`)
	return err
}

// GetRecord returns nil if the record does not exist.
func (d *DB) GetRecord(scope, key string) (*SyncRecord, error) {
	row := d.conn.QueryRow(
		`SELECT scope, key, source_key, etag, size, last_modified, status, COALESCE(synced_at,''), COALESCE(error_msg,'')
		 FROM sync_records WHERE scope = ? AND key = ?`, scope, key)

	r := &SyncRecord{}
	err := row.Scan(&r.Scope, &r.Key, &r.SourceKey, &r.ETag, &r.Size, &r.LastModified, &r.Status, &r.SyncedAt, &r.ErrorMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get record %s: %w", key, err)
	}
	return r, nil
}

func (d *DB) FailedRecordsForScopes(scopes []string, limit int) ([]SyncRecord, error) {
	if len(scopes) == 0 || limit <= 0 {
		return nil, nil
	}

	var (
		rows *sql.Rows
		err  error
	)
	if len(scopes) == 1 {
		rows, err = d.conn.Query(`
			SELECT scope, key, source_key, etag, size, last_modified, status, COALESCE(synced_at,''), COALESCE(error_msg,'')
			FROM sync_records
			WHERE scope = ? AND status = 'failed'
			ORDER BY last_modified DESC, key ASC
			LIMIT ?
		`, scopes[0], limit)
	} else {
		placeholders, args := buildInClause(scopes)
		query := fmt.Sprintf(`
			SELECT scope, key, source_key, etag, size, last_modified, status, COALESCE(synced_at,''), COALESCE(error_msg,'')
			FROM sync_records
			WHERE scope IN (%s) AND status = 'failed'
			ORDER BY last_modified DESC, key ASC
			LIMIT ?
		`, placeholders)
		args = append(args, limit)
		rows, err = d.conn.Query(query, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("list failed records: %w", err)
	}
	defer rows.Close()

	failed := make([]SyncRecord, 0, limit)
	for rows.Next() {
		var record SyncRecord
		if err := rows.Scan(
			&record.Scope,
			&record.Key,
			&record.SourceKey,
			&record.ETag,
			&record.Size,
			&record.LastModified,
			&record.Status,
			&record.SyncedAt,
			&record.ErrorMsg,
		); err != nil {
			return nil, err
		}
		failed = append(failed, record)
	}
	return failed, rows.Err()
}

// UpsertPending inserts or updates a record to pending state (used during listing).
func (d *DB) UpsertPending(scope, key, sourceKey, etag string, size int64, lastModified string) error {
	_, err := d.conn.Exec(`
		INSERT INTO sync_records (scope, key, source_key, etag, size, last_modified, status)
		VALUES (?, ?, ?, ?, ?, ?, 'pending')
		ON CONFLICT(scope, key) DO UPDATE SET
			source_key    = excluded.source_key,
			etag          = excluded.etag,
			size          = excluded.size,
			last_modified = excluded.last_modified,
			status        = CASE WHEN etag != excluded.etag THEN 'pending' ELSE status END
	`, scope, key, sourceKey, etag, size, lastModified)
	return err
}

// MarkSynced marks a record as successfully synced.
func (d *DB) MarkSynced(scope, key string) error {
	_, err := d.conn.Exec(`
		UPDATE sync_records SET status = 'synced', synced_at = ?, error_msg = NULL
		WHERE scope = ? AND key = ?
	`, time.Now().UTC().Format(time.RFC3339), scope, key)
	return err
}

// MarkFailed marks a record as failed with an error message.
func (d *DB) MarkFailed(scope, key, errMsg string) error {
	_, err := d.conn.Exec(`
		UPDATE sync_records SET status = 'failed', error_msg = ?
		WHERE scope = ? AND key = ?
	`, errMsg, scope, key)
	return err
}

// LoadETagsForKeys returns synced ETags for a specific set of keys.
// Only the ETags for the provided keys are fetched, so memory usage is
// O(len(keys)) — bounded by page_size regardless of total record count.
// This replaces the old LoadSyncedETags full-table load.
func (d *DB) LoadETagsForKeys(scope string, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return make(map[string]string), nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	query := fmt.Sprintf(
		`SELECT key, etag FROM sync_records WHERE scope = ? AND status = 'synced' AND key IN (%s)`,
		placeholders,
	)

	args := make([]interface{}, 0, len(keys)+1)
	args = append(args, scope)
	for _, k := range keys {
		args = append(args, k)
	}

	rows, err := d.conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("load etags for keys: %w", err)
	}
	defer rows.Close()

	m := make(map[string]string, len(keys))
	for rows.Next() {
		var key, etag string
		if err := rows.Scan(&key, &etag); err != nil {
			return nil, err
		}
		m[key] = etag
	}
	return m, rows.Err()
}

// IterateStaleRecords calls fn for each pending/failed record whose
// last_modified is at or before baseline, processing rows one at a time
// via a DB cursor — never loads the full result set into memory.
//
// This covers objects left over from a previously interrupted sync run that
// would NOT be re-listed by the time filter in listAndSync (their LastModified
// is older than the incremental baseline).
//
// For full sync (baseline.IsZero()), the iteration is skipped entirely:
// every object is re-examined during the listing phase, so no separate stale
// pass is needed and there is no risk of double-submission.
//
// For incremental sync the two sets are disjoint:
//   - listing phase yields objects with LastModified > baseline
//   - this function yields objects with last_modified ≤ baseline
//
// Therefore no submitted-key tracking is needed to prevent double-submission.
func (d *DB) IterateStaleRecords(scope string, baseline time.Time, fn func(sourceKey, key string, size int64) error) error {
	if baseline.IsZero() {
		return nil // full sync: all objects are re-listed, no stale pass needed
	}

	rows, err := d.conn.Query(`
		SELECT source_key, key, size FROM sync_records
		WHERE  scope        = ?
		  AND  status       IN ('pending', 'failed')
		  AND  last_modified <= ?`,
		scope,
		baseline.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("iterate stale records: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sourceKey string
		var key string
		var size int64
		if err := rows.Scan(&sourceKey, &key, &size); err != nil {
			return err
		}
		if err := fn(sourceKey, key, size); err != nil {
			return err
		}
	}
	return rows.Err()
}
func (d *DB) GetLastSyncTime(scope string) (time.Time, error) {
	// Why started_at and not finished_at / MAX(synced_at)?
	//
	// Using MAX(synced_at) risks skipping objects modified DURING the previous
	// sync window: e.g. session 1 lists object A at T=5min (old ETag), the
	// source modifies A at T=30min, session 1 syncs the stale version at T=45min
	// (synced_at=T=45min), and session 1 finishes at T=60min.
	// With MAX(synced_at)≈T=60min as baseline, A's LastModified=T=30min < T=60min
	// → the new version is silently skipped forever.
	//
	// Using started_at of the last completed session (T=0min) causes session 2
	// to re-examine A (T=30min > T=0min).  ETag deduplication (applied in
	// listAndSync for both modes) then skips objects whose content hasn't changed,
	// so the overlap only costs extra list API calls, not redundant uploads.
	row := d.conn.QueryRow(`
		SELECT started_at FROM sync_sessions
		WHERE scope = ? AND status = 'completed'
		ORDER BY id DESC LIMIT 1`, scope)
	var ts sql.NullString
	if err := row.Scan(&ts); err == sql.ErrNoRows {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	if !ts.Valid || ts.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, ts.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse last sync time: %w", err)
	}
	return t, nil
}

// BatchUpsertPending writes multiple pending records in a single transaction.
func (d *DB) BatchUpsertPending(records []SyncRecord) error {
	if len(records) == 0 {
		return nil
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	stmt, err := tx.Prepare(`
		INSERT INTO sync_records (scope, key, source_key, etag, size, last_modified, status)
		VALUES (?, ?, ?, ?, ?, ?, 'pending')
		ON CONFLICT(scope, key) DO UPDATE SET
			source_key    = excluded.source_key,
			etag          = excluded.etag,
			size          = excluded.size,
			last_modified = excluded.last_modified,
			status        = CASE WHEN etag != excluded.etag THEN 'pending' ELSE status END
	`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.Exec(r.Scope, r.Key, r.SourceKey, r.ETag, r.Size, r.LastModified); err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert pending %s: %w", r.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
func (d *DB) Stats(scope string) (map[string]int64, error) {
	rows, err := d.conn.Query(`SELECT status, COUNT(*) FROM sync_records WHERE scope = ? GROUP BY status`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make(map[string]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		stats[status] = count
	}
	return stats, rows.Err()
}

func buildInClause(values []string) (string, []interface{}) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	args := make([]interface{}, 0, len(values))
	for _, value := range values {
		args = append(args, value)
	}
	return placeholders, args
}

func (d *DB) StatsForScopes(scopes []string) (map[string]int64, error) {
	if len(scopes) == 0 {
		return map[string]int64{}, nil
	}
	if len(scopes) == 1 {
		return d.Stats(scopes[0])
	}

	placeholders, args := buildInClause(scopes)
	query := fmt.Sprintf(`SELECT status, COUNT(*) FROM sync_records WHERE scope IN (%s) GROUP BY status`, placeholders)
	rows, err := d.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make(map[string]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		stats[status] = count
	}
	return stats, rows.Err()
}

// ─── Session management ────────────────────────────────────────────────────

// SyncSession holds metadata for a single sync run.
type SyncSession struct {
	ID         int64
	Mode       string
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string // running | completed | failed
	ErrorMsg   string
}

type RecentSyncRecord struct {
	SourceKey string
	Key       string
	Size      int64
	SyncedAt  time.Time
}

// FullStats combines per-status record counts with the latest session.
type FullStats struct {
	Pending       int64
	Synced        int64
	Failed        int64
	Total         int64
	SyncedBytes   int64
	AvgSyncedSize int64
	MaxSyncedSize int64
	Session       *SyncSession
}

// StartSession inserts a new running session and returns its ID.
func (d *DB) StartSession(scope, mode string) (int64, error) {
	res, err := d.conn.Exec(
		`INSERT INTO sync_sessions (scope, mode, started_at, status) VALUES (?, ?, ?, 'running')`,
		scope, mode, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("start session: %w", err)
	}
	return res.LastInsertId()
}

// FinishSession marks a session as completed or failed.
func (d *DB) FinishSession(id int64, status, errMsg string) error {
	_, err := d.conn.Exec(
		`UPDATE sync_sessions SET status = ?, finished_at = ?, error_msg = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), errMsg, id,
	)
	return err
}

// LatestSession returns the most recent session, or nil if none.
func (d *DB) LatestSession(scope string) (*SyncSession, error) {
	row := d.conn.QueryRow(`
		SELECT id, mode, started_at, COALESCE(finished_at,''), status, COALESCE(error_msg,'')
		FROM sync_sessions WHERE scope = ? ORDER BY id DESC LIMIT 1`, scope)
	return scanSession(row)
}

func (d *DB) LatestSessionForScopes(scopes []string) (*SyncSession, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	if len(scopes) == 1 {
		return d.LatestSession(scopes[0])
	}

	placeholders, args := buildInClause(scopes)
	query := fmt.Sprintf(`
		SELECT id, mode, started_at, COALESCE(finished_at,''), status, COALESCE(error_msg,'')
		FROM sync_sessions
		WHERE scope IN (%s)
		ORDER BY
			CASE status
				WHEN 'running' THEN 0
				WHEN 'failed' THEN 1
				ELSE 2
			END,
			id DESC
		LIMIT 1`, placeholders)
	row := d.conn.QueryRow(query, args...)
	return scanSession(row)
}

func scanSession(row *sql.Row) (*SyncSession, error) {
	var s SyncSession
	var startedAt, finishedAt, errMsg string
	err := row.Scan(&s.ID, &s.Mode, &startedAt, &finishedAt, &s.Status, &errMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	s.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	s.ErrorMsg = errMsg
	if finishedAt != "" {
		t, _ := time.Parse(time.RFC3339, finishedAt)
		s.FinishedAt = &t
	}
	return &s, nil
}

// GetFullStats returns object counts and the latest session in one call.
func (d *DB) GetFullStats(scope string) (FullStats, error) {
	counts, err := d.Stats(scope)
	if err != nil {
		return FullStats{}, err
	}
	syncedBytes, avgSyncedSize, maxSyncedSize, err := d.syncedSizeStats(scope)
	if err != nil {
		return FullStats{}, err
	}
	session, err := d.LatestSession(scope)
	if err != nil {
		return FullStats{}, err
	}
	fs := FullStats{
		Pending:       counts[StatusPending],
		Synced:        counts[StatusSynced],
		Failed:        counts[StatusFailed],
		SyncedBytes:   syncedBytes,
		AvgSyncedSize: avgSyncedSize,
		MaxSyncedSize: maxSyncedSize,
		Session:       session,
	}
	fs.Total = fs.Pending + fs.Synced + fs.Failed
	return fs, nil
}

func (d *DB) GetFullStatsForScopes(scopes []string) (FullStats, error) {
	if len(scopes) == 0 {
		return FullStats{}, nil
	}
	if len(scopes) == 1 {
		return d.GetFullStats(scopes[0])
	}

	counts, err := d.StatsForScopes(scopes)
	if err != nil {
		return FullStats{}, err
	}
	syncedBytes, avgSyncedSize, maxSyncedSize, err := d.syncedSizeStatsForScopes(scopes)
	if err != nil {
		return FullStats{}, err
	}
	session, err := d.LatestSessionForScopes(scopes)
	if err != nil {
		return FullStats{}, err
	}
	fs := FullStats{
		Pending:       counts[StatusPending],
		Synced:        counts[StatusSynced],
		Failed:        counts[StatusFailed],
		SyncedBytes:   syncedBytes,
		AvgSyncedSize: avgSyncedSize,
		MaxSyncedSize: maxSyncedSize,
		Session:       session,
	}
	fs.Total = fs.Pending + fs.Synced + fs.Failed
	return fs, nil
}

func (d *DB) syncedSizeStats(scope string) (int64, int64, int64, error) {
	var totalBytes, maxSize sql.NullInt64
	var avgSize sql.NullFloat64
	err := d.conn.QueryRow(`
		SELECT
			COALESCE(SUM(size), 0),
			COALESCE(AVG(size), 0),
			COALESCE(MAX(size), 0)
		FROM sync_records
		WHERE scope = ? AND status = 'synced'
	`, scope).Scan(&totalBytes, &avgSize, &maxSize)
	if err != nil {
		return 0, 0, 0, err
	}
	return totalBytes.Int64, int64(avgSize.Float64), maxSize.Int64, nil
}

func (d *DB) syncedSizeStatsForScopes(scopes []string) (int64, int64, int64, error) {
	if len(scopes) == 0 {
		return 0, 0, 0, nil
	}
	if len(scopes) == 1 {
		return d.syncedSizeStats(scopes[0])
	}

	placeholders, args := buildInClause(scopes)
	query := fmt.Sprintf(`
		SELECT
			COALESCE(SUM(size), 0),
			COALESCE(AVG(size), 0),
			COALESCE(MAX(size), 0)
		FROM sync_records
		WHERE scope IN (%s) AND status = 'synced'
	`, placeholders)

	var totalBytes, maxSize sql.NullInt64
	var avgSize sql.NullFloat64
	if err := d.conn.QueryRow(query, args...).Scan(&totalBytes, &avgSize, &maxSize); err != nil {
		return 0, 0, 0, err
	}
	return totalBytes.Int64, int64(avgSize.Float64), maxSize.Int64, nil
}

func (d *DB) RecentSyncedForScopes(scopes []string, limit int) ([]RecentSyncRecord, error) {
	if len(scopes) == 0 || limit <= 0 {
		return nil, nil
	}

	var (
		rows *sql.Rows
		err  error
	)
	if len(scopes) == 1 {
		rows, err = d.conn.Query(`
			SELECT source_key, key, size, synced_at
			FROM sync_records
			WHERE scope = ? AND status = 'synced' AND synced_at IS NOT NULL AND synced_at != ''
			ORDER BY synced_at DESC, key ASC
			LIMIT ?
		`, scopes[0], limit)
	} else {
		placeholders, args := buildInClause(scopes)
		query := fmt.Sprintf(`
			SELECT source_key, key, size, synced_at
			FROM sync_records
			WHERE scope IN (%s) AND status = 'synced' AND synced_at IS NOT NULL AND synced_at != ''
			ORDER BY synced_at DESC, key ASC
			LIMIT ?
		`, placeholders)
		args = append(args, limit)
		rows, err = d.conn.Query(query, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recent := make([]RecentSyncRecord, 0, limit)
	for rows.Next() {
		var (
			record   RecentSyncRecord
			syncedAt string
		)
		if err := rows.Scan(&record.SourceKey, &record.Key, &record.Size, &syncedAt); err != nil {
			return nil, err
		}
		record.SyncedAt, _ = time.Parse(time.RFC3339, syncedAt)
		recent = append(recent, record)
	}
	return recent, rows.Err()
}
