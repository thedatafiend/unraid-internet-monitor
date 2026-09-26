package aggregate

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSummarize(t *testing.T) {
	s := Summarize(5, []float64{10, 20, 10, 30})
	if s.Sent != 5 || s.Recv != 4 || !near(s.LossPct(), 20) {
		t.Fatalf("counts wrong: %+v", s)
	}
	if !near(s.Min, 10) || !near(s.Max, 30) || !near(s.Avg, 17.5) {
		t.Fatalf("min/max/avg wrong: %+v", s)
	}
	// |20-10| + |10-20| + |30-10| = 40 over 3 deltas.
	if !near(s.Jitter, 40.0/3) {
		t.Fatalf("jitter = %v", s.Jitter)
	}
	// Nearest rank on [10 10 20 30]: p50 -> rank 2 -> 10; p95 -> rank 4 -> 30.
	if !near(s.P50, 10) || !near(s.P95, 30) || !near(s.P99, 30) {
		t.Fatalf("percentiles wrong: %+v", s)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(3, nil)
	if s.Recv != 0 || !near(s.LossPct(), 100) {
		t.Fatalf("got %+v", s)
	}
	if Summarize(0, nil).LossPct() != 0 {
		t.Fatal("no probes should mean 0% loss")
	}
}

func TestPercentile60(t *testing.T) {
	var rtts []float64
	for i := 1; i <= 60; i++ {
		rtts = append(rtts, float64(i))
	}
	s := Summarize(60, rtts)
	if s.P50 != 30 || s.P95 != 57 || s.P99 != 60 {
		t.Fatalf("got p50=%v p95=%v p99=%v", s.P50, s.P95, s.P99)
	}
}

func TestMOS(t *testing.T) {
	good := MOS(15, 2, 0)
	if good < 4.3 || good > 4.5 {
		t.Fatalf("good line MOS = %v", good)
	}
	if MOS(80, 10, 0) >= good || MOS(15, 2, 3) >= good || MOS(300, 50, 0) >= MOS(80, 10, 0) {
		t.Fatal("MOS must fall as latency, jitter or loss rise")
	}
	if MOS(15, 2, 100) != 1 {
		t.Fatal("total loss should floor at 1")
	}
}

func TestRing(t *testing.T) {
	r := NewRing(10)
	for ts := int64(100); ts < 115; ts++ {
		r.Put(Point{TS: ts, RTT: float32(ts - 100), OK: ts%5 != 0})
	}
	if _, ok := r.Get(104); ok {
		t.Fatal("ts 104 should have been overwritten")
	}
	if p, ok := r.Get(112); !ok || p.RTT != 12 {
		t.Fatalf("Get(112) = %+v, %v", p, ok)
	}
	pts := r.Range(0, 114) // clamped to the last 10 seconds
	if len(pts) != 10 || pts[0].TS != 105 || pts[9].TS != 114 {
		t.Fatalf("Range = %+v", pts)
	}
	s := r.Summarize(105, 114) // 105 and 110 failed
	if s.Sent != 10 || s.Recv != 8 {
		t.Fatalf("Summarize = %+v", s)
	}
	// Out-of-order insert lands in the right slot.
	r.Put(Point{TS: 116, RTT: 1, OK: true})
	r.Put(Point{TS: 115, RTT: 2, OK: true})
	if p, _ := r.Get(115); p.RTT != 2 {
		t.Fatal("out-of-order insert lost")
	}
}

func TestMinutesCollect(t *testing.T) {
	m := NewMinutes(3)
	// Minute 120..179 for target 1, with ticks added out of order.
	for _, ts := range []int64{121, 120, 123, 122} {
		m.Add(1, Point{TS: ts, RTT: float32(ts - 110), OK: ts != 123})
	}
	m.Add(2, Point{TS: 125, OK: false})
	m.Add(1, Point{TS: 180, RTT: 5, OK: true}) // next minute

	if got := m.Collect(182, false); len(got) != 0 {
		t.Fatalf("collected too early: %+v", got)
	}
	got := m.Collect(183, false)
	if len(got) != 2 {
		t.Fatalf("want 2 finalized rollups, got %+v", got)
	}
	a, b := got[0], got[1]
	if a.TargetID != 1 || a.TS != 120 || a.Sent != 4 || a.Recv != 3 {
		t.Fatalf("target 1 rollup = %+v", a)
	}
	// RTTs in time order: 10, 11, 12 -> avg 11, jitter 1.
	if *a.Avg != 11 || *a.Jitter != 1 || *a.Min != 10 || *a.Max != 12 {
		t.Fatalf("target 1 stats avg=%v jitter=%v", *a.Avg, *a.Jitter)
	}
	if b.TargetID != 2 || b.Recv != 0 || b.Avg != nil || b.Jitter != nil {
		t.Fatalf("target 2 rollup = %+v", b)
	}

	rest := m.Collect(183, true)
	if len(rest) != 1 || rest[0].TS != 180 || rest[0].Jitter != nil {
		t.Fatalf("forced flush = %+v", rest)
	}
}
