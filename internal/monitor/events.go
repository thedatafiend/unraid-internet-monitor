package monitor

import (
	"context"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/detect"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// detectConfig derives detector thresholds from the runtime config.
func detectConfig(c config.Config) detect.Config {
	d := detect.DefaultConfig()
	d.OutageAfter = max(1, int((c.OutageThreshold+c.PingInterval-1)/c.PingInterval))
	d.DegradedLossPct = c.DegradedLossPct
	d.DegradedP95Ms = c.DegradedP95Ms
	d.DegradedOpenAfter = int64(c.DegradedMin / time.Second)
	return d
}

// detectLoop owns the detector: it receives completed ticks, processes them
// in time order once no earlier tick can still arrive, persists events and
// hands them to the alert manager.
func (e *Engine) detectLoop(ctx context.Context) {
	// A tick is complete at most PingTimeout after it started.
	delay := int64(e.cfg.PingTimeout/time.Second) + 1
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-e.ticks:
			e.det.Add(tick)
		case now := <-t.C:
			e.advance(ctx, now.Unix(), delay)
		}
	}
}

func (e *Engine) advance(ctx context.Context, now, delay int64) {
	for _, tr := range e.det.Advance(now - delay) {
		ev := tr.Event
		if tr.Open {
			if err := e.st.InsertEvent(ctx, ev); err != nil {
				e.log.Error("storing event", "kind", ev.Kind, "err", err)
			}
			e.log.Info("event opened", "kind", ev.Kind, "scope", ev.Scope, "class", ev.Class, "started", time.Unix(ev.StartedAt, 0))
			e.alerts.EventOpened(*ev, now)
			if ev.Kind == model.EventOutage && ev.Scope == model.FamilyV4 && ev.ID != 0 {
				go e.traceOutage(ctx, ev.ID)
			}
			continue
		}
		closed := e.withTrace(*ev)
		if err := e.st.UpdateEvent(ctx, closed); err != nil {
			e.log.Error("updating event", "kind", ev.Kind, "err", err)
		}
		e.log.Info("event closed", "kind", ev.Kind, "scope", ev.Scope, "class", ev.Class, "duration", time.Duration(ev.Duration(now))*time.Second)
		e.alerts.EventClosed(closed, now)
	}

	snap := e.det.Snapshot()
	for _, ev := range snap.Open {
		e.alerts.EventOngoing(ev, now)
	}
	e.alerts.Tick(now)

	e.mu.Lock()
	e.detState = snap
	e.mu.Unlock()
}

// sendTick hands a finished probe round to the detector loop.
func (e *Engine) sendTick(ctx context.Context, t detect.Tick) {
	select {
	case e.ticks <- t:
	case <-ctx.Done():
	}
}

// recordISPHopChange logs an event (and optionally alerts) when the ISP
// edge router address differs from the one stored last time.
func (e *Engine) recordISPHopChange(ctx context.Context, prev, next model.Target) {
	if !prev.Addr.IsValid() || !next.Addr.IsValid() || prev.Addr == next.Addr {
		return
	}
	now := time.Now().Unix()
	ev := &model.Event{
		Kind: model.EventISPHopChange, Scope: next.Family, StartedAt: now, EndedAt: &now,
		Details: map[string]any{"old": prev.Addr.String(), "new": next.Addr.String()},
	}
	if err := e.st.InsertEvent(ctx, ev); err != nil {
		e.log.Error("storing event", "kind", ev.Kind, "err", err)
	}
	e.log.Info("ISP edge router changed", "old", prev.Addr, "new", next.Addr)
	e.alerts.ISPHopChanged(prev.Addr.String(), next.Addr.String(), now)
}
