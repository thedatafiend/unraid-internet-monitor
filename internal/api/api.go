// Package api serves the JSON API and the web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/alert"
	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/monitor"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
	"github.com/thedatafiend/unraid-internet-monitor/web"
)

const (
	maxPoints     = 1000 // target points per series for /api/metrics
	defaultLive   = 900
	maxLive       = 3600
	staleAfter    = 30 * time.Second
	defaultWindow = 24 * time.Hour
)

// Server holds the API dependencies.
type Server struct {
	eng       *monitor.Engine
	st        *store.Store
	alerts    *alert.Manager
	log       *slog.Logger
	version   string
	retention int
	cfg       config.Config
	now       func() time.Time
}

// New returns the HTTP handler.
func New(eng *monitor.Engine, st *store.Store, alerts *alert.Manager, log *slog.Logger, version string, cfg config.Config) http.Handler {
	s := &Server{eng: eng, st: st, alerts: alerts, log: log, version: version, retention: cfg.RetentionDays, cfg: cfg, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/targets", s.targets)
	mux.HandleFunc("GET /api/metrics", s.metrics)
	mux.HandleFunc("GET /api/live", s.live)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/uptime", s.uptimeHandler)
	mux.HandleFunc("POST /api/alerts/test", s.testAlert)
	mux.HandleFunc("GET /api/stream", s.stream)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/traces", s.traces)
	mux.HandleFunc("GET /api/public-ip", s.publicIPs)
	mux.HandleFunc("GET /api/http", s.httpSamples)
	mux.HandleFunc("GET /api/speedtests", s.speedtests)
	mux.HandleFunc("POST /api/speedtest", s.runSpeedtest)
	mux.HandleFunc("GET /api/speedtest/progress", s.speedProgress)
	mux.Handle("GET /", web.Handler())
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	last := s.eng.LastTick()
	if last.IsZero() || s.now().Sub(last) > staleAfter {
		http.Error(w, "probe loop stalled", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	size, err := s.st.SizeBytes(r.Context())
	if err != nil {
		s.log.Warn("db size", "err", err)
	}
	now := s.now().Unix()
	up, err := s.uptime(r.Context(), now-86400, now, now)
	if err != nil {
		s.log.Warn("uptime", "err", err)
	}
	writeJSON(w, struct {
		monitor.Status
		Uptime24h     uptime `json:"uptime_24h"`
		AlertsEnabled bool   `json:"alerts_enabled"`
		Version       string `json:"version"`
		DBSizeBytes   int64  `json:"db_size_bytes"`
		RetentionDays int    `json:"retention_days"`
	}{s.eng.Status(s.now()), up, s.alerts.Enabled(), s.version, size, s.retention})
}

// events lists events overlapping [from, to]; kind filters (optional).
// Defaults to the last 7 days.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.now().Unix()
	to, err1 := intParam(q.Get("to"), now)
	from, err2 := intParam(q.Get("from"), to-7*86400)
	if err1 != nil || err2 != nil || from > to {
		httpError(w, fmt.Errorf("from/to: want unix seconds with from <= to"), http.StatusBadRequest)
		return
	}
	evs, err := s.st.Events(r.Context(), from, to, q.Get("kind"))
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, evs)
}

type uptime struct {
	From       int64    `json:"from"`
	To         int64    `json:"to"`
	MonitoredS int64    `json:"monitored_s"` // part of the range with data
	DowntimeS  int64    `json:"downtime_s"`
	Outages    int      `json:"outages"`
	UptimePct  *float64 `json:"uptime_pct"` // null before any data exists
	LongestS   int64    `json:"longest_s"`
	LastOutage *int64   `json:"last_outage_at"`
}

func (s *Server) uptimeHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.now().Unix()
	to, err1 := intParam(q.Get("to"), now)
	from, err2 := intParam(q.Get("from"), to-86400)
	if err1 != nil || err2 != nil || from >= to {
		httpError(w, fmt.Errorf("from/to: want unix seconds with from < to"), http.StatusBadRequest)
		return
	}
	up, err := s.uptime(r.Context(), from, to, now)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, up)
}

// uptime computes IPv4 internet availability over [from, to] from outage
// events, counting only the part of the range for which data exists.
func (s *Server) uptime(ctx context.Context, from, to, now int64) (uptime, error) {
	u := uptime{From: from, To: to}
	first, err := s.st.FirstDataTS(ctx)
	if err != nil || first == 0 {
		return u, err
	}
	start := max(from, first)
	end := min(to, now)
	if end <= start {
		return u, nil
	}
	u.MonitoredS = end - start
	evs, err := s.st.Events(ctx, start, end, model.EventOutage)
	if err != nil {
		return u, err
	}
	for _, ev := range evs {
		if ev.Scope != model.FamilyV4 {
			continue
		}
		evEnd := now
		if ev.EndedAt != nil {
			evEnd = *ev.EndedAt
		}
		d := min(evEnd, end) - max(ev.StartedAt, start)
		if d <= 0 {
			continue
		}
		u.Outages++
		u.DowntimeS += d
		u.LongestS = max(u.LongestS, evEnd-ev.StartedAt)
		started := ev.StartedAt
		u.LastOutage = &started
	}
	pct := round3(100 * float64(u.MonitoredS-u.DowntimeS) / float64(u.MonitoredS))
	u.UptimePct = &pct
	return u, nil
}

