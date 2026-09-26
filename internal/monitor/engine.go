// Package monitor runs the probe schedule and keeps live state.
package monitor

import (
	"context"
	"log/slog"
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/aggregate"
	"github.com/thedatafiend/unraid-internet-monitor/internal/alert"
	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/detect"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

const (
	ringSeconds       = 3600
	flushEvery        = 10 * time.Second
	discoverEvery     = time.Hour
	retentionEvery    = time.Hour
	statusWindow      = 60 // seconds summarised per target in Status
	shutdownFlushWait = 5 * time.Second
)

// Engine owns the probe loop and the in-memory state.
type Engine struct {
	cfg     config.Config
	p4, p6  *probe.Pinger
	st      *store.Store
	log     *slog.Logger
	minutes *aggregate.Minutes
	alerts  *alert.Manager
	det     *detect.Detector // owned by detectLoop
	ticks   chan detect.Tick
	live    *hub
	svc     *services
	http    *probe.HTTPProber
	ipf     *probe.IPFetcher

	lastTick atomic.Int64

	mu       sync.RWMutex
	targets  []model.Target
	rings    map[int64]*aggregate.Ring
	info     Info
	detState detect.Snapshot
}

// New creates an engine. p6 may be nil.
func New(cfg config.Config, p4, p6 *probe.Pinger, st *store.Store, alerts *alert.Manager, log *slog.Logger) *Engine {
	grace := int64(cfg.PingTimeout/time.Second) + 2
	return &Engine{
		cfg: cfg, p4: p4, p6: p6, st: st, log: log, alerts: alerts,
		minutes:  aggregate.NewMinutes(grace),
		det:      detect.New(detectConfig(cfg)),
		ticks:    make(chan detect.Tick, 16),
		live:     newHub(),
		svc:      newServices(),
		http:     probe.NewHTTPProber(cfg.HTTPTimeout),
		ipf:      probe.DefaultIPFetcher(),
		rings:    make(map[int64]*aggregate.Ring),
		detState: detect.Snapshot{State: detect.StateUnknown},
	}
}

// Run probes until ctx is cancelled, then flushes pending rollups.
func (e *Engine) Run(ctx context.Context) error {
	if n, err := e.st.CloseDanglingEvents(ctx); err != nil {
		return err
	} else if n > 0 {
		e.log.Info("closed events left open by the previous run", "count", n)
	}
	if err := e.rediscover(ctx); err != nil {
		return err
	}
	e.prune(ctx)

	if p, err := e.st.LatestPublicIP(ctx); err == nil {
		e.svc.pubIP = p
	}

	var wg sync.WaitGroup
	loops := []func(){
		func() { e.pingLoop(ctx) },
		func() { e.detectLoop(ctx) },
		func() { every(ctx, e.cfg.DNSInterval, func(t time.Time) { e.dnsRound(ctx, t) }) },
		func() { every(ctx, e.cfg.HTTPInterval, func(t time.Time) { e.httpRound(ctx, t) }) },
		func() {
			select { // let discovery and the first pings settle
			case <-time.After(firstIPCheck):
			case <-ctx.Done():
				return
			}
			every(ctx, e.cfg.PublicIPInterval, func(t time.Time) { e.ipCheck(ctx, t) })
		},
	}
	wg.Add(len(loops))
	for _, l := range loops {
		go func() { defer wg.Done(); l() }()
	}

	flush := time.NewTicker(flushEvery)
	disc := time.NewTicker(discoverEvery)
	ret := time.NewTicker(retentionEvery)
	defer flush.Stop()
	defer disc.Stop()
	defer ret.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			fctx, cancel := context.WithTimeout(context.Background(), shutdownFlushWait)
			defer cancel()
			e.flush(fctx, true)
			return nil
		case <-flush.C:
			e.flush(ctx, false)
		case <-disc.C:
			if err := e.rediscover(ctx); err != nil {
				e.log.Error("rediscovery failed", "err", err)
			}
		case <-ret.C:
			e.prune(ctx)
		}
	}
}

