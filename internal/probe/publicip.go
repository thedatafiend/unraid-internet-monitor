package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// IPFetcher asks public "what is my IP" services for this connection's
// address. Sources are tried in order; Cloudflare's trace endpoint is
// reached by IP so it works even when DNS is broken.
type IPFetcher struct {
	Sources4 []string
	Sources6 []string
	Timeout  time.Duration
}

// DefaultIPFetcher uses Cloudflare with ipify as a fallback.
func DefaultIPFetcher() *IPFetcher {
	return &IPFetcher{
		Sources4: []string{"https://1.1.1.1/cdn-cgi/trace", "https://api.ipify.org"},
		Sources6: []string{"https://[2606:4700:4700::1111]/cdn-cgi/trace", "https://api6.ipify.org"},
		Timeout:  10 * time.Second,
	}
}

// Fetch returns the public address for family 4 or 6.
func (f *IPFetcher) Fetch(ctx context.Context, family int) (netip.Addr, error) {
	sources, network := f.Sources4, "tcp4"
	if family == 6 {
		sources, network = f.Sources6, "tcp6"
	}
	dialer := &net.Dialer{Timeout: f.Timeout}
	client := &http.Client{
		Timeout: f.Timeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr) // force the address family
			},
		},
	}
	var errs []error
	for _, src := range sources {
		addr, err := fetchIP(ctx, client, src)
		if err == nil && (family == 6) != addr.Is4() {
			return addr, nil
		}
		if err == nil {
			err = fmt.Errorf("got %s, want IPv%d", addr, family)
		}
		errs = append(errs, fmt.Errorf("%s: %w", src, err))
	}
	return netip.Addr{}, errors.Join(errs...)
}

func fetchIP(ctx context.Context, client *http.Client, src string) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("User-Agent", "internet-monitor")
	resp, err := client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return netip.Addr{}, err
	}
	return parseIPBody(string(body))
}

// parseIPBody accepts either a bare address (ipify) or Cloudflare's
// key=value trace format with an "ip=" line.
func parseIPBody(body string) (netip.Addr, error) {
	body = strings.TrimSpace(body)
	if addr, err := netip.ParseAddr(body); err == nil {
		return addr.Unmap(), nil
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ip="); ok {
			addr, err := netip.ParseAddr(v)
			if err != nil {
				return netip.Addr{}, err
			}
			return addr.Unmap(), nil
		}
	}
	return netip.Addr{}, errors.New("no IP address in response")
}