// stream pushes a LiveTick per probe round as server-sent events.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, errors.New("streaming unsupported"), http.StatusInternalServerError)
		return
	}
	msgs, done, cancel := s.eng.Subscribe()
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case m := <-msgs:
			fmt.Fprintf(w, "data: %s\n\n", m)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// config shows the effective settings. The webhook URL is a secret and is
// only reported as set or not.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	c := s.cfg
	custom := []map[string]string{}
	for _, t := range c.CustomTargets {
		custom = append(custom, map[string]string{"name": t.Name, "host": t.Host})
	}
	secs := func(d time.Duration) float64 { return d.Seconds() }
	writeJSON(w, map[string]any{
		"retention_days":       c.RetentionDays,
		"ping_interval_s":      secs(c.PingInterval),
		"ping_timeout_s":       secs(c.PingTimeout),
		"ping_targets":         c.PingTargets,
		"ping_targets_v6":      c.PingTargetsV6,
		"ipv6":                 c.IPv6,
		"gateway":              c.Gateway,
		"isp_hop":              c.ISPHop,
		"custom_targets":       custom,
		"outage_threshold_s":   secs(c.OutageThreshold),
		"degraded_loss_pct":    c.DegradedLossPct,
		"degraded_p95_ms":      c.DegradedP95Ms,
		"degraded_min_s":       secs(c.DegradedMin),
		"dns_servers":          c.DNSServers,
		"dns_query":            c.DNSQuery,
		"dns_interval_s":       secs(c.DNSInterval),
		"http_targets":         c.HTTPTargets,
		"http_interval_s":      secs(c.HTTPInterval),
		"public_ip_interval_s": secs(c.PublicIPInterval),
		"speedtest_schedule":   c.SpeedtestSchedule,
		"speedtest_duration_s": secs(c.SpeedtestDuration),
		"speedtest_streams":    c.SpeedtestStreams,
		"alerts": map[string]any{
			"discord":        c.DiscordWebhookURL != "",
			"min_outage_s":   secs(c.AlertMinOutage),
			"coalesce_s":     secs(c.AlertCoalesce),
			"isp_hop_change": c.AlertISPHopChange,
			"ip_change":      c.AlertIPChange,
			"min_down_mbps":  c.AlertMinDownMbps,
			"min_up_mbps":    c.AlertMinUpMbps,
		},
	})
}

// rangeParams reads from/to (unix seconds) with a default window ending now.
func (s *Server) rangeParams(r *http.Request, window int64) (int64, int64, error) {
	q := r.URL.Query()
	to, err1 := intParam(q.Get("to"), s.now().Unix())
	from, err2 := intParam(q.Get("from"), to-window)
	if err1 != nil || err2 != nil || from > to {
		return 0, 0, fmt.Errorf("from/to: want unix seconds with from <= to")
	}
	return from, to, nil
}

// traces returns traceroutes for one event (event_id) or a time range.
func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	eventID, err := intParam(r.URL.Query().Get("event_id"), 0)
	if err != nil {
		httpError(w, fmt.Errorf("event_id: want an integer"), http.StatusBadRequest)
		return
	}
	from, to, err := s.rangeParams(r, 7*86400)
	if err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	trs, err := s.st.Traces(r.Context(), eventID, from, to, 50)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, trs)
}

// publicIPs returns the public address history, newest first.
func (s *Server) publicIPs(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeParams(r, int64(s.retention)*86400)
	if err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	ips, err := s.st.PublicIPs(r.Context(), from, to)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, ips)
}

// httpSamples returns web-request phase timings for one target.
func (s *Server) httpSamples(w http.ResponseWriter, r *http.Request) {
	target, err := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	if err != nil {
		httpError(w, fmt.Errorf("target: want a target id"), http.StatusBadRequest)
		return
	}
	from, to, err := s.rangeParams(r, 86400)
	if err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	samples, err := s.st.HTTPSamples(r.Context(), target, from, to)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, samples)
}

func (s *Server) speedtests(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeParams(r, int64(s.retention)*86400)
	if err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	res, err := s.st.SpeedTests(r.Context(), from, to)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

func (s *Server) runSpeedtest(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.RunSpeedtest(); err != nil {
		httpError(w, err, http.StatusConflict)
		return
	}
	writeJSONCode(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) speedProgress(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.eng.SpeedProgress())
}

func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	if err := s.alerts.Test(s.now().Unix()); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, alert.ErrDisabled) {
			code = http.StatusConflict
		}
		httpError(w, err, code)
		return
	}
	writeJSONCode(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) targets(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.Targets(r.Context())
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if ts == nil {
		ts = []model.Target{}
	}
	writeJSON(w, ts)
}

