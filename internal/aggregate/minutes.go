package aggregate

import (
	"slices"
	"sync"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

type minuteKey struct {
	target int64
	minute int64
}

// Minutes accumulates samples into per-target, per-minute buckets.
// Samples may arrive slightly out of order (a slow tick can finish after the
// next one); a minute is only finalized once Grace seconds have passed after
// it ended.
type Minutes struct {
	Grace int64

	mu      sync.Mutex
	buckets map[minuteKey][]Point
}

// NewMinutes returns an accumulator that waits grace seconds before
// finalizing a minute.
func NewMinutes(grace int64) *Minutes {
	return &Minutes{Grace: grace, buckets: make(map[minuteKey][]Point)}
}

// Add records one sample for target.
func (m *Minutes) Add(target int64, p Point) {
	k := minuteKey{target, p.TS - p.TS%60}
	m.mu.Lock()
	m.buckets[k] = append(m.buckets[k], p)
	m.mu.Unlock()
}

// Collect removes and returns rollups for every minute that ended at least
// Grace seconds before now. Pass force to flush everything, including the
// current partial minute (used at shutdown).
func (m *Minutes) Collect(now int64, force bool) []model.MinuteStat {
	m.mu.Lock()
	var ready []minuteKey
	var pts [][]Point
	for k, v := range m.buckets {
		if force || now >= k.minute+60+m.Grace {
			ready = append(ready, k)
			pts = append(pts, v)
			delete(m.buckets, k)
		}
	}
	m.mu.Unlock()

	out := make([]model.MinuteStat, 0, len(ready))
	for i, k := range ready {
		out = append(out, rollup(k, pts[i]))
	}
	slices.SortFunc(out, func(a, b model.MinuteStat) int {
		if a.TS != b.TS {
			return int(a.TS - b.TS)
		}
		return int(a.TargetID - b.TargetID)
	})
	return out
}

func rollup(k minuteKey, pts []Point) model.MinuteStat {
	slices.SortFunc(pts, func(a, b Point) int { return int(a.TS - b.TS) })
	pts = slices.CompactFunc(pts, func(a, b Point) bool { return a.TS == b.TS })
	rtts := make([]float64, 0, len(pts))
	for _, p := range pts {
		if p.OK {
			rtts = append(rtts, float64(p.RTT))
		}
	}
	s := Summarize(len(pts), rtts)
	st := model.MinuteStat{TargetID: k.target, TS: k.minute, Sent: s.Sent, Recv: s.Recv}
	if s.Recv > 0 {
		st.Min, st.Avg, st.P50, st.P95, st.P99, st.Max = &s.Min, &s.Avg, &s.P50, &s.P95, &s.P99, &s.Max
		if s.Recv > 1 {
			st.Jitter = &s.Jitter
		}
	}
	return st
}
