package monitor

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/detect"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
)

const (
	dnsTimeout      = 2 * time.Second
	svcFailAfter    = 3 // consecutive failures before a DNS/web event opens
	svcRecoverAfter = 2 // consecutive successes before it closes
	traceHops       = 12
	firstIPCheck    = 15 * time.Second
)

// svcLast is the most recent result for a DNS or web target.
type svcLast struct {
	at   int64
	ok   bool
	ms   *float64
	err  string
	http *model.HTTPSample
}

// svcState tracks consecutive failures of one DNS or web target.
type svcState struct {
	fails, oks int
	okSince    int64
	open       *model.Event
}

type services struct {
	mu     sync.Mutex
	last   map[int64]svcLast
	state  map[int64]*svcState
	traces map[int64]map[string]any // outage event ID -> trace summary for its details
	pubIP  model.PublicIP
}

func newServices() *services {
	return &services{last: map[int64]svcLast{}, state: map[int64]*svcState{}, traces: map[int64]map[string]any{}}
}

// every runs fn now and then every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func(time.Time)) {
	if interval <= 0 {
		return
	}
	fn(time.Now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			fn(now)
		}
	}
}

func (e *Engine) targetsOfKind(kind string) []model.Target {
	var out []model.Target
	for _, t := range e.Targets() {
		if t.Kind == kind {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) dnsRound(ctx context.Context, now time.Time) {
	ts := now.Unix()
	var wg sync.WaitGroup
	for _, t := range e.targetsOfKind(model.KindDNS) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := probe.QueryDNS(ctx, t.Addr, e.cfg.DNSQuery, dnsTimeout)
			if ctx.Err() != nil {
				return
			}
			e.record(t.ID, ts, r)
			e.serviceResult(ctx, t, ts, r.OK, r.RTT, r.Err, nil)
		}()
	}
	wg.Wait()
}

