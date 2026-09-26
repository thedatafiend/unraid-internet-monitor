package alert

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

const (
	maxAge       = 24 * time.Hour // undelivered alerts older than this are dropped
	pollEvery    = 5 * time.Second
	sendTimeout  = 15 * time.Second
	maxBackoff   = 5 * time.Minute
	firstBackoff = 5 * time.Second
)

// Config holds the noise-control settings.
type Config struct {
	MinOutage    time.Duration // shorter outages are recorded but not alerted
	Coalesce     time.Duration // alerts of one kind within this window are merged into a digest
	ISPHopChange bool          // alert when the ISP edge router changes
	IPChange     bool          // alert when the public IP address changes
}

// Sender delivers one message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Outbox persists queued alerts (implemented by store.Store).
type Outbox interface {
	EnqueueAlert(ctx context.Context, kind string, payload []byte, now int64) (int64, error)
	DueAlerts(ctx context.Context, now int64, limit int) ([]model.OutboxItem, error)
	MarkAlertSent(ctx context.Context, id, now int64) error
	RetryAlert(ctx context.Context, id int64, attempts int, next int64, errMsg string) error
	FailAlert(ctx context.Context, id int64, errMsg string) error
	SupersedeAlert(ctx context.Context, id int64) (bool, error)
}

// ErrDisabled is returned by Test when no notifier is configured.
var ErrDisabled = errors.New("alerts are disabled: set DISCORD_WEBHOOK_URL")

type digest struct {
	count       int
	total       int64 // seconds of downtime (outages only)
	first, last int64
	lastEventAt int64
}

// digested marks an event whose alert went into a digest.
const digested = -1

// Manager decides which events become alerts and delivers them.
type Manager struct {
	cfg    Config
	sender Sender
	ob     Outbox
	log    *slog.Logger
	now    func() time.Time
	wake   chan struct{}

	mu        sync.Mutex
	lastAlert map[string]int64 // group -> when its last immediate alert was queued
	digests   map[string]*digest
	tracked   map[int64]int64 // event ID -> outbox ID of its opening alert, or digested
}

// NewManager returns a manager. A nil sender disables alerts entirely.
func NewManager(cfg Config, sender Sender, ob Outbox, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, sender: sender, ob: ob, log: log, now: time.Now,
		wake:      make(chan struct{}, 1),
		lastAlert: make(map[string]int64),
		digests:   make(map[string]*digest),
		tracked:   make(map[int64]int64),
	}
}

// Enabled reports whether a notifier is configured.
func (m *Manager) Enabled() bool { return m.sender != nil }

