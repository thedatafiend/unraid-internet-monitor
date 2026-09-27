package aggregate

import "sync"

// Point is one probe result at one-second resolution.
type Point struct {
	TS  int64   // unix seconds
	RTT float32 // milliseconds, valid when OK
	OK  bool
}

// Ring keeps the most recent per-second points for one target. Points are
// slotted by timestamp, so out-of-order inserts are fine.
type Ring struct {
	mu    sync.RWMutex
	slots []Point
}

// NewRing returns a ring holding the last seconds seconds.
func NewRing(seconds int) *Ring {
	return &Ring{slots: make([]Point, seconds)}
}

func (r *Ring) slot(ts int64) int { return int(ts % int64(len(r.slots))) }

// Put stores p, overwriting whatever was in its slot.
func (r *Ring) Put(p Point) {
	r.mu.Lock()
	r.slots[r.slot(p.TS)] = p
	r.mu.Unlock()
}

// Get returns the point for ts, if present.
func (r *Ring) Get(ts int64) (Point, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p := r.slots[r.slot(ts)]
	return p, p.TS == ts && ts != 0
}

// Range returns the stored points with from <= TS <= to, oldest first.
func (r *Ring) Range(from, to int64) []Point {
	if to-from >= int64(len(r.slots)) {
		from = to - int64(len(r.slots)) + 1
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Point, 0, max(0, to-from+1))
	for ts := from; ts <= to; ts++ {
		if p := r.slots[r.slot(ts)]; p.TS == ts && ts != 0 {
			out = append(out, p)
		}
	}
	return out
}

// Summarize returns statistics over the points with from <= TS <= to.
func (r *Ring) Summarize(from, to int64) Summary {
	pts := r.Range(from, to)
	rtts := make([]float64, 0, len(pts))
	for _, p := range pts {
		if p.OK {
			rtts = append(rtts, float64(p.RTT))
		}
	}
	return Summarize(len(pts), rtts)
}