// metrics returns one target's stored history as parallel arrays (the shape
// uPlot consumes directly). Query: target (required), from, to (unix seconds),
// step (seconds, rounded up to whole minutes; auto if omitted).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target, err := strconv.ParseInt(q.Get("target"), 10, 64)
	if err != nil {
		httpError(w, fmt.Errorf("target: want a target id"), http.StatusBadRequest)
		return
	}
	now := s.now().Unix()
	to, err1 := intParam(q.Get("to"), now)
	from, err2 := intParam(q.Get("from"), to-int64(defaultWindow/time.Second))
	step, err3 := intParam(q.Get("step"), 0)
	if err1 != nil || err2 != nil || err3 != nil || from >= to || step < 0 {
		httpError(w, fmt.Errorf("from/to/step: want unix seconds with from < to"), http.StatusBadRequest)
		return
	}
	// Nothing older than the retention window exists; clamping keeps the auto step sensible.
	from = max(from, now-int64(s.retention)*86400)
	if from >= to {
		httpError(w, fmt.Errorf("range is outside the %d-day retention window", s.retention), http.StatusBadRequest)
		return
	}
	step = pickStep(to-from, step)

	buckets, err := s.st.Series(r.Context(), target, from, to, step)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	n := len(buckets)
	out := struct {
		Target int64      `json:"target"`
		From   int64      `json:"from"`
		To     int64      `json:"to"`
		Step   int64      `json:"step"`
		TS     []int64    `json:"ts"`
		Sent   []int      `json:"sent"`
		Recv   []int      `json:"recv"`
		Loss   []float64  `json:"loss_pct"`
		Min    []*float64 `json:"min"`
		Avg    []*float64 `json:"avg"`
		P50    []*float64 `json:"p50"`
		P95    []*float64 `json:"p95"`
		P99    []*float64 `json:"p99"`
		Max    []*float64 `json:"max"`
		Jitter []*float64 `json:"jitter"`
	}{
		Target: target, From: from, To: to, Step: step,
		TS: make([]int64, n), Sent: make([]int, n), Recv: make([]int, n), Loss: make([]float64, n),
		Min: make([]*float64, n), Avg: make([]*float64, n), P50: make([]*float64, n), P95: make([]*float64, n),
		P99: make([]*float64, n), Max: make([]*float64, n), Jitter: make([]*float64, n),
	}
	for i, b := range buckets {
		out.TS[i], out.Sent[i], out.Recv[i] = b.TS, b.Sent, b.Recv
		if b.Sent > 0 {
			out.Loss[i] = round3(100 * float64(b.Sent-b.Recv) / float64(b.Sent))
		}
		out.Min[i], out.Avg[i], out.P50[i], out.P95[i] = roundPtr(b.Min), roundPtr(b.Avg), roundPtr(b.P50), roundPtr(b.P95)
		out.P99[i], out.Max[i], out.Jitter[i] = roundPtr(b.P99), roundPtr(b.Max), roundPtr(b.Jitter)
	}
	writeJSON(w, out)
}

// pickStep returns a bucket size in whole minutes giving at most maxPoints
// buckets over span, never smaller than requested.
func pickStep(span, requested int64) int64 {
	step := max(requested, (span+maxPoints-1)/maxPoints, 60)
	return (step + 59) / 60 * 60
}

// live returns per-second RTTs from the in-memory ring buffers.
// state is 1 = reply, 0 = lost, -1 = no data.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	secs, err := intParam(r.URL.Query().Get("seconds"), defaultLive)
	if err != nil || secs < 1 {
		httpError(w, fmt.Errorf("seconds: want a positive integer"), http.StatusBadRequest)
		return
	}
	secs = min(secs, maxLive)
	to := s.now().Unix() - 1
	from := to - secs + 1

	type series struct {
		ID    int64      `json:"id"`
		Name  string     `json:"name"`
		Role  string     `json:"role"`
		RTT   []*float64 `json:"rtt"`
		State []int8     `json:"state"`
	}
	out := struct {
		From   int64    `json:"from"`
		To     int64    `json:"to"`
		Series []series `json:"series"`
	}{From: from, To: to, Series: []series{}}

	for _, t := range s.eng.Targets() {
		ring := s.eng.Ring(t.ID)
		if ring == nil {
			continue
		}
		se := series{ID: t.ID, Name: t.Name, Role: t.Role, RTT: make([]*float64, secs), State: make([]int8, secs)}
		for i := range se.State {
			se.State[i] = -1
		}
		for _, p := range ring.Range(from, to) {
			i := p.TS - from
			if p.OK {
				se.State[i] = 1
				v := round3(float64(p.RTT))
				se.RTT[i] = &v
			} else {
				se.State[i] = 0
			}
		}
		out.Series = append(out.Series, se)
	}
	writeJSON(w, out)
}

func intParam(v string, def int64) (int64, error) {
	if v == "" {
		return def, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := round3(*v)
	return &r
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONCode(w, http.StatusOK, v) }

func writeJSONCode(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error, code int) {
	writeJSONCode(w, code, map[string]string{"error": err.Error()})
}
