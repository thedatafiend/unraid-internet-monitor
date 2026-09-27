// Package speedtest measures download and upload throughput against
// Cloudflare's speed test endpoints, plus how much latency rises while the
// line is saturated (bufferbloat).
package speedtest

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// Config controls one test.
type Config struct {
	BaseURL    string        // e.g. https://speed.cloudflare.com
	Duration   time.Duration // per direction
	Streams    int           // parallel connections
	ChunkBytes int64         // bytes per request; streams loop until time is up
	PingEvery  time.Duration // latency sample interval during the test
	IdleFor    time.Duration // how long to sample idle latency first
}

// DefaultConfig is used in production.
func DefaultConfig() Config {
	return Config{
		BaseURL:    "https://speed.cloudflare.com",
		Duration:   10 * time.Second,
		Streams:    6,
		ChunkBytes: 25 << 20,
		PingEvery:  200 * time.Millisecond,
		IdleFor:    2 * time.Second,
	}
}

// Pinger measures one round trip for latency-under-load sampling.
type Pinger func(ctx context.Context) (time.Duration, bool)

// Progress is the state of a running test, for the UI.
type Progress struct {
	Running   bool    `json:"running"`
	Phase     string  `json:"phase,omitempty"` // latency | download | upload
	Mbps      float64 `json:"mbps"`            // throughput over the last second
	StartedAt int64   `json:"started_at,omitempty"`
}

// ErrBusy is returned when a test is already running.
var ErrBusy = errors.New("a speed test is already running")

// Runner runs one test at a time.
type Runner struct {
	cfg    Config
	ping   Pinger
	client *http.Client

	running atomic.Bool
	mu      sync.Mutex
	prog    Progress

	connMu sync.Mutex
	conns  map[*net.TCPConn]*ackedCount
}

// ackedCount tracks one connection's acknowledged bytes within a phase. last
// keeps the final value after the connection closes, so totals never drop.
type ackedCount struct{ base, last uint64 }

// New returns a runner. tlsConf may be nil (system roots).
func New(cfg Config, ping Pinger, tlsConf *tls.Config) *Runner {
	r := &Runner{cfg: cfg, ping: ping, conns: map[*net.TCPConn]*ackedCount{}}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy: nil, // measure this connection, not a proxy
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if tc, ok := c.(*net.TCPConn); ok && err == nil {
				r.connMu.Lock()
				r.conns[tc] = &ackedCount{} // a new connection has acked nothing yet
				r.connMu.Unlock()
			}
			return c, err
		},
		TLSClientConfig:     tlsConf,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: cfg.Streams,
		// HTTP/1.1 only, so every stream is its own TCP connection.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	r.client = &http.Client{Transport: tr}
	return r
}

// resetAcked starts a new phase: open connections' current totals become
// the baseline and closed ones are forgotten. It reports whether counting
// acknowledged bytes is supported on this platform.
func (r *Runner) resetAcked() bool {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	for c := range r.conns {
		acked, ok := bytesAcked(c)
		if !ok {
			delete(r.conns, c)
			continue
		}
		r.conns[c] = &ackedCount{base: acked, last: acked}
	}
	return ackedAvailable
}

// ackedSince sums bytes acknowledged by the peer since the phase began.
func (r *Runner) ackedSince() int64 {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	var total int64
	for c, a := range r.conns {
		if acked, ok := bytesAcked(c); ok && acked >= a.last {
			a.last = acked
		}
		total += int64(a.last - a.base)
	}
	return total
}

// Progress returns the current state.
func (r *Runner) Progress() Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prog
}

func (r *Runner) setProgress(f func(*Progress)) {
	r.mu.Lock()
	f(&r.prog)
	r.mu.Unlock()
}

// Run performs a full test: idle latency, download, upload.
func (r *Runner) Run(ctx context.Context, trigger string) (model.SpeedTest, error) {
	if !r.running.CompareAndSwap(false, true) {
		return model.SpeedTest{}, ErrBusy
	}
	defer r.running.Store(false)
	start := time.Now()
	res := model.SpeedTest{TS: start.Unix(), Trigger: trigger}
	r.setProgress(func(p *Progress) { *p = Progress{Running: true, Phase: "latency", StartedAt: start.Unix()} })
	defer r.setProgress(func(p *Progress) { *p = Progress{} })

	res.Server = r.colo(ctx)
	idle := r.sampleLatency(ctx, r.cfg.IdleFor)
	res.IdleMs = medianMs(idle)

	down, err := r.measure(ctx, "download", r.download, nil)
	res.BytesDown = down.bytes
	res.DownMbps, res.LoadedDownMs = mbps(down.bps), medianMs(down.rtts)
	if err != nil {
		res.Error = "download: " + err.Error()
	}
	if ctx.Err() == nil {
		// Upload counts bytes the server acknowledged where the kernel tells
		// us; bytes handed to the socket overstate it by the send buffers.
		var total func(*atomic.Int64) int64
		if r.resetAcked() {
			total = func(*atomic.Int64) int64 { return r.ackedSince() }
		}
		up, err := r.measure(ctx, "upload", r.upload, total)
		res.BytesUp = up.bytes
		res.UpMbps, res.LoadedUpMs = mbps(up.bps), medianMs(up.rtts)
		if err != nil && res.Error == "" {
			res.Error = "upload: " + err.Error()
		}
	}
	res.Grade = Grade(res.IdleMs, res.LoadedDownMs, res.LoadedUpMs)
	res.DurationS = math.Round(time.Since(start).Seconds()*10) / 10
	if res.DownMbps == nil && res.UpMbps == nil {
		if res.Error == "" {
			res.Error = "no data transferred"
		}
		return res, errors.New(res.Error)
	}
	return res, nil
}

