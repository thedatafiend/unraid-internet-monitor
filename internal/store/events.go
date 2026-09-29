package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// InsertEvent stores ev and sets its ID.
func (s *Store) InsertEvent(ctx context.Context, ev *model.Event) error {
	details, err := marshalDetails(ev.Details)
	if err != nil {
		return err
	}
	planned, err := marshalPlanned(ev.Planned)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `
		INSERT INTO events (kind, scope, class, started_at, ended_at, details, planned)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		ev.Kind, ev.Scope, ev.Class, ev.StartedAt, ev.EndedAt, details, planned).Scan(&ev.ID)
}

// UpdateEvent rewrites an event's class, end and details.
func (s *Store) UpdateEvent(ctx context.Context, ev model.Event) error {
	details, err := marshalDetails(ev.Details)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET class = ?, ended_at = ?, details = ? WHERE id = ?`,
		ev.Class, ev.EndedAt, details, ev.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("event %d not found", ev.ID)
	}
	return nil
}

// Events returns events of kind (all kinds when empty) that overlap
// [from, to], oldest first. Open events overlap everything after their start.
func (s *Store) Events(ctx context.Context, from, to int64, kind string) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, scope, class, started_at, ended_at, details, planned FROM events
		WHERE started_at <= ?2 AND (ended_at IS NULL OR ended_at >= ?1) AND (?3 = '' OR kind = ?3)
		ORDER BY started_at, id`, from, to, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Event{}
	for rows.Next() {
		var ev model.Event
		var ended sql.NullInt64
		var details string
		var planned sql.NullString
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.Scope, &ev.Class, &ev.StartedAt, &ended, &details, &planned); err != nil {
			return nil, err
		}
		if planned.Valid {
			if err := json.Unmarshal([]byte(planned.String), &ev.Planned); err != nil {
				return nil, fmt.Errorf("event %d planned: %w", ev.ID, err)
			}
		}
		if ended.Valid {
			ev.EndedAt = &ended.Int64
		}
		if err := json.Unmarshal([]byte(details), &ev.Details); err != nil {
			return nil, fmt.Errorf("event %d details: %w", ev.ID, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CloseDanglingEvents ends events left open by a crash or shutdown. The end
// is the last stored minute (the best evidence of when monitoring stopped),
// and details get "interrupted": true.
func (s *Store) CloseDanglingEvents(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE events SET
			ended_at = max(started_at, coalesce((SELECT max(ts) + 60 FROM probe_minute), started_at)),
			details  = json_set(details, '$.interrupted', json('true'))
		WHERE ended_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FirstDataTS returns the oldest stored minute, or 0 when there is none.
func (s *Store) FirstDataTS(ctx context.Context) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT min(ts) FROM probe_minute`).Scan(&ts)
	return ts.Int64, err
}

func marshalDetails(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

func marshalPlanned(p *model.Planned) (any, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// EnqueueAlert adds an alert to the outbox, due immediately.
func (s *Store) EnqueueAlert(ctx context.Context, kind string, payload []byte, now int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO alert_outbox (created_at, kind, payload, next_attempt_at)
		VALUES (?, ?, ?, ?) RETURNING id`, now, kind, string(payload), now).Scan(&id)
	return id, err
}

// DueAlerts returns up to limit pending alerts due at now, oldest first.
func (s *Store) DueAlerts(ctx context.Context, now int64, limit int) ([]model.OutboxItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, kind, payload, attempts FROM alert_outbox
		WHERE status = 'pending' AND next_attempt_at <= ?
		ORDER BY id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OutboxItem
	for rows.Next() {
		var it model.OutboxItem
		var payload string
		if err := rows.Scan(&it.ID, &it.CreatedAt, &it.Kind, &payload, &it.Attempts); err != nil {
			return nil, err
		}
		it.Payload = []byte(payload)
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkAlertSent records a successful delivery.
func (s *Store) MarkAlertSent(ctx context.Context, id, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE alert_outbox SET status = 'sent', sent_at = ?, error = NULL WHERE id = ?`, now, id)
	return err
}

// RetryAlert records a failed attempt and when to try again.
func (s *Store) RetryAlert(ctx context.Context, id int64, attempts int, next int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE alert_outbox SET attempts = ?, next_attempt_at = ?, error = ? WHERE id = ?`,
		attempts, next, errMsg, id)
	return err
}

// FailAlert gives up on an alert.
func (s *Store) FailAlert(ctx context.Context, id int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE alert_outbox SET status = 'failed', error = ? WHERE id = ?`, errMsg, id)
	return err
}

// SupersedeAlert cancels a still-pending alert. It reports whether the
// alert was cancelled (false means it was already delivered or given up).
func (s *Store) SupersedeAlert(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alert_outbox SET status = 'superseded' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
