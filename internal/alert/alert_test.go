package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

func TestDiscordSend(t *testing.T) {
	var got map[string]any
	status := http.StatusNoContent
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	d := NewDiscord(srv.URL + "/api/webhooks/123/secret-token")
	ctx := context.Background()

	msg := Message{Title: "t", Description: "d", Color: colorRed, Fields: []Field{{Name: "n", Value: "v", Inline: true}}, Time: 1700000000}
	if err := d.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	embed := got["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "t" || embed["timestamp"] != "2023-11-14T22:13:20Z" || embed["fields"].([]any)[0].(map[string]any)["inline"] != true {
		t.Fatalf("payload = %v", got)
	}
	if got["allowed_mentions"] == nil {
		t.Fatal("mentions must be disabled")
	}

	status, body = http.StatusTooManyRequests, `{"retry_after": 1.5}`
	var ra *RetryAfterError
	if err := d.Send(ctx, msg); !errors.As(err, &ra) || ra.After != 1500*time.Millisecond {
		t.Fatalf("429: %v", err)
	}

	status, body = http.StatusNotFound, `{"message": "Unknown Webhook"}`
	var perm *PermanentError
	if err := d.Send(ctx, msg); !errors.As(err, &perm) {
		t.Fatalf("404: %v", err)
	}

	status = http.StatusBadGateway
	if err := d.Send(ctx, msg); err == nil || errors.As(err, &perm) || errors.As(err, &ra) {
		t.Fatalf("502 should be retryable: %v", err)
	}

	srv.Close()
	err := d.Send(ctx, msg)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("network error must not leak the webhook token: %v", err)
	}
}

// fakeSender records messages and fails while down is true.
type fakeSender struct {
	down bool
	sent []Message
}

func (f *fakeSender) Send(_ context.Context, m Message) error {
	if f.down {
		return errors.New("network is unreachable")
	}
	f.sent = append(f.sent, m)
	return nil
}

func newTestManager(t *testing.T) (*Manager, *fakeSender, *time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	snd := &fakeSender{}
	m := NewManager(Config{MinOutage: 30 * time.Second, Coalesce: 5 * time.Minute}, snd, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Unix(10_000, 0)
	m.now = func() time.Time { return now }
	return m, snd, &now
}

func outage(id, start int64) model.Event {
	return model.Event{ID: id, Kind: model.EventOutage, Scope: "ip4", Class: model.ClassISPEdge, StartedAt: start}
}

func closed(ev model.Event, end int64) model.Event {
	ev.EndedAt = &end
	ev.Details = map[string]any{"gateway_down_pct": 0.0, "isp_down_pct": 100.0}
	return ev
}

func titles(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Title)
	}
	return out
}

func TestShortOutageIsNotAlerted(t *testing.T) {
	m, snd, now := newTestManager(t)
	ev := outage(1, now.Unix())
	for s := int64(0); s < 20; s++ {
		m.EventOngoing(ev, ev.StartedAt+s)
	}
	m.EventClosed(closed(ev, ev.StartedAt+20), ev.StartedAt+23)
	m.Deliver(context.Background())
	if len(snd.sent) != 0 {
		t.Fatalf("sent %v", titles(snd.sent))
	}
}

func TestFullOutageSendsOnlyRecovery(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	start := now.Unix()
	ev := outage(1, start)

	// The internet is down: the "started" alert is queued but cannot be sent.
	snd.down = true
	for s := int64(0); s <= 40; s++ {
		m.EventOngoing(ev, start+s)
	}
	*now = now.Add(40 * time.Second)
	m.Deliver(ctx)

	// Recovery: the stale "started" alert is superseded; only the recovery goes out.
	snd.down = false
	m.EventClosed(closed(ev, start+300), start+303)
	*now = now.Add(10 * time.Minute)
	m.Deliver(ctx)
	if got := titles(snd.sent); len(got) != 1 || got[0] != "🟢 Internet restored" {
		t.Fatalf("sent %v", got)
	}
	if !strings.Contains(snd.sent[0].Description, "5m") {
		t.Fatalf("description = %q", snd.sent[0].Description)
	}
}

func TestPartialOutageStartedIsDelivered(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	start := now.Unix()
	ev := outage(1, start)
	for s := int64(0); s <= 30; s++ {
		m.EventOngoing(ev, start+s)
	}
	*now = time.Unix(start+31, 0)
	m.Deliver(ctx) // Discord reachable (e.g. only some paths are broken)
	m.EventClosed(closed(ev, start+60), start+63)
	*now = time.Unix(start+64, 0)
	m.Deliver(ctx)
	if got := titles(snd.sent); len(got) != 2 || got[0] != "🔴 Internet outage" || got[1] != "🟢 Internet restored" {
		t.Fatalf("sent %v", got)
	}
}

