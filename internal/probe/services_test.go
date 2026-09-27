package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers A queries on a local UDP port. rcode and answer control
// the reply; delay postpones it; a stray reply with the wrong ID is sent first.
func fakeDNS(t *testing.T, rcode dnsmessage.RCode, answer bool, delay time.Duration) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, _ := p.Question()
			time.Sleep(delay)
			stray := dnsmessage.Message{Header: dnsmessage.Header{ID: h.ID + 1, Response: true}}
			if b, err := stray.Pack(); err == nil {
				pc.WriteTo(b, from)
			}
			msg := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: h.ID, Response: true, RCode: rcode},
				Questions: []dnsmessage.Question{q},
			}
			if answer {
				msg.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte{93, 184, 216, 34}},
				}}
			}
			b, _ := msg.Pack()
			pc.WriteTo(b, from)
		}
	}()
	return netip.MustParseAddrPort(pc.LocalAddr().String())
}

func TestQueryDNS(t *testing.T) {
	ctx := context.Background()
	if r := queryDNS(ctx, fakeDNS(t, dnsmessage.RCodeSuccess, true, 5*time.Millisecond), "example.com", time.Second); !r.OK || r.RTT < 5*time.Millisecond {
		t.Fatalf("good reply: %+v", r)
	}
	if r := queryDNS(ctx, fakeDNS(t, dnsmessage.RCodeServerFailure, false, 0), "example.com", time.Second); r.OK || !strings.Contains(r.Err.Error(), "ServerFailure") {
		t.Fatalf("SERVFAIL: %+v", r)
	}
	if r := queryDNS(ctx, fakeDNS(t, dnsmessage.RCodeSuccess, false, 0), "example.com", time.Second); r.OK {
		t.Fatalf("empty answer should fail: %+v", r)
	}
	if r := queryDNS(ctx, fakeDNS(t, dnsmessage.RCodeSuccess, true, 300*time.Millisecond), "example.com", 100*time.Millisecond); r.OK || r.Err != errTimeout {
		t.Fatalf("slow reply should time out: %+v", r)
	}
	// Nothing listening: the kernel's ICMP port-unreachable surfaces as a clear error.
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	closedPort := netip.MustParseAddrPort(pc.LocalAddr().String())
	pc.Close()
	if r := queryDNS(ctx, closedPort, "example.com", time.Second); r.OK || !strings.Contains(r.Err.Error(), "no DNS server listening") {
		t.Fatalf("closed port: %+v", r)
	}
}

func TestHTTPProbePhases(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		time.Sleep(20 * time.Millisecond) // server think time shows up as TTFB
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	// Use a hostname so the DNS phase is exercised too.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	tlsConf := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConf.ServerName = "example.com" // the test certificate's name; we dial localhost
	p := newHTTPProber(2*time.Second, tlsConf)

	r := p.Probe(context.Background(), url+"/generate_204")
	if !r.OK || r.Status != 204 || r.Err != nil {
		t.Fatalf("result = %+v", r)
	}
	if r.DNS <= 0 || r.Connect <= 0 || r.TLS <= 0 || r.TTFB < 20*time.Millisecond || r.Total < r.TTFB {
		t.Fatalf("phases = dns %v connect %v tls %v ttfb %v total %v", r.DNS, r.Connect, r.TLS, r.TTFB, r.Total)
	}
	if r := p.Probe(context.Background(), url+"/fail"); r.OK || r.Status != 503 {
		t.Fatalf("503 should fail: %+v", r)
	}
	srv.Close()
	if r := p.Probe(context.Background(), url+"/generate_204"); r.OK || r.Err == nil {
		t.Fatalf("closed server should fail: %+v", r)
	}
}

func TestParseIPBody(t *testing.T) {
	trace := "fl=123\nh=1.1.1.1\nip=203.0.113.7\nts=1700000000\n"
	for body, want := range map[string]string{
		trace:            "203.0.113.7",
		"198.51.100.4\n": "198.51.100.4",
		"ip=2001:db8::1": "2001:db8::1",
	} {
		got, err := parseIPBody(body)
		if err != nil || got.String() != want {
			t.Errorf("parseIPBody(%q) = %v, %v", body, got, err)
		}
	}
	if _, err := parseIPBody("<html>captive portal</html>"); err == nil {
		t.Error("expected error for HTML")
	}
}

func TestIPFetcherFallback(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ip=203.0.113.7\n")) }))
	defer good.Close()
	wrongFamily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("2001:db8::1")) }))
	defer wrongFamily.Close()

	f := &IPFetcher{Sources4: []string{bad.URL, wrongFamily.URL, good.URL}, Timeout: time.Second}
	addr, err := f.Fetch(context.Background(), 4)
	if err != nil || addr.String() != "203.0.113.7" {
		t.Fatalf("got %v, %v", addr, err)
	}
	f.Sources4 = []string{bad.URL}
	if _, err := f.Fetch(context.Background(), 4); err == nil {
		t.Fatal("all sources failing should be an error")
	}
}
