package probe

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func TestInnerEchoIPv4(t *testing.T) {
	data := make([]byte, 28)
	data[0] = 0x45 // v4, IHL 5
	data[9] = protoICMP
	binary.BigEndian.PutUint16(data[24:], 0xBEEF)
	binary.BigEndian.PutUint16(data[26:], 42)
	id, seq, ok := innerEcho(4, data)
	if !ok || id != 0xBEEF || seq != 42 {
		t.Fatalf("got id=%x seq=%d ok=%v", id, seq, ok)
	}
	if _, _, ok := innerEcho(4, data[:27]); ok {
		t.Fatal("expected truncated datagram to be rejected")
	}
	data[9] = 17 // UDP
	if _, _, ok := innerEcho(4, data); ok {
		t.Fatal("expected non-ICMP datagram to be rejected")
	}
}

func TestInnerEchoIPv6(t *testing.T) {
	data := make([]byte, 48)
	data[0] = 0x60
	data[6] = protoICMPv6
	binary.BigEndian.PutUint16(data[44:], 7)
	binary.BigEndian.PutUint16(data[46:], 65535)
	id, seq, ok := innerEcho(6, data)
	if !ok || id != 7 || seq != 65535 {
		t.Fatalf("got id=%d seq=%d ok=%v", id, seq, ok)
	}
}

// TestPingLoopback needs either root/CAP_NET_RAW or ping_group_range; it is
// skipped when neither is available.
func TestPingLoopback(t *testing.T) {
	p, err := NewPinger(4)
	if err != nil {
		t.Skipf("no ICMP socket available: %v", err)
	}
	defer p.Close()

	lo := netip.MustParseAddr("127.0.0.1")
	res := p.PingAll(context.Background(), []netip.Addr{lo, lo, lo}, time.Second)
	for i, r := range res {
		if !r.OK {
			t.Fatalf("ping %d to loopback failed: %v", i, r.Err)
		}
		if r.RTT <= 0 || r.RTT > 100*time.Millisecond {
			t.Fatalf("ping %d: implausible RTT %v", i, r.RTT)
		}
	}

	if !p.Privileged() {
		return
	}
	hops, err := p.Trace(context.Background(), lo, 4, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 1 || !hops[0].Reached || hops[0].Addr != lo {
		t.Fatalf("unexpected trace to loopback: %+v", hops)
	}
}

func TestPingTimeout(t *testing.T) {
	p, err := NewPinger(4)
	if err != nil {
		t.Skipf("no ICMP socket available: %v", err)
	}
	defer p.Close()
	// TEST-NET-2 is documentation space and never answers; expect a timeout
	// or a send error. (TEST-NET-1 is avoided: some sandboxes use it for their gateway.)
	res := p.PingAll(context.Background(), []netip.Addr{netip.MustParseAddr("198.51.100.1")}, 200*time.Millisecond)
	if res[0].OK {
		t.Fatal("expected failure pinging TEST-NET-2")
	}
}