func (e *Engine) rediscover(ctx context.Context) error {
	targets, info := Discover(ctx, e.cfg, e.p4, e.p6)
	for _, w := range info.Warnings {
		e.log.Warn(w)
	}
	if len(info.Trace) > 0 {
		dst := ""
		for _, t := range targets {
			if t.Role == model.RoleInternet && t.Family == model.FamilyV4 {
				dst = t.Addr.String()
				break
			}
		}
		tr := &model.Trace{TS: time.Now().Unix(), Reason: "discovery", Dst: dst, Hops: toHops(info.Trace)}
		if err := e.st.InsertTrace(ctx, tr); err != nil {
			e.log.Error("storing trace", "err", err)
		}
	}
	stored, err := e.st.Targets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		for _, old := range stored {
			if t.Role == model.RoleISP && old.Key == t.Key {
				e.recordISPHopChange(ctx, old, t)
			}
		}
	}
	ids := make([]int64, 0, len(targets))
	for i := range targets {
		id, err := e.st.UpsertTarget(ctx, targets[i])
		if err != nil {
			return err
		}
		targets[i].ID = id
		ids = append(ids, id)
	}
	if err := e.st.DisableTargetsExcept(ctx, ids); err != nil {
		return err
	}

	e.mu.Lock()
	old := e.targets
	for _, t := range targets {
		if e.rings[t.ID] == nil {
			e.rings[t.ID] = aggregate.NewRing(ringSeconds)
		}
	}
	e.targets, e.info = targets, info
	e.mu.Unlock()

	if changed(old, targets) {
		attrs := []any{"count", len(targets), "gateway", info.Gateway, "isp_hop", info.ISPHop,
			"ipv6", info.IPv6Available, "socket_v4", info.SocketV4}
		e.log.Info("targets updated", attrs...)
	}
	return nil
}

func changed(a, b []model.Target) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Addr != b[i].Addr {
			return true
		}
	}
	return false
}

func (e *Engine) pingLoop(ctx context.Context) {
	// Align ticks to whole seconds so every target shares timestamps.
	interval := e.cfg.PingInterval
	select {
	case <-time.After(time.Until(time.Now().Truncate(time.Second).Add(time.Second))):
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var inflight sync.WaitGroup
	defer inflight.Wait()
	tick := func(at time.Time) {
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			e.probeOnce(ctx, at.Truncate(time.Second).Unix())
		}()
	}
	tick(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			tick(t)
		}
	}
}

// probeOnce pings every enabled target once, records the results at ts and
// passes the round to the outage detector.
func (e *Engine) probeOnce(ctx context.Context, ts int64) {
	targets := e.Targets()
	var wg sync.WaitGroup
	var mu sync.Mutex
	tick := detect.Tick{TS: ts}
	rtts := make(map[int64]*float64, len(targets))
	for _, fam := range []struct {
		name string
		p    *probe.Pinger
	}{{model.FamilyV4, e.p4}, {model.FamilyV6, e.p6}} {
		var group []model.Target
		var addrs []netip.Addr
		for _, t := range targets {
			if t.Kind == model.KindICMP && t.Family == fam.name {
				group = append(group, t)
				addrs = append(addrs, t.Addr)
			}
		}
		if len(group) == 0 || fam.p == nil {
			continue
		}
		wg.Add(1)
		go func(p *probe.Pinger) {
			defer wg.Done()
			results := p.PingAll(ctx, addrs, e.cfg.PingTimeout)
			if ctx.Err() != nil {
				return // shutting down: a cancelled wait is not packet loss
			}
			mu.Lock()
			defer mu.Unlock()
			for i, r := range results {
				t := group[i]
				e.record(t.ID, ts, r)
				ms := r.RTT.Seconds() * 1000
				tick.Samples = append(tick.Samples, detect.Sample{
					TargetID: t.ID, Name: t.Name, Role: t.Role, Family: t.Family,
					OK: r.OK, RTT: ms,
				})
				rtts[t.ID] = nil
				if r.OK {
					rtts[t.ID] = &ms
				}
			}
		}(fam.p)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	e.lastTick.Store(time.Now().Unix())
	e.publishLive(ts, rtts)
	e.sendTick(ctx, tick)
}

func (e *Engine) record(targetID, ts int64, r probe.Result) {
	pt := aggregate.Point{TS: ts, OK: r.OK}
	if r.OK {
		pt.RTT = float32(r.RTT.Seconds() * 1000)
	}
	e.mu.RLock()
	ring := e.rings[targetID]
	e.mu.RUnlock()
	if ring != nil {
		ring.Put(pt)
	}
	e.minutes.Add(targetID, pt)
}

func (e *Engine) flush(ctx context.Context, force bool) {
	stats := e.minutes.Collect(time.Now().Unix(), force)
	if err := e.st.WriteMinutes(ctx, stats); err != nil {
		e.log.Error("writing rollups failed", "rows", len(stats), "err", err)
	}
}

func (e *Engine) prune(ctx context.Context) {
	cutoff := time.Now().Add(-time.Duration(e.cfg.RetentionDays) * 24 * time.Hour).Unix()
	n, err := e.st.Prune(ctx, cutoff)
	if err != nil {
		e.log.Error("retention prune failed", "err", err)
		return
	}
	if n > 0 {
		e.log.Info("retention pruned rollups", "rows", n)
	}
}

// Targets returns the currently probed targets.
func (e *Engine) Targets() []model.Target {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]model.Target(nil), e.targets...)
}

