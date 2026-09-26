// Package api serves the JSON API and the web UI.
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/monitor"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

//go:embed placeholder.html
var placeholderHTML []byte

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
	log       *slog.Logger
	version   string
	retention int
	now       func() time.Time
}

// New returns the HTTP handler.
func New(eng *monitor.Engine, st *store.Store, log *slog.Logger, version string, retentionDays int) http.Handler {
	s := &Server{eng: eng, st: st, log: log, version: version, retention: retentionDays, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/targets", s.targets)
	mux.HandleFunc("GET /api/metrics", s.metrics)
	mux.HandleFunc("GET /api/live", s.live)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(placeholderHTML)
	})
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
	writeJSON(w, struct {
		monitor.Status
		Version       string `json:"version"`
		DBSizeBytes   int64  `json:"db_size_bytes"`
		RetentionDays int    `json:"retention_days"`
	}{s.eng.Status(s.now()), s.version, size, s.retention})
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
