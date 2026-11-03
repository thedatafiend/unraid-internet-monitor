package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

func testServer(t *testing.T, now int64) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{st: st, log: slog.Default(), retention: 90, now: func() time.Time { return time.Unix(now, 0) }}, st
}

func do(t *testing.T, h http.HandlerFunc, req *http.Request, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, req)
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: %v", rec.Body, err)
		}
	}
	return rec.Code
}

func TestMarkersEndpoints(t *testing.T) {
	s, _ := testServer(t, 1_000_000)
	post := func(body, ctype string) *http.Request {
		r := httptest.NewRequest("POST", "/api/markers", strings.NewReader(body))
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		return r
	}
	var m model.Marker
	if code := do(t, s.addMarker, post(`{"ts": 999000, "label": "  ONT replaced  "}`, "application/json"), &m); code != http.StatusCreated || m.ID == 0 || m.Label != "ONT replaced" {
		t.Fatalf("add: %d %+v", code, m)
	}
	for _, bad := range []struct {
		body, ctype string
		want        int
	}{
		{`{"ts": 999000, "label": "x"}`, "text/plain", http.StatusUnsupportedMediaType}, // what a cross-site form can send
		{`{"ts": 999000, "label": "x"}`, "", http.StatusUnsupportedMediaType},
		{`{"ts": 999000, "label": " "}`, "application/json", http.StatusBadRequest},
		{`{"ts": 0, "label": "x"}`, "application/json", http.StatusBadRequest},
		{`{"ts": 999000, "label": "` + strings.Repeat("é", 81) + `"}`, "application/json", http.StatusBadRequest},
		{`not json`, "application/json", http.StatusBadRequest},
	} {
		if code := do(t, s.addMarker, post(bad.body, bad.ctype), nil); code != bad.want {
			t.Errorf("%q (%s): status %d, want %d", bad.body, bad.ctype, code, bad.want)
		}
	}

	var list []model.Marker
	if do(t, s.markers, httptest.NewRequest("GET", "/api/markers", nil), &list); len(list) != 1 || list[0] != m {
		t.Fatalf("list = %+v", list)
	}

	// Through the mux, for the {id} path value.
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/markers/{id}", s.deleteMarker)
	del := func(id string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/markers/"+id, nil))
		return rec.Code
	}
	if code := del("1"); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := del("1"); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
	if code := del("x"); code != http.StatusBadRequest {
		t.Fatalf("delete x: %d", code)
	}
}

func TestBlipsEndpoint(t *testing.T) {
	s, st := testServer(t, 10_000)
	st.InsertBlips(context.Background(), []model.Blip{
		{TS: 3600, Seconds: 1, Class: model.ClassISPEdge},
		{TS: 3610, Seconds: 2, Class: model.ClassWAN},
		{TS: 7300, Seconds: 1, Class: model.ClassLocal},
	})
	var out struct {
		Step  int64       `json:"step"`
		TS    []int64     `json:"ts"`
		Count []int       `json:"count"`
		Total blipSummary `json:"total"`
	}
	if code := do(t, s.blips, httptest.NewRequest("GET", "/api/blips?from=0&to=10000&step=3600", nil), &out); code != 200 {
		t.Fatalf("status %d", code)
	}
	if out.Step != 3600 || len(out.TS) != 2 || out.TS[0] != 3600 || out.Count[0] != 2 || out.TS[1] != 7200 || out.Count[1] != 1 {
		t.Fatalf("buckets = %+v", out)
	}
	if out.Total.Count != 3 || out.Total.ByClass[model.ClassWAN].Seconds != 2 || out.Total.ByClass[model.ClassISPEdge].Count != 1 {
		t.Fatalf("total = %+v", out.Total)
	}
	if code := do(t, s.blips, httptest.NewRequest("GET", "/api/blips?from=5&to=5", nil), nil); code != http.StatusBadRequest {
		t.Fatalf("empty range: %d", code)
	}
}