func TestFlappingIsCoalesced(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	t0 := now.Unix()

	// First outage (40 s) alerts normally.
	first := outage(1, t0)
	for s := int64(0); s <= 40; s++ {
		m.EventOngoing(first, t0+s)
		if s == 30 {
			*now = time.Unix(t0+30, 0)
			m.Deliver(ctx) // the sender loop runs mid-outage and gets the "started" alert out
		}
	}
	m.EventClosed(closed(first, t0+40), t0+43)
	// Two more outages within the 5-minute window go into a digest.
	for i, start := range []int64{t0 + 100, t0 + 200} {
		ev := outage(int64(2+i), start)
		for s := int64(0); s <= 35; s++ {
			m.EventOngoing(ev, start+s)
		}
		m.EventClosed(closed(ev, start+35), start+38)
	}
	m.Tick(t0 + 300) // too early: last event at t0+238
	*now = time.Unix(t0+300, 0)
	m.Deliver(ctx)
	if got := titles(snd.sent); len(got) != 2 {
		t.Fatalf("before digest: %v", got)
	}
	m.Tick(t0 + 238 + 300)
	*now = time.Unix(t0+238+300, 0)
	m.Deliver(ctx)
	got := titles(snd.sent)
	if len(got) != 3 || got[2] != "🔴 2 more outages" {
		t.Fatalf("sent %v", got)
	}
	if !strings.Contains(snd.sent[2].Description, "1m10s") {
		t.Fatalf("digest = %q", snd.sent[2].Description)
	}
}

func TestDegradedAlerts(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	start := now.Unix()
	ev := model.Event{ID: 7, Kind: model.EventDegraded, Scope: "ip4", StartedAt: start,
		Details: map[string]any{"loss_pct": 3.3, "p95_ms": 45.0}}
	m.EventOpened(ev, start+120)
	end := start + 600
	ev.EndedAt = &end
	ev.Details = map[string]any{"loss_pct": 3.3, "p95_ms": 45.0, "peak_loss_pct": 5.1, "peak_p95_ms": 180.0}
	m.EventClosed(ev, end+60)
	*now = time.Unix(end+60, 0)
	m.Deliver(ctx)
	got := titles(snd.sent)
	if len(got) != 2 || got[0] != "🟠 Connection degraded" || got[1] != "🟢 Connection back to normal" {
		t.Fatalf("sent %v", got)
	}
	if snd.sent[1].Fields[0].Value != "5.1%" {
		t.Fatalf("fields = %+v", snd.sent[1].Fields)
	}
}

func TestDeliveryStopsAtFirstFailureAndExpires(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	m.Test(now.Unix())
	m.Test(now.Unix())
	snd.down = true
	m.Deliver(ctx)
	due, _ := m.ob.DueAlerts(ctx, now.Unix()+3600, 10)
	if len(due) != 2 || due[0].Attempts != 1 || due[1].Attempts != 0 {
		t.Fatalf("only the first alert should have been tried: %+v", due)
	}
	// A day later the alerts are stale and dropped instead of sent.
	snd.down = false
	*now = now.Add(25 * time.Hour)
	m.Deliver(ctx)
	if len(snd.sent) != 0 {
		t.Fatalf("stale alerts were sent: %v", titles(snd.sent))
	}
}

func TestDisabledManager(t *testing.T) {
	m := NewManager(Config{}, nil, nil, slog.Default())
	if m.Enabled() || !errors.Is(m.Test(0), ErrDisabled) {
		t.Fatal("manager without a sender must be disabled")
	}
	m.EventOngoing(outage(1, 0), 1000) // must not touch the nil outbox
	m.EventClosed(closed(outage(1, 0), 999), 1000)
	m.Tick(5000)
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if backoff(50) != maxBackoff {
		t.Error("backoff must be capped")
	}
}

