package probe

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// QueryDNS sends one recursive A query for name to server:53 over UDP and
// times the round trip. It succeeds only for NOERROR with at least one answer.
func QueryDNS(ctx context.Context, server netip.Addr, name string, timeout time.Duration) Result {
	return queryDNS(ctx, netip.AddrPortFrom(server, 53), name, timeout)
}

func queryDNS(ctx context.Context, server netip.AddrPort, name string, timeout time.Duration) Result {
	if !strings.HasSuffix(name, ".") {
		name += "." // fully qualified: no search domains
	}
	qname, err := dnsmessage.NewName(name)
	if err != nil {
		return Result{Err: fmt.Errorf("bad DNS name %q: %w", name, err)}
	}
	id := uint16(rand.IntN(0x10000))
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return Result{Err: err}
	}
	if err := b.Question(dnsmessage.Question{Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return Result{Err: err}
	}
	query, err := b.Finish()
	if err != nil {
		return Result{Err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server.String())
	if err != nil {
		return Result{Err: err}
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	start := time.Now()
	if _, err := conn.Write(query); err != nil {
		return Result{Err: err}
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout():
				return Result{Err: errTimeout}
			case errors.Is(err, syscall.ECONNREFUSED):
				return Result{Err: errors.New("connection refused: no DNS server listening")}
			}
			return Result{Err: err}
		}
		rtt := time.Since(start)
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil || h.ID != id || !h.Response {
			continue // not our reply
		}
		if h.RCode != dnsmessage.RCodeSuccess {
			return Result{Err: fmt.Errorf("DNS %s", strings.TrimPrefix(h.RCode.String(), "RCode"))}
		}
		if err := p.SkipAllQuestions(); err != nil {
			return Result{Err: err}
		}
		answers, err := p.AllAnswers()
		if err != nil {
			return Result{Err: err}
		}
		for _, a := range answers {
			if a.Header.Type == dnsmessage.TypeA {
				return Result{OK: true, RTT: rtt}
			}
		}
		return Result{Err: errors.New("DNS reply had no address")}
	}
}
