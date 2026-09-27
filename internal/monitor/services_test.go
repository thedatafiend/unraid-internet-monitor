package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/alert"
	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/detect"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(config.Defaults(), nil, nil, st, alert.NewManager(alert.Config{}, nil, st, log), log)
}

func TestServiceFailureEvents(t *testing.T) {
	ctx := context.Background()
	e := testEngine(t)
	dns := model.Target{ID: 9, Kind: model.KindDNS, Role: model.RoleDNS, Name: "DNS (system)"}
	fail := errors.New("DNS ServerFailure")
	step := func(ts int64, ok bool) {
		e.serviceResult(ctx, dns, ts, ok, 12*time.Millisecond, map[bool]error{true: nil, false: fail}[ok], nil)
	}
	events := func() []model.Event {
		evs, err := e.st.Events(ctx, 0, 10_000, "")
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}

	step(100, false)
	step(130, false)
	if len(events()) != 0 {
		t.Fatal("two failures must not open an event")
	}
	step(160, false)
	evs := events()
	if len(evs) != 1 || evs[0].Kind != model.EventPartial || evs[0].Scope != "DNS (system)" || evs[0].StartedAt != 160 || evs[0].EndedAt != nil {
		t.Fatalf("after 3 failures: %+v", evs)
	}
	step(190, true)
	if events()[0].EndedAt != nil {
		t.Fatal("one success must not close it")
	}
	step(220, true)
	if ev := events()[0]; ev.EndedAt == nil || *ev.EndedAt != 190 || ev.Details["error"] != "DNS ServerFailure" {
		t.Fatalf("closed event = %+v", ev)
	}

	// During an internet outage the outage covers it: no DNS event.
	e.mu.Lock()
	e.detState.State = detect.StateOutage
	e.mu.Unlock()
	for ts := int64(300); ts < 500; ts += 30 {
		step(ts, false)
	}
	if n := len(events()); n != 1 {
		t.Fatalf("failures during an outage opened events: %d", n)
	}
	if s := e.serviceStatus(dns, 500); s.LastOK || s.LastError != "DNS ServerFailure" || s.LastAt != 480 {
		t.Fatalf("service status = %+v", s)
	}
}

func TestPublicIPChange(t *testing.T) {
	ctx := context.Background()
	e := testEngine(t)
	var ip atomic.Value
	ip.Store("203.0.113.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ip="+ip.Load().(string)+"\n")
	}))
	defer srv.Close()
	e.ipf = &probe.IPFetcher{Sources4: []string{srv.URL}, Timeout: time.Second}

	e.ipCheck(ctx, time.Unix(1000, 0))
	e.ipCheck(ctx, time.Unix(1300, 0)) // unchanged
	if evs, _ := e.st.Events(ctx, 0, 5000, ""); len(evs) != 0 {
		t.Fatalf("first sighting or no change must not create events: %+v", evs)
	}
	ip.Store("203.0.113.99")
	e.ipCheck(ctx, time.Unix(1600, 0))
	evs, _ := e.st.Events(ctx, 0, 5000, "")
	if len(evs) != 1 || evs[0].Kind != model.EventIPChange || evs[0].Details["old_ipv4"] != "203.0.113.1" || evs[0].Details["new_ipv4"] != "203.0.113.99" {
		t.Fatalf("events = %+v", evs)
	}
	if p := e.PublicIP(); p.IPv4 != "203.0.113.99" || p.TS != 1600 {
		t.Fatalf("current = %+v", p)
	}
	// Lookup failing (e.g. during an outage) keeps the last known address.
	srv.Close()
	e.ipCheck(ctx, time.Unix(1900, 0))
	if p := e.PublicIP(); p.IPv4 != "203.0.113.99" {
		t.Fatalf("after failed lookup = %+v", p)
	}
}

func TestWithTraceDoesNotMutateDetectorDetails(t *testing.T) {
	e := testEngine(t)
	orig := map[string]any{"down_ticks": 5}
	ev := model.Event{ID: 3, Kind: model.EventOutage, Details: orig}
	e.svc.traces[3] = map[string]any{"trace_last_hop": "207.204.56.1", "trace_last_ttl": 2}
	got := e.withTrace(ev)
	if got.Details["trace_last_hop"] != "207.204.56.1" || got.Details["down_ticks"] != 5 {
		t.Fatalf("merged = %v", got.Details)
	}
	if _, leaked := orig["trace_last_hop"]; leaked {
		t.Fatal("detector's details map was modified")
	}
	if _, kept := e.svc.traces[3]; kept {
		t.Fatal("summary should be consumed")
	}
}