func TestFmtDur(t *testing.T) {
	for sec, want := range map[int64]string{45: "45s", 300: "5m", 312: "5m12s", 3600: "1h0m", 3725: "1h2m5s"} {
		if got := fmtDur(sec); got != want {
			t.Errorf("fmtDur(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestRecoveryMessageIncludesRoute(t *testing.T) {
	end := int64(160)
	ev := model.Event{ID: 1, Kind: model.EventOutage, Class: model.ClassISPEdge, StartedAt: 100, EndedAt: &end,
		Details: map[string]any{"trace_last_hop": "207.204.56.1", "trace_last_ttl": 2}}
	m := outageResolved(ev, 170)
	last := m.Fields[len(m.Fields)-1]
	if last.Name != "Route when it started" || !strings.Contains(last.Value, "hop 2") || !strings.Contains(last.Value, "207.204.56.1") {
		t.Fatalf("fields = %+v", m.Fields)
	}
}

func TestPublicIPChangeAlert(t *testing.T) {
	m, snd, now := newTestManager(t)
	m.PublicIPChanged("203.0.113.1", "203.0.113.99", now.Unix()) // IPChange off in newTestManager
	m.cfg.IPChange = true
	m.PublicIPChanged("203.0.113.1", "203.0.113.99", now.Unix())
	m.Deliver(context.Background())
	if len(snd.sent) != 1 || !strings.Contains(snd.sent[0].Description, "203.0.113.99") {
		t.Fatalf("sent %v", titles(snd.sent))
	}
}

func TestSpeedTestAlert(t *testing.T) {
	m, snd, now := newTestManager(t)
	fv := func(v float64) *float64 { return &v }
	m.SpeedTestResult(model.SpeedTest{TS: now.Unix(), DownMbps: fv(50), UpMbps: fv(5)}, now.Unix()) // thresholds off
	m.cfg.MinDownMbps, m.cfg.MinUpMbps = 100, 10
	m.SpeedTestResult(model.SpeedTest{TS: now.Unix(), DownMbps: fv(900), UpMbps: fv(40)}, now.Unix()) // fine
	m.SpeedTestResult(model.SpeedTest{TS: now.Unix(), DownMbps: fv(50), UpMbps: fv(40), Grade: "C"}, now.Unix())
	m.Deliver(context.Background())
	if len(snd.sent) != 1 || snd.sent[0].Title != "🟠 Slow speed test" || snd.sent[0].Fields[0].Value != "50 Mbps (min 100)" {
		t.Fatalf("sent %+v", snd.sent)
	}
}

func planned(ev model.Event, start, end int64) model.Event {
	ev.Planned = &model.Planned{Start: start, End: end, Label: "daily at 03:00 for 10m"}
	return ev
}

func TestScheduledRebootIsNotAlerted(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	win := now.Unix()
	ev := planned(outage(1, win+20), win, win+600) // a 3-minute reboot inside a 10-minute window
	for s := ev.StartedAt; s <= ev.StartedAt+180; s++ {
		m.EventOngoing(ev, s)
	}
	m.EventClosed(closed(ev, ev.StartedAt+180), ev.StartedAt+183)

	deg := planned(model.Event{ID: 2, Kind: model.EventDegraded, Scope: "ip4", StartedAt: win + 200}, win, win+600)
	m.EventOpened(deg, win+320)
	m.EventClosed(closed(deg, win+400), win+400)
	m.Tick(win + 3600)
	*now = time.Unix(win+3600, 0)
	m.Deliver(ctx)
	if len(snd.sent) != 0 {
		t.Fatalf("sent %v", titles(snd.sent))
	}
}

func TestScheduledRebootOverrunIsAlerted(t *testing.T) {
	m, snd, now := newTestManager(t)
	ctx := context.Background()
	win := now.Unix()
	ev := planned(outage(1, win+60), win, win+600)
	for s := ev.StartedAt; s < win+600+30; s++ {
		m.EventOngoing(ev, s)
	}
	*now = time.Unix(win+629, 0)
	m.Deliver(ctx)
	if len(snd.sent) != 0 {
		t.Fatalf("alerted before MinOutage past the window: %v", titles(snd.sent))
	}
	m.EventOngoing(ev, win+630)
	*now = time.Unix(win+631, 0)
	m.Deliver(ctx)
	if got := titles(snd.sent); len(got) != 1 || got[0] != "🔴 Internet outage" {
		t.Fatalf("sent %v", got)
	}
	if f := snd.sent[0].Fields; len(f) != 2 || f[1].Name != "Scheduled reboot" {
		t.Fatalf("fields = %+v", f)
	}
	m.EventClosed(closed(ev, win+900), win+903)
	*now = time.Unix(win+904, 0)
	m.Deliver(ctx)
	if got := titles(snd.sent); len(got) != 2 || got[1] != "🟢 Internet restored" {
		t.Fatalf("sent %v", got)
	}

	// A slowdown that only shows up after the window closes alerts as usual.
	deg := planned(model.Event{ID: 2, Kind: model.EventDegraded, Scope: "ip4", StartedAt: win + 500}, win, win+600)
	m.EventOpened(deg, win+620)
	*now = time.Unix(win+1000, 0)
	m.Deliver(ctx)
	if got := titles(snd.sent); len(got) != 3 {
		t.Fatalf("sent %v", got)
	}
}
