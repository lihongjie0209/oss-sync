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
	Key          string
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
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS sync_records (
			key           TEXT PRIMARY KEY,
			etag          TEXT NOT NULL DEFAULT '',
			size          INTEGER NOT NULL DEFAULT 0,
			last_modified TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT 'pending',
			synced_at     TEXT,
			error_msg     TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_status    ON sync_records(status);
		CREATE INDEX IF NOT EXISTS idx_synced_at ON sync_records(synced_at);

		CREATE TABLE IF NOT EXISTS sync_sessions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			mode         TEXT    NOT NULL,
			started_at   TEXT    NOT NULL,
			finished_at  TEXT,
			status       TEXT    NOT NULL DEFAULT 'running',
			error_msg    TEXT
		);
	`)
	return err
}

// GetRecord returns nil if the record does not exist.
func (d *DB) GetRecord(key string) (*SyncRecord, error) {
	row := d.conn.QueryRow(
		`SELECT key, etag, size, last_modified, status, COALESCE(synced_at,''), COALESCE(error_msg,'')
		 FROM sync_records WHERE key = ?`, key)

	r := &SyncRecord{}
	err := row.Scan(&r.Key, &r.ETag, &r.Size, &r.LastModified, &r.Status, &r.SyncedAt, &r.ErrorMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get record %s: %w", key, err)
	}
	return r, nil
}

// UpsertPending inserts or updates a record to pending state (used during listing).
func (d *DB) UpsertPending(key, etag string, size int64, lastModified string) error {
	_, err := d.conn.Exec(`
		INSERT INTO sync_records (key, etag, size, last_modified, status)
		VALUES (?, ?, ?, ?, 'pending')
		ON CONFLICT(key) DO UPDATE SET
			etag          = excluded.etag,
			size          = excluded.size,
			last_modified = excluded.last_modified,
			status        = CASE WHEN etag != excluded.etag THEN 'pending' ELSE status END
	`, key, etag, size, lastModified)
	return err
}

// MarkSynced marks a record as successfully synced.
func (d *DB) MarkSynced(key string) error {
	_, err := d.conn.Exec(`
		UPDATE sync_records SET status = 'synced', synced_at = ?, error_msg = NULL
		WHERE key = ?
	`, time.Now().UTC().Format(time.RFC3339), key)
	return err
}

// MarkFailed marks a record as failed with an error message.
func (d *DB) MarkFailed(key, errMsg string) error {
	_, err := d.conn.Exec(`
		UPDATE sync_records SET status = 'failed', error_msg = ?
		WHERE key = ?
	`, errMsg, key)
	return err
}

// LoadETagsForKeys returns synced ETags for a specific set of keys.
// Only the ETags for the provided keys are fetched, so memory usage is
// O(len(keys)) — bounded by page_size regardless of total record count.
// This replaces the old LoadSyncedETags full-table load.
func (d *DB) LoadETagsForKeys(keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return make(map[string]string), nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	query := fmt.Sprintf(
		`SELECT key, etag FROM sync_records WHERE status = 'synced' AND key IN (%s)`,
		placeholders,
	)

	args := make([]interface{}, len(keys))
	for i, k := range keys {
		args[i] = k
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
func (d *DB) IterateStaleRecords(baseline time.Time, fn func(key string, size int64) error) error {
	if baseline.IsZero() {
		return nil // full sync: all objects are re-listed, no stale pass needed
	}

	rows, err := d.conn.Query(`
		SELECT key, size FROM sync_records
		WHERE  status       IN ('pending', 'failed')
		  AND  last_modified <= ?`,
		baseline.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("iterate stale records: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var size int64
		if err := rows.Scan(&key, &size); err != nil {
			return err
		}
		if err := fn(key, size); err != nil {
			return err
		}
	}
	return rows.Err()
}
func (d *DB) GetLastSyncTime() (time.Time, error) {
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
		WHERE status = 'completed'
		ORDER BY id DESC LIMIT 1`)
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
		INSERT INTO sync_records (key, etag, size, last_modified, status)
		VALUES (?, ?, ?, ?, 'pending')
		ON CONFLICT(key) DO UPDATE SET
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
		if _, err := stmt.Exec(r.Key, r.ETag, r.Size, r.LastModified); err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert pending %s: %w", r.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
func (d *DB) Stats() (map[string]int64, error) {
	rows, err := d.conn.Query(`SELECT status, COUNT(*) FROM sync_records GROUP BY status`)
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

// FullStats combines per-status record counts with the latest session.
type FullStats struct {
	Pending int64
	Synced  int64
	Failed  int64
	Total   int64
	Session *SyncSession
}

// StartSession inserts a new running session and returns its ID.
func (d *DB) StartSession(mode string) (int64, error) {
	res, err := d.conn.Exec(
		`INSERT INTO sync_sessions (mode, started_at, status) VALUES (?, ?, 'running')`,
		mode, time.Now().UTC().Format(time.RFC3339),
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
func (d *DB) LatestSession() (*SyncSession, error) {
	row := d.conn.QueryRow(`
		SELECT id, mode, started_at, COALESCE(finished_at,''), status, COALESCE(error_msg,'')
		FROM sync_sessions ORDER BY id DESC LIMIT 1`)
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
func (d *DB) GetFullStats() (FullStats, error) {
	counts, err := d.Stats()
	if err != nil {
		return FullStats{}, err
	}
	session, err := d.LatestSession()
	if err != nil {
		return FullStats{}, err
	}
	fs := FullStats{
		Pending: counts[StatusPending],
		Synced:  counts[StatusSynced],
		Failed:  counts[StatusFailed],
		Session: session,
	}
	fs.Total = fs.Pending + fs.Synced + fs.Failed
	return fs, nil
}
