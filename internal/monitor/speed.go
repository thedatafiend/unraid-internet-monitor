package monitor

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/detect"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/speedtest"
)

// maxJitter spreads scheduled tests so they don't all hit at the same second.
const maxJitter = 10 * time.Minute

func newSpeedRunner(cfg config.Config, e *Engine) *speedtest.Runner {
	sc := speedtest.DefaultConfig()
	sc.Duration = cfg.SpeedtestDuration
	sc.Streams = cfg.SpeedtestStreams
	sc.BaseURL = cfg.SpeedtestURL
	return speedtest.New(sc, e.speedPing, nil)
}

// speedPing measures latency to the first IPv4 internet target, the same
// path the test saturates.
func (e *Engine) speedPing(ctx context.Context) (time.Duration, bool) {
	if e.p4 == nil {
		return 0, false
	}
	var dst netip.Addr
	for _, t := range e.Targets() {
		if t.Role == model.RoleInternet && t.Family == model.FamilyV4 {
			dst = t.Addr
			break
		}
	}
	if !dst.IsValid() {
		return 0, false
	}
	r := e.p4.PingAll(ctx, []netip.Addr{dst}, time.Second)[0]
	return r.RTT, r.OK
}

// speedLoop runs the daily scheduled test.
func (e *Engine) speedLoop(ctx context.Context) {
	if e.cfg.SpeedtestSchedule == config.Off {
		return
	}
	for {
		next, err := speedtest.NextRun(time.Now(), e.cfg.SpeedtestSchedule)
		if err != nil {
			e.log.Error("speed test schedule", "err", err)
			return
		}
		next = next.Add(rand.N(maxJitter))
		if _, end, ok := e.cfg.RebootSchedule.Active(next); ok {
			next = end.Add(time.Minute) // don't measure a rebooting router
		}
		e.mu.Lock()
		e.nextSpeedtest = next.Unix()
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		e.mu.RLock()
		outage := e.detState.State == detect.StateOutage
		e.mu.RUnlock()
		if outage {
			e.log.Info("skipping scheduled speed test during an outage")
			continue
		}
		e.runSpeedtest(ctx, "scheduled")
	}
}

// RunSpeedtest starts a manual test in the background.
func (e *Engine) RunSpeedtest() error {
	if e.speed.Progress().Running {
		return speedtest.ErrBusy
	}
	e.mu.RLock()
	ctx := e.runCtx
	e.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go e.runSpeedtest(ctx, "manual")
	return nil
}

func (e *Engine) runSpeedtest(ctx context.Context, trigger string) {
	e.log.Info("speed test starting", "trigger", trigger)
	res, err := e.speed.Run(ctx, trigger)
	if err == speedtest.ErrBusy || ctx.Err() != nil {
		return
	}
	if serr := e.st.InsertSpeedTest(ctx, &res); serr != nil {
		e.log.Error("storing speed test", "err", serr)
	}
	attrs := []any{"down_mbps", deref(res.DownMbps), "up_mbps", deref(res.UpMbps), "idle_ms", deref(res.IdleMs),
		"grade", res.Grade, "server", res.Server, "mb_used", (res.BytesDown + res.BytesUp) >> 20}
	if err != nil {
		e.log.Warn("speed test failed", append(attrs, "err", err)...)
		return
	}
	e.log.Info("speed test finished", attrs...)
	e.alerts.SpeedTestResult(res, time.Now().Unix())
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// SpeedStatus summarises the speed test for /api/status.
type SpeedStatus struct {
	Last     *model.SpeedTest   `json:"last"`
	NextAt   int64              `json:"next_at,omitempty"`
	Progress speedtest.Progress `json:"progress"`
}

func (e *Engine) speedStatus(ctx context.Context) SpeedStatus {
	e.mu.RLock()
	next := e.nextSpeedtest
	e.mu.RUnlock()
	last, err := e.st.LatestSpeedTest(ctx)
	if err != nil {
		e.log.Warn("reading speed tests", "err", err)
	}
	return SpeedStatus{Last: last, NextAt: next, Progress: e.speed.Progress()}
}

// SpeedProgress returns the running test's progress.
func (e *Engine) SpeedProgress() speedtest.Progress { return e.speed.Progress() }
