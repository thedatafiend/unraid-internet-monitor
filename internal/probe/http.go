package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"time"
)

// HTTPResult is one web request broken into phases. A zero phase was not
// measured (e.g. no TLS for http://, or the request failed first).
type HTTPResult struct {
	OK      bool
	Status  int
	DNS     time.Duration
	Connect time.Duration
	TLS     time.Duration
	TTFB    time.Duration // request written -> first response byte
	Total   time.Duration
	Err     error
}

// HTTPProber makes one fresh connection per request, so every sample pays
// the full DNS + TCP + TLS cost a browser would on a cold start. It never
// uses a proxy: the point is to measure this connection.
type HTTPProber struct {
	client *http.Client
}

// NewHTTPProber returns a prober whose requests give up after timeout.
func NewHTTPProber(timeout time.Duration) *HTTPProber {
	return newHTTPProber(timeout, nil)
}

func newHTTPProber(timeout time.Duration, tlsConf *tls.Config) *HTTPProber {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
		TLSClientConfig:     tlsConf,
		ForceAttemptHTTP2:   true,
	}
	return &HTTPProber{client: &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// A redirect is still a working connection; don't chase it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Probe fetches target and times each phase.
func (p *HTTPProber) Probe(ctx context.Context, target string) HTTPResult {
	var dnsStart, dnsDone, connStart, connDone, tlsStart, tlsDone, wrote, firstByte time.Time
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:  func(httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart: func(string, string) {
			if connStart.IsZero() {
				connStart = time.Now()
			}
		},
		ConnectDone:          func(string, string, error) { connDone = time.Now() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tlsDone = time.Now() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { wrote = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, target, nil)
	if err != nil {
		return HTTPResult{Err: err}
	}
	req.Header.Set("User-Agent", "internet-monitor")

	start := time.Now()
	resp, err := p.client.Do(req)
	var r HTTPResult
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		r.Status = resp.StatusCode
		r.OK = resp.StatusCode < 400
		if !r.OK {
			r.Err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	} else {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		r.Err = err
	}
	r.Total = time.Since(start)
	span := func(a, b time.Time) time.Duration {
		if a.IsZero() || b.IsZero() || b.Before(a) {
			return 0
		}
		return b.Sub(a)
	}
	r.DNS = span(dnsStart, dnsDone)
	r.Connect = span(connStart, connDone)
	r.TLS = span(tlsStart, tlsDone)
	r.TTFB = span(wrote, firstByte)
	return r
}
