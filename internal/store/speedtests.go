package store

import (
	"context"
	"database/sql"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// InsertSpeedTest stores a result and sets its ID.
func (s *Store) InsertSpeedTest(ctx context.Context, t *model.SpeedTest) error {
	return s.db.QueryRowContext(ctx, `
		INSERT INTO speedtests (ts, trigger, down_mbps, up_mbps, idle_ms, loaded_down_ms, loaded_up_ms,
			grade, bytes_down, bytes_up, server, duration_s, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		t.TS, t.Trigger, t.DownMbps, t.UpMbps, t.IdleMs, t.LoadedDownMs, t.LoadedUpMs,
		t.Grade, t.BytesDown, t.BytesUp, t.Server, t.DurationS, t.Error).Scan(&t.ID)
}

// SpeedTests returns results in [from, to], newest first.
func (s *Store) SpeedTests(ctx context.Context, from, to int64) ([]model.SpeedTest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, trigger, down_mbps, up_mbps, idle_ms, loaded_down_ms, loaded_up_ms,
			grade, bytes_down, bytes_up, server, duration_s, error
		FROM speedtests WHERE ts >= ? AND ts <= ? ORDER BY ts DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SpeedTest{}
	for rows.Next() {
		var t model.SpeedTest
		var down, up, idle, ld, lu sql.NullFloat64
		if err := rows.Scan(&t.ID, &t.TS, &t.Trigger, &down, &up, &idle, &ld, &lu,
			&t.Grade, &t.BytesDown, &t.BytesUp, &t.Server, &t.DurationS, &t.Error); err != nil {
			return nil, err
		}
		t.DownMbps, t.UpMbps, t.IdleMs, t.LoadedDownMs, t.LoadedUpMs = ptr(down), ptr(up), ptr(idle), ptr(ld), ptr(lu)
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestSpeedTest returns the newest result, or nil if there is none.
func (s *Store) LatestSpeedTest(ctx context.Context) (*model.SpeedTest, error) {
	ts, err := s.SpeedTests(ctx, 0, 1<<62)
	if err != nil || len(ts) == 0 {
		return nil, err
	}
	return &ts[0], nil
}
