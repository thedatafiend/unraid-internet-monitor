// Package detect turns per-second probe results into outage, partial and
// degraded events. It is a pure state machine: no clocks, no I/O.
package detect

import (
	"math"
	"slices"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// Config holds the detection thresholds.
type Config struct {
	OutageAfter        int     // consecutive all-down ticks before an outage opens
	RecoverAfter       int     // consecutive up ticks before an event closes
	PartialAfter       int     // consecutive down ticks before a single-target event opens
	DegradedLossPct    float64 // loss over the window that counts as degraded
	DegradedP95Ms      float64 // p95 RTT over the window that counts as degraded
	DegradedWindow     int64   // seconds of samples the degraded check looks at
	DegradedOpenAfter  int64   // seconds the degraded condition must hold before opening
	DegradedClearAfter int64   // seconds it must be clear before closing
}

// DefaultConfig matches the defaults documented in the plan.
func DefaultConfig() Config {
	return Config{
		OutageAfter: 3, RecoverAfter: 3, PartialAfter: 10,
		DegradedLossPct: 2, DegradedP95Ms: 100,
		DegradedWindow: 60, DegradedOpenAfter: 120, DegradedClearAfter: 60,
	}
}

// Sample is one target's result within a tick.
type Sample struct {
	TargetID int64
	Name     string
	Role     string
	Family   string
	OK       bool
	RTT      float64 // ms, valid when OK
}

// Tick is one probe round.
type Tick struct {
	TS      int64
	Samples []Sample
}

// Transition reports an event opening or closing. The Event pointer stays
// owned by the detector; callers may set its ID and Planned but must not change Details
// (maps are replaced, never mutated, so snapshots stay race-free).
type Transition struct {
	Open  bool
	Event *model.Event
}

// States reported by Snapshot.
const (
	StateUnknown  = "unknown"
	StateOnline   = "online"
	StateDegraded = "degraded"
	StateOutage   = "outage"
)

// Snapshot is the detector's current view.
type Snapshot struct {
	State string        `json:"state"`
	Since int64         `json:"since"`
	Open  []model.Event `json:"open"`
}

// Detector is not safe for concurrent use; drive it from one goroutine.
type Detector struct {
	cfg     Config
	pending map[int64]Tick
	lastTS  int64

	v4      runState // IPv4 internet: outages
	v6      runState // IPv6 internet: partial events while IPv4 is up
	targets map[int64]*runState
	deg     degradedState

	state   string
	since   int64
	lastEnd int64 // when the last outage or degraded event ended
}

// New returns a detector.
func New(cfg Config) *Detector {
	return &Detector{cfg: cfg, pending: make(map[int64]Tick), targets: make(map[int64]*runState), state: StateUnknown}
}

// Add queues a tick. Ticks may arrive out of order; ticks at or before the
// last processed one are dropped.
func (d *Detector) Add(t Tick) {
	if t.TS > d.lastTS {
		d.pending[t.TS] = t
	}
}

// Advance processes queued ticks with TS <= upTo in time order.
func (d *Detector) Advance(upTo int64) []Transition {
	var ready []int64
	for ts := range d.pending {
		if ts <= upTo {
			ready = append(ready, ts)
		}
	}
	slices.Sort(ready)
	var out []Transition
	for _, ts := range ready {
		out = append(out, d.process(d.pending[ts])...)
		delete(d.pending, ts)
		d.lastTS = ts
	}
	return out
}

// Snapshot returns the current state and copies of the open events.
func (d *Detector) Snapshot() Snapshot {
	s := Snapshot{State: d.state, Since: d.since}
	for _, ev := range d.openEvents() {
		s.Open = append(s.Open, *ev)
	}
	return s
}

func (d *Detector) openEvents() []*model.Event {
	var evs []*model.Event
	for _, ev := range []*model.Event{d.v4.open, d.v6.open, d.deg.open} {
		if ev != nil {
			evs = append(evs, ev)
		}
	}
	ids := make([]int64, 0, len(d.targets))
	for id := range d.targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if ev := d.targets[id].open; ev != nil {
			evs = append(evs, ev)
		}
	}
	return evs
}

// observation is the gateway / ISP-hop status within one tick.
type observation struct {
	hasGW, gwDown, hasISP, ispDown bool
}

type counts struct {
	ticks, gwDown, ispDown int
	hasGW, hasISP          bool
}

func (c *counts) add(o observation) {
	c.ticks++
	c.hasGW = c.hasGW || o.hasGW
	c.hasISP = c.hasISP || o.hasISP
	if o.hasGW && o.gwDown {
		c.gwDown++
	}
	if o.hasISP && o.ispDown {
		c.ispDown++
	}
}

// classify blames the closest hop that was down for most of the outage.
func (c counts) classify() string {
	switch {
	case c.hasGW && c.gwDown*2 > c.ticks:
		return model.ClassLocal
	case c.hasISP && c.ispDown*2 > c.ticks:
		return model.ClassISPEdge
	}
	return model.ClassUpstream
}

