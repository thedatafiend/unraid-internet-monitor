package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

const (
	maxMarkerLabel     = 80
	defaultCompareDays = 14
)

type blipStat struct {
	Count   int `json:"count"`
	Seconds int `json:"seconds"`
}

// blipSummary is the number of brief drops in a range, in total and by
// likely cause.
type blipSummary struct {
	Count   int                  `json:"count"`
	ByClass map[string]*blipStat `json:"by_class"`
}

func (s *Server) blipSummary(ctx context.Context, from, to int64) (blipSummary, error) {
	sum := blipSummary{ByClass: map[string]*blipStat{}}
	if to <= from {
		return sum, nil
	}
	counts, err := s.st.BlipCounts(ctx, from, to, to-from)
	for _, c := range counts {
		sum.add(c)
	}
	return sum, err
}

func (b *blipSummary) add(c store.BlipCount) {
	b.Count += c.Count
	bs := b.ByClass[c.Class]
	if bs == nil {
		bs = &blipStat{}
		b.ByClass[c.Class] = bs
	}
	bs.Count += c.Count
	bs.Seconds += c.Seconds
}

// blips returns brief drops counted in buckets for charts. Query: from, to
// (unix seconds, default the last 24 h), step (seconds, auto if omitted, as
// for /api/metrics). Buckets without drops are left out.
func (s *Server) blips(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeParams(r, 86400)
	step, err2 := intParam(r.URL.Query().Get("step"), 0)
	if err != nil || err2 != nil || from >= to || step < 0 {
		httpError(w, fmt.Errorf("from/to/step: want unix seconds with from < to"), http.StatusBadRequest)
		return
	}
	step = pickStep(to-from, step)
	counts, err := s.st.BlipCounts(r.Context(), from, to, step)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	out := struct {
		From  int64       `json:"from"`
		To    int64       `json:"to"`
		Step  int64       `json:"step"`
		TS    []int64     `json:"ts"`
		Count []int       `json:"count"` // drops per bucket
		Total blipSummary `json:"total"`
	}{From: from, To: to, Step: step, TS: []int64{}, Count: []int{}, Total: blipSummary{ByClass: map[string]*blipStat{}}}
	for _, c := range counts {
		if n := len(out.TS); n == 0 || out.TS[n-1] != c.TS {
			out.TS = append(out.TS, c.TS)
			out.Count = append(out.Count, 0)
		}
		out.Count[len(out.Count)-1] += c.Count
		out.Total.add(c)
	}
	writeJSON(w, out)
}

func (s *Server) markers(w http.ResponseWriter, r *http.Request) {
	ms, err := s.st.Markers(r.Context())
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, ms)
}

// addMarker stores {ts, label}. Only JSON bodies are accepted, so a plain
// form on another site cannot post one.
func (s *Server) addMarker(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		httpError(w, fmt.Errorf("want a JSON body"), http.StatusUnsupportedMediaType)
		return
	}
	var m model.Marker
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&m); err != nil {
		httpError(w, fmt.Errorf("body: %v", err), http.StatusBadRequest)
		return
	}
	m.ID = 0
	m.Label = strings.TrimSpace(m.Label)
	now := s.now().Unix()
	switch {
	case m.Label == "":
		httpError(w, fmt.Errorf("label: required"), http.StatusBadRequest)
		return
	case utf8.RuneCountInString(m.Label) > maxMarkerLabel:
		httpError(w, fmt.Errorf("label: at most %d characters", maxMarkerLabel), http.StatusBadRequest)
		return
	case m.TS <= 0 || m.TS > now+365*86400:
		httpError(w, fmt.Errorf("ts: want unix seconds no more than a year ahead"), http.StatusBadRequest)
		return
	}
	if err := s.st.InsertMarker(r.Context(), &m); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSONCode(w, http.StatusCreated, m)
}

func (s *Server) deleteMarker(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, fmt.Errorf("id: want a marker id"), http.StatusBadRequest)
		return
	}
	ok, err := s.st.DeleteMarker(r.Context(), id)
	switch {
	case err != nil:
		httpError(w, err, http.StatusInternalServerError)
	case !ok:
		httpError(w, fmt.Errorf("marker %d not found", id), http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// window describes the connection over one side of a comparison.
type window struct {
	From     int64       `json:"from"`
	To       int64       `json:"to"`
	Uptime   uptime      `json:"uptime"`
	Blips    blipSummary `json:"blips"`
	Internet struct {
		Sent    int64    `json:"sent"`
		LossPct *float64 `json:"loss_pct"` // outside outages
		AvgMs   *float64 `json:"avg_ms"`
	} `json:"internet"`
	Speed struct {
		Tests    int      `json:"tests"`
		DownMbps *float64 `json:"down_mbps"` // medians
		UpMbps   *float64 `json:"up_mbps"`
	} `json:"speed"`
}

// compare summarises equal windows before and after a moment, such as a
// marker, so the effect of a change can be judged. Query: at (unix
// seconds, required), days (each side, default 14). The "after" window
// ends now until enough time has passed.
func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.now().Unix()
	at, err := strconv.ParseInt(q.Get("at"), 10, 64)
	if err != nil || at <= 0 || at > now {
		httpError(w, fmt.Errorf("at: want unix seconds, not in the future"), http.StatusBadRequest)
		return
	}
	days, err := intParam(q.Get("days"), defaultCompareDays)
	if err != nil || days < 1 || days > int64(s.retention) {
		httpError(w, fmt.Errorf("days: want 1 to %d", s.retention), http.StatusBadRequest)
		return
	}
	span := days * 86400
	before, err := s.window(r.Context(), at-span, at, now)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	after, err := s.window(r.Context(), at, min(at+span, now), now)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		At     int64  `json:"at"`
		Days   int64  `json:"days"`
		Before window `json:"before"`
		After  window `json:"after"`
	}{at, days, before, after})
}

func (s *Server) window(ctx context.Context, from, to, now int64) (window, error) {
	wd := window{From: from, To: to}
	var err error
	if wd.Uptime, err = s.uptime(ctx, from, to, now); err != nil {
		return wd, err
	}
	if wd.Blips, err = s.blipSummary(ctx, from, to); err != nil {
		return wd, err
	}
	inet, err := s.st.InternetStats(ctx, from, to, now)
	if err != nil {
		return wd, err
	}
	wd.Internet.Sent, wd.Internet.AvgMs = inet.Sent, roundPtr(inet.Avg)
	if inet.Sent > 0 {
		loss := round3(100 * float64(inet.Sent-inet.Recv) / float64(inet.Sent))
		wd.Internet.LossPct = &loss
	}
	tests, err := s.st.SpeedTests(ctx, from, to-1) // SpeedTests includes its end
	if err != nil {
		return wd, err
	}
	var downs, ups []float64
	for _, t := range tests {
		if t.DownMbps != nil {
			downs = append(downs, *t.DownMbps)
		}
		if t.UpMbps != nil {
			ups = append(ups, *t.UpMbps)
		}
	}
	wd.Speed.Tests = len(downs)
	wd.Speed.DownMbps, wd.Speed.UpMbps = median(downs), median(ups)
	return wd, nil
}

func median(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	slices.Sort(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + m) / 2
	}
	m = round3(m)
	return &m
}