func (e *Engine) httpRound(ctx context.Context, now time.Time) {
	ts := now.Unix()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var samples []model.HTTPSample
	for _, t := range e.targetsOfKind(model.KindHTTP) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.http.Probe(ctx, t.URL)
			if ctx.Err() != nil {
				return
			}
			ms := func(d time.Duration) *float64 {
				if d <= 0 {
					return nil
				}
				v := roundV(d.Seconds() * 1000)
				return &v
			}
			s := model.HTTPSample{TargetID: t.ID, TS: ts, OK: r.OK, Status: r.Status,
				DNSMs: ms(r.DNS), ConnectMs: ms(r.Connect), TLSMs: ms(r.TLS), TTFBMs: ms(r.TTFB), TotalMs: ms(r.Total)}
			if r.Err != nil {
				s.Error = shortErr(r.Err)
			}
			e.record(t.ID, ts, probe.Result{OK: r.OK, RTT: r.Total, Err: r.Err})
			e.serviceResult(ctx, t, ts, r.OK, r.Total, r.Err, &s)
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if err := e.st.WriteHTTPSamples(ctx, samples); err != nil && ctx.Err() == nil {
		e.log.Error("storing web samples", "err", err)
	}
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// serviceResult records the latest result and opens or closes a partial
// event after repeated failures. Failures during an internet outage are
// covered by the outage itself.
func (e *Engine) serviceResult(ctx context.Context, t model.Target, ts int64, ok bool, rtt time.Duration, err error, h *model.HTTPSample) {
	last := svcLast{at: ts, ok: ok, http: h}
	if ok {
		v := roundV(rtt.Seconds() * 1000)
		last.ms = &v
	} else if err != nil {
		last.err = shortErr(err)
	}

	e.mu.RLock()
	outage := e.detState.State == detect.StateOutage
	e.mu.RUnlock()

	var opened, closed *model.Event
	e.svc.mu.Lock()
	e.svc.last[t.ID] = last
	st := e.svc.state[t.ID]
	if st == nil {
		st = &svcState{}
		e.svc.state[t.ID] = st
	}
	switch {
	case outage:
		st.fails, st.oks = 0, 0
	case !ok:
		st.fails++
		st.oks = 0
		if st.open == nil && st.fails >= svcFailAfter {
			st.open = &model.Event{Kind: model.EventPartial, Scope: t.Name, StartedAt: ts,
				Details: map[string]any{"target_id": t.ID, "role": t.Role, "error": last.err}}
			opened = st.open
		}
	default:
		if st.oks == 0 {
			st.okSince = ts
		}
		st.oks++
		st.fails = 0
		if st.open != nil && st.oks >= svcRecoverAfter {
			ev := *st.open
			end := st.okSince
			ev.EndedAt = &end
			closed, st.open = &ev, nil
		}
	}
	e.svc.mu.Unlock()

	if opened != nil {
		if err := e.st.InsertEvent(ctx, opened); err != nil {
			e.log.Error("storing event", "err", err)
		}
		e.log.Info("event opened", "kind", opened.Kind, "scope", opened.Scope, "error", last.err)
	}
	if closed != nil {
		if err := e.st.UpdateEvent(ctx, *closed); err != nil {
			e.log.Error("updating event", "err", err)
		}
		e.log.Info("event closed", "kind", closed.Kind, "scope", closed.Scope)
	}
}

// ipCheck looks up the public address and records a change.
func (e *Engine) ipCheck(ctx context.Context, now time.Time) {
	e.mu.RLock()
	outage := e.detState.State == detect.StateOutage
	v6 := e.info.IPv6Available
	e.mu.RUnlock()
	if outage {
		return
	}
	cur := model.PublicIP{TS: now.Unix()}
	if a, err := e.ipf.Fetch(ctx, 4); err == nil {
		cur.IPv4 = a.String()
	} else if ctx.Err() == nil {
		e.log.Warn("public IPv4 lookup failed", "err", err)
	}
	if v6 {
		if a, err := e.ipf.Fetch(ctx, 6); err == nil {
			cur.IPv6 = a.String()
		}
	}
	if cur.IPv4 == "" && cur.IPv6 == "" {
		return
	}
	prev, err := e.st.LatestPublicIP(ctx)
	if err != nil {
		e.log.Error("reading public IP", "err", err)
		return
	}
	// A family that failed this time keeps its previous value.
	if cur.IPv4 == "" {
		cur.IPv4 = prev.IPv4
	}
	if cur.IPv6 == "" {
		cur.IPv6 = prev.IPv6
	}
	if prev.TS != 0 && cur.IPv4 == prev.IPv4 && cur.IPv6 == prev.IPv6 {
		e.svc.mu.Lock()
		e.svc.pubIP = prev
		e.svc.mu.Unlock()
		return
	}
	if err := e.st.RecordPublicIP(ctx, cur); err != nil {
		e.log.Error("storing public IP", "err", err)
		return
	}
	e.svc.mu.Lock()
	e.svc.pubIP = cur
	e.svc.mu.Unlock()
	if prev.TS == 0 {
		e.log.Info("public IP", "ipv4", cur.IPv4, "ipv6", cur.IPv6)
		return
	}
	ts := now.Unix()
	ev := &model.Event{Kind: model.EventIPChange, Scope: "public", StartedAt: ts, EndedAt: &ts,
		Details: map[string]any{"old_ipv4": prev.IPv4, "new_ipv4": cur.IPv4, "old_ipv6": prev.IPv6, "new_ipv6": cur.IPv6}}
	if err := e.st.InsertEvent(ctx, ev); err != nil {
		e.log.Error("storing event", "err", err)
	}
	e.log.Info("public IP changed", "old", describeIP(prev), "new", describeIP(cur))
	e.alerts.PublicIPChanged(describeIP(prev), describeIP(cur), ts)
}

func describeIP(p model.PublicIP) string {
	switch {
	case p.IPv4 != "" && p.IPv6 != "":
		return p.IPv4 + " / " + p.IPv6
	case p.IPv4 != "":
		return p.IPv4
	}
	return p.IPv6
}

// PublicIP returns the latest known public address.
func (e *Engine) PublicIP() model.PublicIP {
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	return e.svc.pubIP
}

func toHops(hs []probe.Hop) []model.Hop {
	out := make([]model.Hop, 0, len(hs))
	for _, h := range hs {
		m := model.Hop{TTL: h.TTL, Reached: h.Reached}
		if h.Addr.IsValid() {
			m.Addr = h.Addr.String()
			m.RTTMs = roundV(h.RTT.Seconds() * 1000)
		}
		out = append(out, m)
	}
	return out
}

// traceOutage records the route at the start of an outage: how far packets
// still get says where the break is.
func (e *Engine) traceOutage(ctx context.Context, eventID int64) {
	if e.p4 == nil || !e.p4.Privileged() {
		return
	}
	var dst model.Target
	for _, t := range e.Targets() {
		if t.Role == model.RoleInternet && t.Family == model.FamilyV4 {
			dst = t
			break
		}
	}
	if !dst.Addr.IsValid() {
		return
	}
	hops, err := e.p4.Trace(ctx, dst.Addr, traceHops, 2*time.Second)
	if err != nil {
		// Sending can fail outright when the local network is down; keep
		// whatever hops were probed before that.
		e.log.Warn("route trace at outage start incomplete", "err", err, "hops", len(hops))
		if len(hops) == 0 {
			return
		}
	}
	tr := &model.Trace{TS: time.Now().Unix(), Reason: "outage", EventID: &eventID, Dst: dst.Addr.String(), Hops: toHops(hops)}
	if err := e.st.InsertTrace(ctx, tr); err != nil {
		e.log.Error("storing trace", "err", err)
		return
	}
	summary := map[string]any{"trace_id": tr.ID}
	for _, h := range tr.Hops {
		if h.Reached {
			summary["trace_reached"] = true
		}
		if h.Addr != "" && !h.Reached {
			summary["trace_last_hop"] = h.Addr
			summary["trace_last_ttl"] = h.TTL
		}
	}
	e.svc.mu.Lock()
	e.svc.traces[eventID] = summary
	e.svc.mu.Unlock()
	e.log.Info("route at outage start", "last_hop", summary["trace_last_hop"], "reached", summary["trace_reached"] == true)
}

// withTrace returns a copy of ev whose details include its outage trace
// summary (the detector's own maps are never modified).
func (e *Engine) withTrace(ev model.Event) model.Event {
	e.svc.mu.Lock()
	summary := e.svc.traces[ev.ID]
	delete(e.svc.traces, ev.ID)
	e.svc.mu.Unlock()
	if summary == nil {
		return ev
	}
	details := maps.Clone(ev.Details)
	if details == nil {
		details = map[string]any{}
	}
	maps.Copy(details, summary)
	ev.Details = details
	return ev
}

// ServiceStatus is a DNS or web target with its recent results.
type ServiceStatus struct {
	model.Target
	LastAt     int64             `json:"last_at"`
	LastOK     bool              `json:"last_ok"`
	LastMs     *float64          `json:"last_ms"`
	LastError  string            `json:"last_error,omitempty"`
	LastHTTP   *model.HTTPSample `json:"last_http,omitempty"`
	WindowS    int64             `json:"window_s"`
	Sent       int               `json:"sent"`
	SuccessPct *float64          `json:"success_pct"`
	Avg        *float64          `json:"avg_ms"`
	P95        *float64          `json:"p95_ms"`
}

const serviceWindow = 15 * 60

func (e *Engine) serviceStatus(t model.Target, now int64) ServiceStatus {
	s := ServiceStatus{Target: t, WindowS: serviceWindow}
	e.svc.mu.Lock()
	last, ok := e.svc.last[t.ID]
	e.svc.mu.Unlock()
	if ok {
		s.LastAt, s.LastOK, s.LastMs, s.LastError, s.LastHTTP = last.at, last.ok, last.ms, last.err, last.http
	}
	if ring := e.Ring(t.ID); ring != nil {
		sum := ring.Summarize(now-serviceWindow, now)
		s.Sent = sum.Sent
		if sum.Sent > 0 {
			s.SuccessPct = round(100 - sum.LossPct())
		}
		if sum.Recv > 0 {
			s.Avg, s.P95 = round(sum.Avg), round(sum.P95)
		}
	}
	return s
}