// EventOpened is called when the detector opens an event.
func (m *Manager) EventOpened(ev model.Event, now int64) {
	if !m.Enabled() || ev.Kind != model.EventDegraded {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inWindow(model.EventDegraded, now) {
		m.addDigest(model.EventDegraded, ev.StartedAt, 0, now)
		m.tracked[ev.ID] = digested
		return
	}
	m.tracked[ev.ID] = m.enqueue(KindDegraded, degradedStarted(ev, now), now)
	m.lastAlert[model.EventDegraded] = now
}

// EventOngoing is called every second for each open event. An outage is
// announced once it has lasted MinOutage; if the internet is really down the
// alert waits in the outbox and is superseded by the recovery message.
func (m *Manager) EventOngoing(ev model.Event, now int64) {
	if !m.Enabled() || ev.Kind != model.EventOutage {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, done := m.tracked[ev.ID]; done || now-ev.StartedAt < m.minOutage() {
		return
	}
	if m.inWindow(model.EventOutage, now) {
		m.tracked[ev.ID] = digested // counted in the digest when it closes
		return
	}
	m.tracked[ev.ID] = m.enqueue(KindOutageStarted, outageStarted(ev, now), now)
	m.lastAlert[model.EventOutage] = now
}

// EventClosed is called when the detector closes an event.
func (m *Manager) EventClosed(ev model.Event, now int64) {
	if !m.Enabled() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	opened, tracked := m.tracked[ev.ID]
	delete(m.tracked, ev.ID)

	switch ev.Kind {
	case model.EventOutage:
		dur := ev.Duration(now)
		switch {
		case tracked && opened > 0:
			// Cancel the "started" alert if it never got out, then report the recovery.
			if _, err := m.ob.SupersedeAlert(context.Background(), opened); err != nil {
				m.log.Error("superseding alert", "err", err)
			}
			m.enqueue(KindOutageResolved, outageResolved(ev, now), now)
			m.lastAlert[model.EventOutage] = now
		case tracked && opened == digested:
			m.addDigest(model.EventOutage, ev.StartedAt, dur, now)
		case dur < m.minOutage():
			// too short to alert
		case m.inWindow(model.EventOutage, now):
			m.addDigest(model.EventOutage, ev.StartedAt, dur, now)
		default:
			m.enqueue(KindOutageResolved, outageResolved(ev, now), now)
			m.lastAlert[model.EventOutage] = now
		}
	case model.EventDegraded:
		if tracked && opened > 0 && !isSuperseded(ev) {
			m.enqueue(KindDegradedCleared, degradedCleared(ev, now), now)
		}
	}
}

func isSuperseded(ev model.Event) bool { v, _ := ev.Details["superseded"].(bool); return v }

// ISPHopChanged reports a new ISP edge router, when enabled.
func (m *Manager) ISPHopChanged(oldHop, newHop string, now int64) {
	if !m.Enabled() || !m.cfg.ISPHopChange {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueue(KindISPHopChange, ispHopChanged(oldHop, newHop, now), now)
}

// PublicIPChanged reports a new public address, when enabled.
func (m *Manager) PublicIPChanged(oldIP, newIP string, now int64) {
	if !m.Enabled() || !m.cfg.IPChange {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueue(KindIPChange, ipChanged(oldIP, newIP, now), now)
}

// Test queues a test message.
func (m *Manager) Test(now int64) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enqueue(KindTest, testMessage(now), now) == 0 {
		return errors.New("could not queue the test alert")
	}
	return nil
}

// Tick flushes digests whose coalescing window has passed.
func (m *Manager) Tick(now int64) {
	if !m.Enabled() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for group, d := range m.digests {
		if now-d.lastEventAt >= int64(m.cfg.Coalesce/time.Second) {
			m.enqueue(KindDigest, digestMessage(group, d, now), now)
			m.lastAlert[group] = now
			delete(m.digests, group)
		}
	}
}

func (m *Manager) minOutage() int64 { return int64(m.cfg.MinOutage / time.Second) }

func (m *Manager) inWindow(group string, now int64) bool {
	if _, pending := m.digests[group]; pending {
		return true
	}
	last, ok := m.lastAlert[group]
	return ok && now-last < int64(m.cfg.Coalesce/time.Second)
}

func (m *Manager) addDigest(group string, start, dur, now int64) {
	d := m.digests[group]
	if d == nil {
		d = &digest{first: start}
		m.digests[group] = d
	}
	d.count++
	d.total += dur
	d.last = start + dur
	d.lastEventAt = now
}

// enqueue stores msg in the outbox and wakes the sender. It returns the
// outbox ID, or 0 if storing failed.
func (m *Manager) enqueue(kind string, msg Message, now int64) int64 {
	payload, err := json.Marshal(msg)
	if err != nil {
		m.log.Error("encoding alert", "kind", kind, "err", err)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := m.ob.EnqueueAlert(ctx, kind, payload, now)
	if err != nil {
		m.log.Error("queueing alert", "kind", kind, "err", err)
		return 0
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return id
}

// Run delivers queued alerts until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		m.Deliver(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
	}
}

// Deliver sends every due alert in order. It stops at the first retryable
// failure so alerts are not reordered and a dead link is not hammered.
func (m *Manager) Deliver(ctx context.Context) {
	now := m.now()
	items, err := m.ob.DueAlerts(ctx, now.Unix(), 20)
	if err != nil {
		m.log.Error("reading alert outbox", "err", err)
		return
	}
	for _, it := range items {
		if now.Sub(time.Unix(it.CreatedAt, 0)) > maxAge {
			m.ob.FailAlert(ctx, it.ID, "expired before it could be delivered")
			m.log.Warn("dropped undelivered alert", "kind", it.Kind, "age", now.Sub(time.Unix(it.CreatedAt, 0)).Round(time.Minute))
			continue
		}
		var msg Message
		if err := json.Unmarshal(it.Payload, &msg); err != nil {
			m.ob.FailAlert(ctx, it.ID, "corrupt payload: "+err.Error())
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := m.sender.Send(sctx, msg)
		cancel()
		if err == nil {
			m.ob.MarkAlertSent(ctx, it.ID, m.now().Unix())
			m.log.Info("alert sent", "kind", it.Kind)
			continue
		}
		var perm *PermanentError
		if errors.As(err, &perm) {
			m.ob.FailAlert(ctx, it.ID, err.Error())
			m.log.Error("alert rejected; check DISCORD_WEBHOOK_URL", "kind", it.Kind, "err", err)
			continue
		}
		attempts := it.Attempts + 1
		delay := backoff(attempts)
		var ra *RetryAfterError
		if errors.As(err, &ra) {
			delay = max(ra.After, time.Second)
		}
		m.ob.RetryAlert(ctx, it.ID, attempts, now.Add(delay).Unix(), err.Error())
		if attempts == 1 {
			m.log.Warn("alert delivery failed, will retry", "kind", it.Kind, "err", err)
		}
		return
	}
}

func backoff(attempts int) time.Duration {
	d := firstBackoff
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}
