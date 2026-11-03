package monitor

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/detect"
	"github.com/thedatafiend/unraid-internet-monitor/internal/discover"
	"github.com/thedatafiend/unraid-internet-monitor/internal/upnp"
)

const (
	routerPollTimeout = 4 * time.Second
	routerFails       = 3                // failed readings in a row before the router is looked up again
	routerRetryLost   = 15 * time.Second // first retry when a router that was found stops answering
	routerRetryMin    = time.Minute      // first retry when no router was ever found
	routerRetryMax    = 30 * time.Minute
)

// RouterStatus is the router's own report of its WAN connection, read over
// UPnP. It is what the router sees on the port the ONT or modem plugs into.
type RouterStatus struct {
	Mode         string   `json:"mode"` // auto | off | url
	Found        bool     `json:"found"`
	Address      string   `json:"address,omitempty"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Server       string   `json:"server,omitempty"`
	Location     string   `json:"location,omitempty"`
	LastAt       int64    `json:"last_at,omitempty"` // last successful reading
	WANUp        *bool    `json:"wan_up"`
	LinkStatus   string   `json:"link_status,omitempty"`
	ConnStatus   string   `json:"connection_status,omitempty"`
	UptimeS      *int64   `json:"wan_uptime_s,omitempty"`
	ExternalIP   string   `json:"external_ip,omitempty"`
	RxBytes      *uint64  `json:"rx_bytes,omitempty"`
	TxBytes      *uint64  `json:"tx_bytes,omitempty"`
	RxBps        *float64 `json:"rx_bps"` // bits per second over the last two readings
	TxBps        *float64 `json:"tx_bps"`
	Error        string   `json:"error,omitempty"`
}

// router is the engine's UPnP connection to the gateway and its latest reading.
type router struct {
	mu        sync.Mutex
	dev       *upnp.Device
	status    RouterStatus
	lastOK    int64 // when the last good reading was taken
	prevAt    int64 // counters of the previous reading, for rates
	prevRx    *uint64
	prevTx    *uint64
	fails     int
	retry     time.Duration
	nextFind  time.Time
	everFound bool
	loggedUp  *bool // WAN state last written to the log
}

func newRouter(cfg config.Config) *router {
	mode := cfg.RouterUPnP
	if mode != config.Auto && mode != config.Off {
		mode = "url"
	}
	return &router{status: RouterStatus{Mode: mode}}
}

// routerLoop reads the router's WAN status every RouterInterval.
func (e *Engine) routerLoop(ctx context.Context) {
	if e.cfg.RouterUPnP == config.Off {
		return
	}
	every(ctx, e.cfg.RouterInterval, func(now time.Time) { e.routerRound(ctx, now) })
}

func (e *Engine) routerRound(ctx context.Context, now time.Time) {
	r := e.rt
	r.mu.Lock()
	dev, due := r.dev, !now.Before(r.nextFind)
	r.mu.Unlock()
	if dev == nil {
		if !due {
			return
		}
		d, err := e.findRouter(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			e.routerNotFound(now, err)
			return
		}
		e.routerFound(d)
		dev = d
	}
	pctx, cancel := context.WithTimeout(ctx, routerPollTimeout)
	s, err := dev.Status(pctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	e.routerReading(now, s, err)
}

func (e *Engine) findRouter(ctx context.Context) (*upnp.Device, error) {
	return FindRouter(ctx, e.cfg, e.Info().Gateway)
}

// FindRouter asks the gateway for its UPnP description, or opens the one
// configured in ROUTER_UPNP.
func FindRouter(ctx context.Context, cfg config.Config, gateway string) (*upnp.Device, error) {
	if cfg.RouterUPnP != config.Auto {
		return upnp.Open(ctx, cfg.RouterUPnP)
	}
	gw, err := netip.ParseAddr(gateway)
	if err != nil {
		return nil, errors.New("no gateway was found to ask")
	}
	var opt upnp.Options
	if rt, err := discover.RouteTo(gw); err == nil {
		opt.Iface = rt.Iface // search on the LAN even when a VPN carries the default route
	}
	return upnp.Discover(ctx, gw, opt)
}

func (e *Engine) routerNotFound(now time.Time, err error) {
	r := e.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.retry == 0 && r.everFound:
		r.retry = routerRetryLost
	case r.retry == 0:
		r.retry = routerRetryMin
	default:
		r.retry = min(2*r.retry, routerRetryMax)
	}
	r.nextFind = now.Add(r.retry)
	msg := shortErr(err)
	if msg != r.status.Error {
		e.log.Info("router WAN status not available over UPnP; turn on UPnP in the router, or set ROUTER_UPNP=off to stop looking",
			"err", msg, "retry_in", r.retry)
	}
	r.status.Found, r.status.Error = false, msg
}

func (e *Engine) routerFound(d *upnp.Device) {
	r := e.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dev, r.fails, r.retry, r.everFound = d, 0, 0, true
	s := &r.status
	s.Found, s.Error = true, ""
	s.Manufacturer, s.Model, s.Server, s.Location = d.Manufacturer, d.Model, d.Server, d.Location
	s.Address = ""
	if d.Addr.IsValid() {
		s.Address = d.Addr.String()
	}
	e.log.Info("reading the router's WAN status over UPnP", "router", d.Manufacturer+" "+d.Model, "location", d.Location)
}

// routerReading records one poll. Repeated failures drop the device so it
// is looked up again: routers often restart their UPnP service (with new
// URLs) when the WAN reconnects.
func (e *Engine) routerReading(now time.Time, s upnp.Status, err error) {
	r := e.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	st := &r.status
	if err != nil {
		r.fails++
		st.Error = shortErr(err)
		if r.fails >= routerFails {
			e.log.Warn("router stopped answering UPnP; looking for it again", "err", st.Error)
			r.dev, r.fails, r.retry, r.nextFind = nil, 0, 0, now
			st.Found = false
		}
		return
	}
	ts := now.Unix()
	r.fails, r.lastOK = 0, ts
	st.Error, st.LastAt = "", ts
	st.LinkStatus, st.ConnStatus, st.UptimeS, st.ExternalIP = s.LinkStatus, s.ConnStatus, s.UptimeS, s.ExternalIP
	st.WANUp = s.Up()
	rate := func(cur, prev *uint64) *float64 {
		if cur == nil || prev == nil || *cur < *prev || ts <= r.prevAt {
			return nil // missing, or the counter wrapped or reset
		}
		v := roundV(float64(*cur-*prev) * 8 / float64(ts-r.prevAt))
		return &v
	}
	st.RxBps, st.TxBps = rate(s.RxBytes, r.prevRx), rate(s.TxBytes, r.prevTx)
	st.RxBytes, st.TxBytes = s.RxBytes, s.TxBytes
	r.prevRx, r.prevTx, r.prevAt = s.RxBytes, s.TxBytes, ts

	if up := st.WANUp; up != nil && (r.loggedUp == nil || *r.loggedUp != *up) {
		switch {
		case r.loggedUp == nil:
		case *up:
			e.log.Info("router reports its WAN port up again", "wan_ip", s.ExternalIP)
		default:
			e.log.Warn("router reports its WAN port down", "link", s.LinkStatus, "connection", s.ConnStatus, "wan_ip", s.ExternalIP)
		}
		r.loggedUp = up
	}
}

// wanObservation is the router's latest WAN reading for the detector, if it
// is recent enough to describe second ts.
func (e *Engine) wanObservation(ts int64) detect.WAN {
	r := e.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	w := detect.WAN{Enabled: r.everFound}
	fresh := int64((2*e.cfg.RouterInterval + routerPollTimeout) / time.Second)
	if r.lastOK == 0 || ts-r.lastOK > fresh {
		return w
	}
	w.At = r.lastOK
	if up := r.status.WANUp; up != nil {
		w.Known, w.Up = true, *up
	}
	if r.status.RxBytes != nil {
		w.HasRx, w.RxBytes = true, *r.status.RxBytes
	}
	return w
}

// RouterStatus returns the router's latest WAN reading.
func (e *Engine) RouterStatus() RouterStatus {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.rt.status
}
