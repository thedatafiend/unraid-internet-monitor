package store

import (
	"context"
	"database/sql"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// InsertBlips stores brief drops in one transaction.
func (s *Store) InsertBlips(ctx context.Context, bs []model.Blip) error {
	if len(bs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range bs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO blips (ts, seconds, class) VALUES (?, ?, ?)`, b.TS, b.Seconds, b.Class); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BlipCount is the number of brief drops of one class in a bucket.
type BlipCount struct {
	TS      int64
	Class   string
	Count   int
	Seconds int
}

// BlipCounts returns brief drops in [from, to) counted per class in buckets
// of step seconds, oldest first.
func (s *Store) BlipCounts(ctx context.Context, from, to, step int64) ([]BlipCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ts / ?1) * ?1 AS b, class, COUNT(*), SUM(seconds) FROM blips
		WHERE ts >= ?2 AND ts < ?3 GROUP BY b, class ORDER BY b, class`, step, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlipCount
	for rows.Next() {
		var c BlipCount
		if err := rows.Scan(&c.TS, &c.Class, &c.Count, &c.Seconds); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InternetStats is the IPv4 internet targets' totals over a window.
type InternetStats struct {
	Sent, Recv int64
	Avg        *float64 // ms, weighted by replies
}

// InternetStats sums the IPv4 internet targets' rollups in [from, to),
// leaving out minutes that overlap an outage, so loss and latency describe
// the connection while it was up. Outages still open end at now.
func (s *Store) InternetStats(ctx context.Context, from, to, now int64) (InternetStats, error) {
	var st InternetStats
	var sent, recv sql.NullInt64
	var avg sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(p.sent), SUM(p.recv),
			SUM(p.rtt_avg * p.recv) / NULLIF(SUM(CASE WHEN p.rtt_avg IS NULL THEN 0 ELSE p.recv END), 0)
		FROM probe_minute p JOIN targets t ON t.id = p.target_id
		WHERE t.role = 'internet' AND t.family = 'ip4' AND p.ts >= ?1 AND p.ts < ?2
			AND NOT EXISTS (SELECT 1 FROM events e
				WHERE e.kind = 'outage' AND e.scope = 'ip4'
					AND e.started_at < p.ts + 60 AND coalesce(e.ended_at, ?3) > p.ts)`,
		from, to, now).Scan(&sent, &recv, &avg)
	st.Sent, st.Recv, st.Avg = sent.Int64, recv.Int64, ptr(avg)
	return st, err
}

// Markers returns every marker, oldest first.
func (s *Store) Markers(ctx context.Context) ([]model.Marker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, label FROM markers ORDER BY ts, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Marker{}
	for rows.Next() {
		var m model.Marker
		if err := rows.Scan(&m.ID, &m.TS, &m.Label); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InsertMarker stores m and sets its ID.
func (s *Store) InsertMarker(ctx context.Context, m *model.Marker) error {
	return s.db.QueryRowContext(ctx, `INSERT INTO markers (ts, label) VALUES (?, ?) RETURNING id`, m.TS, m.Label).Scan(&m.ID)
}

// DeleteMarker removes a marker. It reports whether one existed.
func (s *Store) DeleteMarker(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM markers WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