func (c counts) details() map[string]any {
	m := map[string]any{"down_ticks": c.ticks}
	if c.hasGW {
		m["gateway_down_pct"] = pct(c.gwDown, c.ticks)
	}
	if c.hasISP {
		m["isp_down_pct"] = pct(c.ispDown, c.ticks)
	}
	return m
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(1000*float64(n)/float64(total)) / 10
}

// runState tracks consecutive down/up ticks for one scope.
type runState struct {
	downRun, upRun    int
	runStart, upStart int64
	run, total        counts
	open              *model.Event
}

func (d *Detector) process(t Tick) []Transition {
	var out []Transition
	emit := func(open bool, ev *model.Event) {
		out = append(out, Transition{Open: open, Event: ev})
		if !open && ((ev.Kind == model.EventOutage) || ev.Kind == model.EventDegraded) {
			d.lastEnd = *ev.EndedAt
		}
	}

	obs := observation{}
	up4, has4, up6, has6 := false, false, false, false
	down := map[string]bool{} // family -> every internet target of that family failed
	seen := map[string]bool{}
	for _, s := range t.Samples {
		switch s.Role {
		case model.RoleGateway:
			obs.hasGW = true
			obs.gwDown = obs.gwDown || !s.OK
		case model.RoleISP:
			obs.hasISP = true
			obs.ispDown = obs.ispDown || !s.OK
		case model.RoleInternet:
			if !seen[s.Family] {
				seen[s.Family], down[s.Family] = true, true
			}
			if s.OK {
				down[s.Family] = false
			}
		}
	}
	has4, up4 = seen[model.FamilyV4], !down[model.FamilyV4]
	has6, up6 = seen[model.FamilyV6], !down[model.FamilyV6]

	// IPv4 internet: the primary outage signal. An open degraded event is
	// closed before a new outage is reported so events stay in time order.
	if has4 {
		var v4 []Transition
		d.step(&d.v4, t.TS, up4, obs, d.cfg.OutageAfter, model.EventOutage, model.FamilyV4, true,
			func(open bool, ev *model.Event) { v4 = append(v4, Transition{Open: open, Event: ev}) })
		if d.v4.open != nil && d.deg.open != nil {
			d.closeDegraded(d.v4.open.StartedAt, map[string]any{"superseded": true}, emit)
		}
		for _, x := range v4 {
			emit(x.Open, x.Event)
		}
	}
	v4Down := d.v4.open != nil || d.v4.downRun > 0

	// IPv6 internet: reported as partial, and only while IPv4 works.
	if has6 {
		if v4Down {
			d.supersede(&d.v6, t.TS, emit)
		} else {
			d.step(&d.v6, t.TS, up6, obs, d.cfg.OutageAfter, model.EventPartial, model.FamilyV6, false, emit)
		}
	}

	// Single targets (internet and custom) down while their family is up.
	present := map[int64]bool{}
	for _, s := range t.Samples {
		if s.Role != model.RoleInternet && s.Role != model.RoleCustom {
			continue
		}
		present[s.TargetID] = true
		st := d.targets[s.TargetID]
		if st == nil {
			st = &runState{}
			d.targets[s.TargetID] = st
		}
		if v4Down || (seen[s.Family] && down[s.Family]) {
			d.supersede(st, t.TS, emit)
			continue
		}
		d.step(st, t.TS, s.OK, obs, d.cfg.PartialAfter, model.EventPartial, s.Name, false, emit)
		if st.open != nil && st.open.Details == nil {
			st.open.Details = map[string]any{"target_id": s.TargetID, "role": s.Role}
		}
	}
	for id, st := range d.targets {
		if !present[id] { // target removed by rediscovery
			d.supersede(st, t.TS, emit)
			delete(d.targets, id)
		}
	}

	if has4 {
		// A short blip counts as loss in the degraded window; only a real
		// outage takes over from the degraded check.
		d.stepDegraded(t, d.v4.open != nil, emit)
	}
	d.updateState(t.TS)
	return out
}

// step advances one scope's run counters and opens or closes its event.
func (d *Detector) step(st *runState, ts int64, up bool, obs observation, openAfter int, kind, scope string, classify bool, emit func(bool, *model.Event)) {
	if !up {
		st.upRun = 0
		if st.downRun == 0 {
			st.runStart, st.run = ts, counts{}
		}
		st.downRun++
		st.run.add(obs)
		switch {
		case st.open == nil && st.downRun >= openAfter:
			ev := &model.Event{Kind: kind, Scope: scope, StartedAt: st.runStart}
			if classify {
				ev.Class = st.run.classify()
				ev.Details = st.run.details()
			}
			st.open, st.total = ev, st.run
			emit(true, ev)
		case st.open != nil:
			st.total.add(obs)
		}
		return
	}
	if st.open == nil {
		st.downRun = 0
		return
	}
	if st.upRun == 0 {
		st.upStart = ts
	}
	st.upRun++
	if st.upRun < d.cfg.RecoverAfter {
		return
	}
	ev := st.open
	end := st.upStart
	ev.EndedAt = &end
	if classify {
		ev.Class = st.total.classify()
		ev.Details = st.total.details()
	}
	*st = runState{}
	emit(false, ev)
}

