package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

func TestPickStep(t *testing.T) {
	tests := []struct{ span, req, want int64 }{
		{3600, 0, 60},          // 1 h -> 60 one-minute points
		{86400, 0, 120},        // 24 h -> 87 s rounded up to 2 min
		{30 * 86400, 0, 2640},  // 30 d -> 2592 s rounded up to 44 min
		{3600, 300, 300},       // explicit step respected
		{3600, 90, 120},        // rounded up to whole minutes
		{86400 * 30, 60, 2640}, // never more than maxPoints buckets
	}
	for _, tt := range tests {
		if got := pickStep(tt.span, tt.req); got != tt.want {
			t.Errorf("pickStep(%d, %d) = %d, want %d", tt.span, tt.req, got, tt.want)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	avg := 12.3456
	if err := st.WriteMinutes(ctx, []model.MinuteStat{
		{TargetID: 1, TS: 600, Sent: 60, Recv: 57, Avg: &avg},
		{TargetID: 1, TS: 660, Sent: 60, Recv: 0},
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{st: st, log: slog.Default(), retention: 30, now: func() time.Time { return time.Unix(1000, 0) }}

	rec := httptest.NewRecorder()
	s.metrics(rec, httptest.NewRequest("GET", "/api/metrics?target=1&from=0&to=1000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Step int64      `json:"step"`
		TS   []int64    `json:"ts"`
		Loss []float64  `json:"loss_pct"`
		Avg  []*float64 `json:"avg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Step != 60 || len(out.TS) != 2 || out.Loss[0] != 5 || out.Loss[1] != 100 {
		t.Fatalf("unexpected response %+v", out)
	}
	if out.Avg[0] == nil || *out.Avg[0] != 12.346 || out.Avg[1] != nil {
		t.Fatalf("avg = %v, want [12.346 null]", out.Avg)
	}

	for _, q := range []string{"", "target=x", "target=1&from=5&to=5", "target=1&step=-1"} {
		rec := httptest.NewRecorder()
		s.metrics(rec, httptest.NewRequest("GET", "/api/metrics?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status %d, want 400", q, rec.Code)
		}
	}
}

func TestUptime(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{st: st, log: slog.Default()}

	if u, err := s.uptime(ctx, 0, 1000, 1000); err != nil || u.UptimePct != nil {
		t.Fatalf("no data yet should give null uptime: %+v %v", u, err)
	}
	st.WriteMinutes(ctx, []model.MinuteStat{{TargetID: 1, TS: 1000, Sent: 1, Recv: 1}})
	end := int64(1100)
	for _, ev := range []*model.Event{
		{Kind: model.EventOutage, Scope: model.FamilyV4, StartedAt: 900, EndedAt: &end}, // clipped to start at 1000
		{Kind: model.EventOutage, Scope: model.FamilyV6, StartedAt: 1200},               // IPv6 does not count
		{Kind: model.EventDegraded, Scope: model.FamilyV4, StartedAt: 1300},             // not an outage
		{Kind: model.EventOutage, Scope: model.FamilyV4, StartedAt: 1900},               // open: runs to now
	} {
		if err := st.InsertEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.uptime(ctx, 0, 5000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	// Monitored 1000..2000; down 1000..1100 and 1900..2000.
	if u.MonitoredS != 1000 || u.DowntimeS != 200 || u.Outages != 2 || *u.UptimePct != 80 || u.LongestS != 200 {
		t.Fatalf("uptime = %+v (pct %v)", u, *u.UptimePct)
	}
	if *u.LastOutage != 1900 {
		t.Fatalf("last outage = %d", *u.LastOutage)
	}
}

func TestUptimeExcludesScheduledReboots(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{st: st, log: slog.Default()}
	st.WriteMinutes(ctx, []model.MinuteStat{{TargetID: 1, TS: 1000, Sent: 1, Recv: 1}})
	e1, e2 := int64(1200), int64(1700)
	win := func(start, end int64) *model.Planned { return &model.Planned{Start: start, End: end, Label: "x"} }
	for _, ev := range []*model.Event{
		{Kind: model.EventOutage, Scope: model.FamilyV4, StartedAt: 1100, EndedAt: &e1, Planned: win(1050, 1650)}, // all planned
		{Kind: model.EventOutage, Scope: model.FamilyV4, StartedAt: 1400, EndedAt: &e2, Planned: win(1350, 1500)}, // 100 s planned, 200 s overrun
	} {
		if err := st.InsertEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.uptime(ctx, 0, 5000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	// Monitored 1000 s, 200 s planned; 200 s of the remaining 800 s down.
	if u.PlannedReboots != 2 || u.PlannedS != 200 || u.Outages != 1 || u.DowntimeS != 200 || *u.UptimePct != 75 || u.LongestS != 200 {
		t.Fatalf("uptime = %+v (pct %v)", u, *u.UptimePct)
	}
}
