package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// WriteHTTPSamples stores web-request phase timings.
func (s *Store) WriteHTTPSamples(ctx context.Context, samples []model.HTTPSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range samples {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO http_sample (target_id, ts, ok, status, dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms, error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			h.TargetID, h.TS, h.OK, h.Status, h.DNSMs, h.ConnectMs, h.TLSMs, h.TTFBMs, h.TotalMs, h.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HTTPSamples returns target's samples in [from, to], oldest first.
func (s *Store) HTTPSamples(ctx context.Context, targetID, from, to int64) ([]model.HTTPSample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_id, ts, ok, status, dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms, error
		FROM http_sample WHERE target_id = ? AND ts >= ? AND ts <= ? ORDER BY ts`, targetID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.HTTPSample{}
	for rows.Next() {
		var h model.HTTPSample
		var dns, conn, tls, ttfb, total sql.NullFloat64
		if err := rows.Scan(&h.TargetID, &h.TS, &h.OK, &h.Status, &dns, &conn, &tls, &ttfb, &total, &h.Error); err != nil {
			return nil, err
		}
		h.DNSMs, h.ConnectMs, h.TLSMs, h.TTFBMs, h.TotalMs = ptr(dns), ptr(conn), ptr(tls), ptr(ttfb), ptr(total)
		out = append(out, h)
	}
	return out, rows.Err()
}

// LatestPublicIP returns the most recent record (zero value if none).
func (s *Store) LatestPublicIP(ctx context.Context) (model.PublicIP, error) {
	var p model.PublicIP
	err := s.db.QueryRowContext(ctx, `SELECT ts, ipv4, ipv6 FROM public_ip ORDER BY ts DESC LIMIT 1`).Scan(&p.TS, &p.IPv4, &p.IPv6)
	if err == sql.ErrNoRows {
		return model.PublicIP{}, nil
	}
	return p, err
}

// RecordPublicIP stores a new public address (call only when it changed).
func (s *Store) RecordPublicIP(ctx context.Context, p model.PublicIP) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO public_ip (ts, ipv4, ipv6) VALUES (?, ?, ?)`, p.TS, p.IPv4, p.IPv6)
	return err
}

// PublicIPs returns the address history in [from, to], newest first.
func (s *Store) PublicIPs(ctx context.Context, from, to int64) ([]model.PublicIP, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, ipv4, ipv6 FROM public_ip WHERE ts >= ? AND ts <= ? ORDER BY ts DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.PublicIP{}
	for rows.Next() {
		var p model.PublicIP
		if err := rows.Scan(&p.TS, &p.IPv4, &p.IPv6); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertTrace stores a traceroute and sets its ID.
func (s *Store) InsertTrace(ctx context.Context, t *model.Trace) error {
	hops, err := json.Marshal(t.Hops)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `
		INSERT INTO traces (ts, reason, event_id, dst, hops) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		t.TS, t.Reason, t.EventID, t.Dst, string(hops)).Scan(&t.ID)
}

// Traces returns traceroutes for an event (eventID > 0) or else those in
// [from, to], newest first, at most limit.
func (s *Store) Traces(ctx context.Context, eventID, from, to int64, limit int) ([]model.Trace, error) {
	q := `SELECT id, ts, reason, event_id, dst, hops FROM traces WHERE ts >= ? AND ts <= ? ORDER BY ts DESC LIMIT ?`
	args := []any{from, to, limit}
	if eventID > 0 {
		q = `SELECT id, ts, reason, event_id, dst, hops FROM traces WHERE event_id = ? ORDER BY ts DESC LIMIT ?`
		args = []any{eventID, limit}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Trace{}
	for rows.Next() {
		var t model.Trace
		var ev sql.NullInt64
		var hops string
		if err := rows.Scan(&t.ID, &t.TS, &t.Reason, &ev, &t.Dst, &hops); err != nil {
			return nil, err
		}
		if ev.Valid {
			t.EventID = &ev.Int64
		}
		if err := json.Unmarshal([]byte(hops), &t.Hops); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