// supersede closes st's open event at ts (a wider failure now covers it)
// and resets its counters.
func (d *Detector) supersede(st *runState, ts int64, emit func(bool, *model.Event)) {
	if ev := st.open; ev != nil {
		end := ts
		ev.EndedAt = &end
		ev.Details = withKey(ev.Details, "superseded", true)
		emit(false, ev)
	}
	*st = runState{}
}

func withKey(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for key, val := range m {
		out[key] = val
	}
	out[k] = v
	return out
}

type tickAgg struct {
	ts         int64
	sent, recv int
	rtts       []float64
}

type degradedState struct {
	window              []tickAgg
	badSince, goodSince int64
	open                *model.Event
	peakLoss, peakP95   float64
}

func (d *Detector) stepDegraded(t Tick, outage bool, emit func(bool, *model.Event)) {
	st := &d.deg
	if outage {
		// Outage ticks would read as 100% loss; the outage event covers them.
		if st.open != nil {
			d.closeDegraded(d.v4.open.StartedAt, map[string]any{"superseded": true}, emit)
		}
		st.window, st.badSince, st.goodSince = nil, 0, 0
		return
	}

	agg := tickAgg{ts: t.TS}
	for _, s := range t.Samples {
		if s.Role == model.RoleInternet && s.Family == model.FamilyV4 {
			agg.sent++
			if s.OK {
				agg.recv++
				agg.rtts = append(agg.rtts, s.RTT)
			}
		}
	}
	st.window = append(st.window, agg)
	cut := 0
	for cut < len(st.window) && st.window[cut].ts <= t.TS-d.cfg.DegradedWindow {
		cut++
	}
	st.window = st.window[cut:]
	if int64(len(st.window))*2 < d.cfg.DegradedWindow {
		return // not enough data yet
	}

	var sent, recv int
	var rtts []float64
	for _, a := range st.window {
		sent, recv = sent+a.sent, recv+a.recv
		rtts = append(rtts, a.rtts...)
	}
	loss := 100 * float64(sent-recv) / float64(max(sent, 1))
	var p95 float64
	if len(rtts) > 0 {
		slices.Sort(rtts)
		p95 = rtts[max(0, int(math.Ceil(0.95*float64(len(rtts))))-1)]
	}
	bad := loss >= d.cfg.DegradedLossPct || p95 > d.cfg.DegradedP95Ms

	if bad {
		st.goodSince = 0
		if st.badSince == 0 {
			st.badSince = t.TS
		}
		if st.open == nil && t.TS-st.badSince >= d.cfg.DegradedOpenAfter {
			st.open = &model.Event{
				Kind: model.EventDegraded, Scope: model.FamilyV4, StartedAt: st.badSince,
				Details: map[string]any{"loss_pct": round1(loss), "p95_ms": round1(p95)},
			}
			st.peakLoss, st.peakP95 = loss, p95
			emit(true, st.open)
		} else if st.open != nil {
			st.peakLoss, st.peakP95 = max(st.peakLoss, loss), max(st.peakP95, p95)
		}
		return
	}
	st.badSince = 0
	if st.open == nil {
		return
	}
	if st.goodSince == 0 {
		st.goodSince = t.TS
	}
	if t.TS-st.goodSince >= d.cfg.DegradedClearAfter {
		d.closeDegraded(st.goodSince, nil, emit)
	}
}

func (d *Detector) closeDegraded(end int64, extra map[string]any, emit func(bool, *model.Event)) {
	st := &d.deg
	ev := st.open
	details := withKey(ev.Details, "peak_loss_pct", round1(st.peakLoss))
	details["peak_p95_ms"] = round1(st.peakP95)
	for k, v := range extra {
		details[k] = v
	}
	ev.EndedAt, ev.Details = &end, details
	st.open, st.badSince, st.goodSince = nil, 0, 0
	emit(false, ev)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (d *Detector) updateState(ts int64) {
	next, since := StateOnline, ts
	switch {
	case d.v4.open != nil:
		next, since = StateOutage, d.v4.open.StartedAt
	case d.deg.open != nil:
		next, since = StateDegraded, d.deg.open.StartedAt
	}
	if next != d.state {
		if next == StateOnline && d.state != StateUnknown && d.lastEnd != 0 {
			since = d.lastEnd // recovered: online since the first good tick
		}
		d.state, d.since = next, since
	}
}