// colo asks Cloudflare which data centre served us (best effort).
func (r *Runner) colo(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.BaseURL+"/cdn-cgi/trace", nil)
	if err != nil {
		return ""
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 4096))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "colo="); ok {
			return v
		}
	}
	return ""
}

type rtt struct {
	at time.Duration
	d  time.Duration
}

// sampleLatency pings every PingEvery for d.
func (r *Runner) sampleLatency(ctx context.Context, d time.Duration) []time.Duration {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var out []time.Duration
	for _, s := range r.pingLoop(ctx, time.Now()) {
		out = append(out, s.d)
	}
	return out
}

// pingLoop samples latency until ctx ends; timestamps are relative to start.
func (r *Runner) pingLoop(ctx context.Context, start time.Time) []rtt {
	if r.ping == nil {
		<-ctx.Done()
		return nil
	}
	var out []rtt
	t := time.NewTicker(r.cfg.PingEvery)
	defer t.Stop()
	for {
		if d, ok := r.ping(ctx); ok && ctx.Err() == nil {
			out = append(out, rtt{at: time.Since(start), d: d})
		}
		select {
		case <-ctx.Done():
			return out
		case <-t.C:
		}
	}
}

type phaseResult struct {
	bps   float64
	bytes int64
	rtts  []time.Duration
}

// measure runs Streams copies of worker for Duration while sampling bytes
// and latency. Throughput ignores the first fifth (TCP slow start).
// total, if set, reports bytes transferred so far instead of the counter.
func (r *Runner) measure(ctx context.Context, phase string, worker func(context.Context, *atomic.Int64) error, total func(*atomic.Int64) int64) (phaseResult, error) {
	if total == nil {
		total = func(c *atomic.Int64) int64 { return c.Load() }
	}
	r.setProgress(func(p *Progress) { p.Phase, p.Mbps = phase, 0 })
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Duration)
	defer cancel()
	start := time.Now()
	var counter atomic.Int64
	var firstErr error
	var errOnce sync.Once

	var wg sync.WaitGroup
	for range r.cfg.Streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := worker(ctx, &counter); err != nil && ctx.Err() == nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
			}
		}()
	}
	var pings []rtt
	wg.Add(1)
	go func() { defer wg.Done(); pings = r.pingLoop(ctx, start) }()

	type sample struct {
		at    time.Duration
		bytes int64
	}
	samples := []sample{{0, 0}}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			s := sample{time.Since(start), total(&counter)}
			samples = append(samples, s)
			// Live rate over the last second for the progress display.
			back := samples[max(0, len(samples)-11)]
			if dt := (s.at - back.at).Seconds(); dt > 0 {
				rate := float64(s.bytes-back.bytes) * 8 / dt / 1e6
				r.setProgress(func(p *Progress) { p.Mbps = math.Round(rate*10) / 10 })
			}
		}
	}
	end := sample{time.Since(start), total(&counter)} // before workers tear down connections
	wg.Wait()

	warm := r.cfg.Duration / 5
	from := samples[0]
	for _, s := range samples {
		if s.at >= warm {
			from = s
			break
		}
	}
	res := phaseResult{bytes: end.bytes}
	if dt := (end.at - from.at).Seconds(); dt > 0 && end.bytes > from.bytes {
		res.bps = float64(end.bytes-from.bytes) * 8 / dt
	}
	for _, p := range pings {
		if p.at >= warm {
			res.rtts = append(res.rtts, p.d)
		}
	}
	if res.bytes == 0 && firstErr == nil {
		firstErr = errors.New("no data transferred")
	}
	return res, firstErr
}

func (r *Runner) download(ctx context.Context, counter *atomic.Int64) error {
	url := fmt.Sprintf("%s/__down?bytes=%d", r.cfg.BaseURL, r.cfg.ChunkBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		counter.Add(int64(n))
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (r *Runner) upload(ctx context.Context, counter *atomic.Int64) error {
	body := &countingReader{remaining: r.cfg.ChunkBytes, counter: counter}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL+"/__up", body)
	if err != nil {
		return err
	}
	req.ContentLength = r.cfg.ChunkBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// payload is incompressible, so no middlebox can shrink the upload.
var payload = func() []byte {
	b := make([]byte, 1<<20)
	rng := rand.NewChaCha8([32]byte{1})
	rng.Read(b)
	return b
}()

type countingReader struct {
	remaining int64
	offset    int
	counter   *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(int64(len(p)), c.remaining)], payload[c.offset:])
	c.offset = (c.offset + n) % len(payload)
	c.remaining -= int64(n)
	c.counter.Add(int64(n))
	return n, nil
}

func mbps(bps float64) *float64 {
	if bps <= 0 {
		return nil
	}
	v := math.Round(bps/1e5) / 10
	return &v
}

func medianMs(ds []time.Duration) *float64 {
	if len(ds) == 0 {
		return nil
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	v := math.Round(m.Seconds()*10000) / 10
	return &v
}

// Grade rates bufferbloat by how much latency rises under load (the worse
// of download and upload), on the same scale as the Waveform test.
func Grade(idle, loadedDown, loadedUp *float64) string {
	if idle == nil || (loadedDown == nil && loadedUp == nil) {
		return ""
	}
	inc := 0.0
	for _, l := range []*float64{loadedDown, loadedUp} {
		if l != nil {
			inc = max(inc, *l-*idle)
		}
	}
	switch {
	case inc < 5:
		return "A+"
	case inc < 30:
		return "A"
	case inc < 60:
		return "B"
	case inc < 200:
		return "C"
	case inc < 400:
		return "D"
	}
	return "F"
}

// NextRun returns the next time at hh:mm local time strictly after now.
func NextRun(now time.Time, hhmm string) (time.Time, error) {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("want HH:MM, got %q", hhmm)
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return next, nil
}