func TestCompare(t *testing.T) {
	const day = 86400
	at := int64(20 * day)
	now := at + 2*day // two days since the change
	s, st := testServer(t, now)
	ctx := context.Background()
	inet, err := st.UpsertTarget(ctx, model.Target{Key: "icmp:1.1.1.1", Kind: model.KindICMP, Role: model.RoleInternet,
		Name: "Cloudflare", Addr: netip.MustParseAddr("1.1.1.1"), Family: model.FamilyV4})
	if err != nil {
		t.Fatal(err)
	}
	// Data from 10 days before the change; 1% loss before, none after.
	var stats []model.MinuteStat
	for ts := at - 10*day; ts < now; ts += 3600 {
		recv := 59
		if ts >= at {
			recv = 60
		}
		stats = append(stats, model.MinuteStat{TargetID: inet, TS: ts, Sent: 60, Recv: recv, Avg: f(12)})
	}
	if err := st.WriteMinutes(ctx, stats); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []model.Event{
		{Kind: model.EventOutage, Scope: model.FamilyV4, Class: model.ClassWAN, StartedAt: at - 5*day, EndedAt: ptrTo(at - 5*day + 120)},
		{Kind: model.EventOutage, Scope: model.FamilyV4, Class: model.ClassWAN, StartedAt: at - 2*day, EndedAt: ptrTo(at - 2*day + 90)},
		{Kind: model.EventOutage, Scope: model.FamilyV4, Class: model.ClassUpstream, StartedAt: at + day, EndedAt: ptrTo(at + day + 30)},
	} {
		if err := st.InsertEvent(ctx, &ev); err != nil {
			t.Fatal(err)
		}
	}
	st.InsertBlips(ctx, []model.Blip{{TS: at - day, Seconds: 2, Class: model.ClassISPEdge}, {TS: at - 3*day, Seconds: 1, Class: model.ClassISPEdge}})
	down, up := 300.0, 20.0
	st.InsertSpeedTest(ctx, &model.SpeedTest{TS: at - day, DownMbps: &down, UpMbps: &up})

	var out struct {
		Days   int64  `json:"days"`
		Before window `json:"before"`
		After  window `json:"after"`
	}
	if code := do(t, s.compare, httptest.NewRequest("GET", "/api/compare?at="+itoa(at)+"&days=14", nil), &out); code != 200 {
		t.Fatalf("status %d", code)
	}
	b, a := out.Before, out.After
	if b.From != at-14*day || b.To != at || a.From != at || a.To != now {
		t.Fatalf("windows: before %d..%d after %d..%d", b.From, b.To, a.From, a.To)
	}
	if b.Uptime.MonitoredS != 10*day || b.Uptime.Outages != 2 || b.Uptime.ByClass[model.ClassWAN].DowntimeS != 210 {
		t.Fatalf("before uptime = %+v", b.Uptime)
	}
	if a.Uptime.Outages != 1 || a.Uptime.ByClass[model.ClassUpstream] == nil || a.Uptime.MonitoredS != 2*day {
		t.Fatalf("after uptime = %+v", a.Uptime)
	}
	if b.Blips.Count != 2 || a.Blips.Count != 0 {
		t.Fatalf("blips before %d after %d", b.Blips.Count, a.Blips.Count)
	}
	if b.Internet.LossPct == nil || *b.Internet.LossPct < 1.6 || *b.Internet.LossPct > 1.7 || *a.Internet.LossPct != 0 {
		t.Fatalf("loss before %v after %v", b.Internet.LossPct, a.Internet.LossPct)
	}
	if b.Speed.Tests != 1 || *b.Speed.DownMbps != 300 || a.Speed.Tests != 0 || a.Speed.DownMbps != nil {
		t.Fatalf("speed before %+v after %+v", b.Speed, a.Speed)
	}

	for _, q := range []string{"", "at=x", "at=" + itoa(now+10), "at=" + itoa(at) + "&days=0", "at=" + itoa(at) + "&days=91"} {
		if code := do(t, s.compare, httptest.NewRequest("GET", "/api/compare?"+q, nil), nil); code != http.StatusBadRequest {
			t.Errorf("%q: status %d", q, code)
		}
	}
}

func ptrTo(v int64) *int64 { return &v }

func f(v float64) *float64 { return &v }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestMedian(t *testing.T) {
	if median(nil) != nil || *median([]float64{3, 1, 2}) != 2 || *median([]float64{4, 1, 3, 2}) != 2.5 {
		t.Fatal("median")
	}
}
