// Package store persists targets and per-minute rollups in SQLite.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"net/url"
	"os"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"

	_ "modernc.org/sqlite"
)

// migrations are applied in order; PRAGMA user_version records progress.
// Never edit an existing entry, only append.
var migrations = []string{
	`CREATE TABLE targets (
		id      INTEGER PRIMARY KEY,
		key     TEXT NOT NULL UNIQUE,
		kind    TEXT NOT NULL,
		role    TEXT NOT NULL,
		name    TEXT NOT NULL,
		address TEXT NOT NULL,
		family  TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1
	);
	CREATE TABLE probe_minute (
		target_id INTEGER NOT NULL,
		ts        INTEGER NOT NULL,
		sent      INTEGER NOT NULL,
		recv      INTEGER NOT NULL,
		rtt_min REAL, rtt_avg REAL, rtt_p50 REAL, rtt_p95 REAL, rtt_p99 REAL, rtt_max REAL,
		jitter  REAL,
		PRIMARY KEY (target_id, ts)
	) WITHOUT ROWID;
	CREATE INDEX probe_minute_ts ON probe_minute(ts);`,

	`CREATE TABLE events (
		id         INTEGER PRIMARY KEY,
		kind       TEXT NOT NULL,
		scope      TEXT NOT NULL,
		class      TEXT NOT NULL DEFAULT '',
		started_at INTEGER NOT NULL,
		ended_at   INTEGER,
		details    TEXT NOT NULL DEFAULT '{}'
	);
	CREATE INDEX events_started ON events(started_at);
	CREATE TABLE alert_outbox (
		id              INTEGER PRIMARY KEY,
		created_at      INTEGER NOT NULL,
		kind            TEXT NOT NULL,
		payload         TEXT NOT NULL,
		status          TEXT NOT NULL DEFAULT 'pending', -- pending | sent | failed | superseded
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		sent_at         INTEGER,
		error           TEXT
	);
	CREATE INDEX alert_outbox_due ON alert_outbox(status, next_attempt_at);`,
}

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{
		"auto_vacuum(INCREMENTAL)", // only takes effect on a new, empty database
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"busy_timeout(5000)",
		"foreign_keys(ON)",
	} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this build (v%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// UpsertTarget inserts or updates a target by its Key and returns its ID.
// The row is marked enabled.
func (s *Store) UpsertTarget(ctx context.Context, t model.Target) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO targets (key, kind, role, name, address, family, enabled)
		VALUES (?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(key) DO UPDATE SET
			kind = excluded.kind, role = excluded.role, name = excluded.name,
			address = excluded.address, family = excluded.family, enabled = 1
		RETURNING id`,
		t.Key, t.Kind, t.Role, t.Name, t.Addr.String(), t.Family).Scan(&id)
	return id, err
}

// DisableTargetsExcept marks every target not in keep as disabled. History is kept.
func (s *Store) DisableTargetsExcept(ctx context.Context, keep []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE targets SET enabled = 0`); err != nil {
		return err
	}
	for _, id := range keep {
		if _, err := tx.ExecContext(ctx, `UPDATE targets SET enabled = 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Targets returns all targets, enabled or not.
func (s *Store) Targets(ctx context.Context) ([]model.Target, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key, kind, role, name, address, family, enabled FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Target
	for rows.Next() {
		var t model.Target
		var addr string
		if err := rows.Scan(&t.ID, &t.Key, &t.Kind, &t.Role, &t.Name, &addr, &t.Family, &t.Enabled); err != nil {
			return nil, err
		}
		t.Addr, _ = netip.ParseAddr(addr)
		out = append(out, t)
	}
	return out, rows.Err()
}

// WriteMinutes stores rollups in one transaction. If a row for the same
// target and minute exists (e.g. a partial minute flushed at shutdown), the
// two are merged: counts add up, min/max combine, averages are weighted by
// replies, and percentiles keep the worse value.
func (s *Store) WriteMinutes(ctx context.Context, stats []model.MinuteStat) error {
	if len(stats) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO probe_minute (target_id, ts, sent, recv, rtt_min, rtt_avg, rtt_p50, rtt_p95, rtt_p99, rtt_max, jitter)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(target_id, ts) DO UPDATE SET
			rtt_min = min(coalesce(rtt_min, excluded.rtt_min), coalesce(excluded.rtt_min, rtt_min)),
			rtt_max = max(coalesce(rtt_max, excluded.rtt_max), coalesce(excluded.rtt_max, rtt_max)),
			rtt_avg = `+weighted("rtt_avg")+`,
			rtt_p50 = `+weighted("rtt_p50")+`,
			jitter  = `+weighted("jitter")+`,
			rtt_p95 = max(coalesce(rtt_p95, excluded.rtt_p95), coalesce(excluded.rtt_p95, rtt_p95)),
			rtt_p99 = max(coalesce(rtt_p99, excluded.rtt_p99), coalesce(excluded.rtt_p99, rtt_p99)),
			sent = sent + excluded.sent,
			recv = recv + excluded.recv`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range stats {
		if _, err := stmt.ExecContext(ctx, m.TargetID, m.TS, m.Sent, m.Recv,
			m.Min, m.Avg, m.P50, m.P95, m.P99, m.Max, m.Jitter); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// weighted merges an existing and an incoming average, weighting each by its
// reply count and falling back to whichever side is non-null.
func weighted(col string) string {
	return fmt.Sprintf(`CASE
		WHEN %[1]s IS NULL THEN excluded.%[1]s
		WHEN excluded.%[1]s IS NULL THEN %[1]s
		ELSE (%[1]s * recv + excluded.%[1]s * excluded.recv) / (recv + excluded.recv)
	END`, col)
}

// Series returns target's rollups in [from, to) downsampled into buckets of
// step seconds (a multiple of 60). Averages are weighted by replies; p95/p99
// and max take the worst minute in the bucket.
func (s *Store) Series(ctx context.Context, targetID, from, to, step int64) ([]model.Bucket, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ts / ?1) * ?1 AS b,
			SUM(sent), SUM(recv),
			MIN(rtt_min),
			SUM(rtt_avg * recv) / NULLIF(SUM(CASE WHEN rtt_avg IS NULL THEN 0 ELSE recv END), 0),
			SUM(rtt_p50 * recv) / NULLIF(SUM(CASE WHEN rtt_p50 IS NULL THEN 0 ELSE recv END), 0),
			MAX(rtt_p95), MAX(rtt_p99), MAX(rtt_max),
			SUM(jitter * recv) / NULLIF(SUM(CASE WHEN jitter IS NULL THEN 0 ELSE recv END), 0)
		FROM probe_minute
		WHERE target_id = ?2 AND ts >= ?3 AND ts < ?4
		GROUP BY b ORDER BY b`, step, targetID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Bucket
	for rows.Next() {
		var b model.Bucket
		var mn, avg, p50, p95, p99, mx, jit sql.NullFloat64
		if err := rows.Scan(&b.TS, &b.Sent, &b.Recv, &mn, &avg, &p50, &p95, &p99, &mx, &jit); err != nil {
			return nil, err
		}
		b.Min, b.Avg, b.P50, b.P95, b.P99, b.Max, b.Jitter = ptr(mn), ptr(avg), ptr(p50), ptr(p95), ptr(p99), ptr(mx), ptr(jit)
		out = append(out, b)
	}
	return out, rows.Err()
}

func ptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

// Prune deletes rollups and finished events older than before (unix
// seconds), and delivered alerts older than a week, then returns freed pages
// to the filesystem. It reports how many rollup rows were removed.
func (s *Store) Prune(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM probe_minute WHERE ts < ?`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE ended_at IS NOT NULL AND ended_at < ?`, before); err != nil {
		return n, err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM alert_outbox WHERE status != 'pending' AND created_at < ?`, before-7*86400); err != nil {
		return n, err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return n, err
	}
	return n, nil
}

// SizeBytes returns the on-disk size of the database, including the
// write-ahead log (recent writes live there until a checkpoint).
func (s *Store) SizeBytes(ctx context.Context) (int64, error) {
	var total int64
	for _, p := range []string{s.path, s.path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return total, err
		}
		total += fi.Size()
	}
	return total, nil
}
