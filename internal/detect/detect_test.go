package detect

import (
	"testing"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// line describes one tick: gateway, ISP hop and internet targets A, B, C.
// 'u' = reply, 'd' = lost, '-' = target absent.
type line struct{ gw, isp, a, b, c byte }

// all returns n ticks of the same state.
func all(n int, l line) []line {
	out := make([]line, n)
	for i := range out {
		out[i] = l
	}
	return out
}

var (
	up         = line{'u', 'u', 'u', 'u', 'u'}
	downISP    = line{'u', 'd', 'd', 'd', 'd'} // gateway answers, ISP edge doesn't
	downLocal  = line{'d', 'd', 'd', 'd', 'd'}
	downFar    = line{'u', 'u', 'd', 'd', 'd'}
	cDown      = line{'u', 'u', 'u', 'u', 'd'}
	slowOrLost = func(i int) line { // one lost packet every 10 ticks: 1 of 30 packets, 3.3% loss
		if i%10 == 0 {
			return line{'u', 'u', 'd', 'u', 'u'}
		}
		return up
	}
)

func tick(ts int64, l line, rtt float64) Tick {
	t := Tick{TS: ts}
	add := func(id int64, name, role string, c byte) {
		if c == '-' {
			return
		}
		t.Samples = append(t.Samples, Sample{TargetID: id, Name: name, Role: role, Family: model.FamilyV4, OK: c == 'u', RTT: rtt})
	}
	add(1, "Gateway", model.RoleGateway, l.gw)
	add(2, "ISP edge", model.RoleISP, l.isp)
	add(3, "A", model.RoleInternet, l.a)
	add(4, "B", model.RoleInternet, l.b)
	add(5, "C", model.RoleInternet, l.c)
	return t
}

// run feeds lines starting at ts=1000 and returns all transitions.
func run(d *Detector, lines []line, rtt float64) []Transition {
	var out []Transition
	for i, l := range lines {
		ts := int64(1000 + i)
		d.Add(tick(ts, l, rtt))
		out = append(out, d.Advance(ts)...)
	}
	return out
}

func concat(parts ...[]line) []line {
	var out []line
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestShortBlipIsNotAnOutage(t *testing.T) {
	d := New(DefaultConfig())
	tr := run(d, concat(all(10, up), all(2, downFar), all(10, up)), 10)
	if len(tr) != 0 {
		t.Fatalf("unexpected transitions: %+v", tr)
	}
	if s := d.Snapshot(); s.State != StateOnline || s.Since != 1000 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestOutageLifecycleAndClassification(t *testing.T) {
	tests := []struct {
		name  string
		down  line
		class string
	}{
		{"isp edge", downISP, model.ClassISPEdge},
		{"local", downLocal, model.ClassLocal},
		{"upstream", downFar, model.ClassUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(DefaultConfig())
			// ticks 1000-1004 up, 1005-1014 down, 1015+ up
			tr := run(d, concat(all(5, up), all(10, tt.down), all(5, up)), 10)
			if len(tr) != 2 || !tr[0].Open || tr[1].Open || tr[0].Event != tr[1].Event {
				t.Fatalf("want open+close of one event, got %+v", tr)
			}
			ev := tr[1].Event
			if ev.Kind != model.EventOutage || ev.Scope != model.FamilyV4 || ev.Class != tt.class {
				t.Fatalf("event = %+v", ev)
			}
			if ev.StartedAt != 1005 || ev.EndedAt == nil || *ev.EndedAt != 1015 || ev.Duration(0) != 10 {
				t.Fatalf("times = %d..%v", ev.StartedAt, ev.EndedAt)
			}
			if ev.Details["down_ticks"] != 10 {
				t.Fatalf("details = %v", ev.Details)
			}
			if s := d.Snapshot(); s.State != StateOnline || s.Since != 1015 || len(s.Open) != 0 {
				t.Fatalf("after recovery snapshot = %+v", s)
			}
		})
	}
}

func TestOutageSnapshotWhileOpen(t *testing.T) {
	d := New(DefaultConfig())
	run(d, concat(all(5, up), all(4, downISP)), 10)
	s := d.Snapshot()
	if s.State != StateOutage || s.Since != 1005 || len(s.Open) != 1 || s.Open[0].EndedAt != nil {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestFlapInsideOutageKeepsOneEvent(t *testing.T) {
	d := New(DefaultConfig())
	// A single good tick (and then two) inside the outage is below RecoverAfter.
	tr := run(d, concat(all(5, up), all(5, downFar), all(1, up), all(5, downFar), all(2, up), all(5, downFar), all(3, up)), 10)
	if len(tr) != 2 {
		t.Fatalf("want one outage, got %d transitions: %+v", len(tr), tr)
	}
	ev := tr[1].Event
	if ev.StartedAt != 1005 || *ev.EndedAt != 1023 || ev.Details["down_ticks"] != 15 {
		t.Fatalf("event = %+v, ended %d", ev, *ev.EndedAt)
	}
}

func TestPartialSingleTarget(t *testing.T) {
	d := New(DefaultConfig())
	tr := run(d, concat(all(5, up), all(15, cDown), all(5, up)), 10)
	if len(tr) != 2 {
		t.Fatalf("got %+v", tr)
	}
	ev := tr[1].Event
	if ev.Kind != model.EventPartial || ev.Scope != "C" || ev.StartedAt != 1005 || *ev.EndedAt != 1020 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Details["target_id"] != int64(5) {
		t.Fatalf("details = %v", ev.Details)
	}
	if d.Snapshot().State != StateOnline {
		t.Fatal("a partial event must not change the overall state")
	}
}

func TestPartialSupersededByOutage(t *testing.T) {
	d := New(DefaultConfig())
	tr := run(d, concat(all(12, cDown), all(5, downFar)), 10)
	// partial opens at tick 10, closes (superseded) when the outage run starts, then the outage opens.
	if len(tr) != 3 || tr[0].Event.Kind != model.EventPartial || tr[1].Open || tr[2].Event.Kind != model.EventOutage {
		t.Fatalf("got %+v", tr)
	}
	if *tr[1].Event.EndedAt != 1012 || tr[1].Event.Details["superseded"] != true {
		t.Fatalf("partial close = %+v", tr[1].Event)
	}
}

func TestDegradedOpensAndClears(t *testing.T) {
	cfg := DefaultConfig()
	d := New(cfg)
	// 1 of 30 packets lost = 3.3% loss: above the 2% threshold.
	var lossy []line
	for i := 0; i < 300; i++ {
		lossy = append(lossy, slowOrLost(i))
	}
	tr := run(d, concat(lossy, all(200, up)), 10)
	if len(tr) != 2 || tr[0].Event.Kind != model.EventDegraded {
		t.Fatalf("got %+v", tr)
	}
	ev := tr[1].Event
	// The window needs 30 ticks before judging; badSince = 1029, open after 120 s.
	if ev.StartedAt != 1029 {
		t.Fatalf("started %d", ev.StartedAt)
	}
	// Lossy ticks come every 10 s; once only three remain in the 60 s window
	// (at 1320: 1270, 1280, 1290 = 3 of 180 packets, 1.7%) the condition clears.
	if *ev.EndedAt != 1320 {
		t.Fatalf("ended %d", *ev.EndedAt)
	}
	if ev.Details["peak_loss_pct"].(float64) < 3 {
		t.Fatalf("details = %v", ev.Details)
	}
}

func TestDegradedByLatency(t *testing.T) {
	d := New(DefaultConfig())
	tr := run(d, all(200, up), 150) // 150 ms > 100 ms p95 threshold
	if len(tr) != 1 || !tr[0].Open || tr[0].Event.Kind != model.EventDegraded {
		t.Fatalf("got %+v", tr)
	}
	if s := d.Snapshot(); s.State != StateDegraded {
		t.Fatalf("state = %s", s.State)
	}
}

func TestDegradedClosedByOutage(t *testing.T) {
	d := New(DefaultConfig())
	tr := run(d, concat(all(200, up), all(5, downFar)), 150)
	if len(tr) != 3 || tr[1].Open || tr[1].Event.Kind != model.EventDegraded || tr[2].Event.Kind != model.EventOutage {
		t.Fatalf("got %+v", tr)
	}
	if *tr[1].Event.EndedAt != tr[2].Event.StartedAt || tr[1].Event.Details["superseded"] != true {
		t.Fatalf("degraded close = %+v", tr[1].Event)
	}
}

func TestOutOfOrderTicks(t *testing.T) {
	d := New(DefaultConfig())
	// Ticks 1001..1003 are down but arrive in reverse; 1000 arrives last.
	for _, ts := range []int64{1003, 1002, 1001} {
		d.Add(tick(ts, downFar, 0))
	}
	d.Add(tick(1000, up, 10))
	tr := d.Advance(1003)
	if len(tr) != 1 || tr[0].Event.StartedAt != 1001 {
		t.Fatalf("got %+v", tr)
	}
	d.Add(tick(999, up, 10)) // older than processed: ignored
	if len(d.pending) != 0 {
		t.Fatal("stale tick was queued")
	}
	if tr := d.Advance(999); len(tr) != 0 {
		t.Fatal("unexpected transition")
	}
}

func TestIPv6PartialWhileIPv4Up(t *testing.T) {
	d := New(DefaultConfig())
	mk := func(ts int64, v6ok bool, v4 line) Tick {
		t := tick(ts, v4, 10)
		for i, name := range []string{"v6a", "v6b"} {
			t.Samples = append(t.Samples, Sample{TargetID: int64(10 + i), Name: name, Role: model.RoleInternet, Family: model.FamilyV6, OK: v6ok, RTT: 10})
		}
		return t
	}
	var tr []Transition
	for ts := int64(1000); ts < 1010; ts++ {
		d.Add(mk(ts, ts < 1002, up))
		tr = append(tr, d.Advance(ts)...)
	}
	if len(tr) != 1 || tr[0].Event.Kind != model.EventPartial || tr[0].Event.Scope != model.FamilyV6 || tr[0].Event.StartedAt != 1002 {
		t.Fatalf("got %+v", tr)
	}
	// IPv4 goes down too: the IPv6 partial closes, the outage opens.
	for ts := int64(1010); ts < 1015; ts++ {
		d.Add(mk(ts, false, downFar))
		tr = append(tr, d.Advance(ts)...)
	}
	if len(tr) != 3 || tr[1].Open || tr[2].Event.Kind != model.EventOutage {
		t.Fatalf("got %+v", tr)
	}
}

func TestRemovedTargetClosesPartial(t *testing.T) {
	d := New(DefaultConfig())
	tr := run(d, all(12, cDown), 10)
	if len(tr) != 1 {
		t.Fatalf("got %+v", tr)
	}
	d.Add(tick(2000, line{'u', 'u', 'u', 'u', '-'}, 10))
	tr = d.Advance(2000)
	if len(tr) != 1 || tr[0].Open || *tr[0].Event.EndedAt != 2000 {
		t.Fatalf("got %+v", tr)
	}
}