// Ring returns the live buffer for a target, or nil.
func (e *Engine) Ring(id int64) *aggregate.Ring {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rings[id]
}

// Info returns the latest discovery results.
func (e *Engine) Info() Info {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.info
}

// LastTick returns when the last probe round completed (zero if never).
func (e *Engine) LastTick() time.Time {
	if ts := e.lastTick.Load(); ts != 0 {
		return time.Unix(ts, 0)
	}
	return time.Time{}
}

// TargetStatus is a target plus its recent live statistics.
type TargetStatus struct {
	model.Target
	LastAt  int64    `json:"last_at"`
	LastOK  bool     `json:"last_ok"`
	LastRTT *float64 `json:"last_rtt"`
	Sent    int      `json:"sent_60s"`
	LossPct float64  `json:"loss_pct_60s"`
	Avg     *float64 `json:"avg_60s"`
	P95     *float64 `json:"p95_60s"`
	Jitter  *float64 `json:"jitter_60s"`
	MOS     *float64 `json:"mos_60s"`
}

// Status is the live overview served by /api/status.
type Status struct {
	Now        int64           `json:"now"`
	State      string          `json:"state"` // unknown | online | degraded | outage
	Since      int64           `json:"since"`
	OpenEvents []model.Event   `json:"open_events"`
	MOS        *float64        `json:"mos"`
	Targets    []TargetStatus  `json:"targets"`  // ping targets
	Services   []ServiceStatus `json:"services"` // DNS and web targets
	PublicIP   model.PublicIP  `json:"public_ip"`
	Info       Info            `json:"info"`
}

// Status summarises the last minute of live data.
func (e *Engine) Status(now time.Time) Status {
	// Look at the last fully completed second: the current one may still be in flight.
	end := now.Unix() - int64(e.cfg.PingTimeout/time.Second) - 1
	e.mu.RLock()
	snap := e.detState
	e.mu.RUnlock()
	st := Status{Now: now.Unix(), State: snap.State, Since: snap.Since, OpenEvents: snap.Open, Info: e.Info(),
		PublicIP: e.PublicIP(), Targets: []TargetStatus{}, Services: []ServiceStatus{}}
	if st.OpenEvents == nil {
		st.OpenEvents = []model.Event{}
	}

	var mosSum float64
	var mosN int
	for _, t := range e.Targets() {
		if t.Kind != model.KindICMP {
			st.Services = append(st.Services, e.serviceStatus(t, now.Unix()))
			continue
		}
		ring := e.Ring(t.ID)
		if ring == nil {
			continue
		}
		ts := TargetStatus{Target: t}
		for _, p := range ring.Range(end-statusWindow+1, now.Unix()) {
			if p.TS > ts.LastAt {
				ts.LastAt, ts.LastOK = p.TS, p.OK
				ts.LastRTT = nil
				if p.OK {
					ts.LastRTT = round(float64(p.RTT))
				}
			}
		}
		s := ring.Summarize(end-statusWindow+1, end)
		ts.Sent, ts.LossPct = s.Sent, roundV(s.LossPct())
		if s.Recv > 0 {
			ts.Avg, ts.P95, ts.Jitter = round(s.Avg), round(s.P95), round(s.Jitter)
			ts.MOS = round(aggregate.MOS(s.Avg, s.Jitter, s.LossPct()))
		}
		if t.Role == model.RoleInternet {
			if ts.MOS != nil {
				mosSum += *ts.MOS
				mosN++
			} else if s.Sent > 0 {
				mosN++ // total loss counts as the floor score
				mosSum++
			}
		}
		st.Targets = append(st.Targets, ts)
	}
	if mosN > 0 {
		st.MOS = round(mosSum / float64(mosN))
	}
	return st
}

func roundV(v float64) float64 { return math.Round(v*1000) / 1000 }

func round(v float64) *float64 {
	r := roundV(v)
	return &r
}
