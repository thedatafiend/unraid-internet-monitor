// Package probe implements the network probes.
package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	protoICMP   = 1
	protoICMPv6 = 58
	defaultTTL  = 64
	payloadSize = 56
)

// ErrTraceUnsupported is returned by Trace when the socket cannot receive
// ICMP errors (unprivileged ping sockets only deliver echo replies).
var ErrTraceUnsupported = errors.New("traceroute needs a raw ICMP socket (run as root or with CAP_NET_RAW)")

// Result is the outcome of one echo request.
type Result struct {
	OK  bool
	RTT time.Duration
	Err error
}

// Hop is one row of a traceroute.
type Hop struct {
	TTL     int           `json:"ttl"`
	Addr    netip.Addr    `json:"address"` // invalid when the hop did not answer
	RTT     time.Duration `json:"rtt"`
	Reached bool          `json:"reached"` // Addr is the destination itself
}

type replyKind int

const (
	kindEcho replyKind = iota
	kindTimeExceeded
	kindUnreachable
)

type reply struct {
	idx  int
	at   time.Time
	from netip.Addr
	kind replyKind
}

type waiter struct {
	idx int
	dst netip.Addr
	ch  chan reply
}

// Pinger sends ICMP echo requests over a single socket and matches replies by
// sequence number. It is safe for concurrent use.
type Pinger struct {
	family     int // 4 or 6
	conn       *icmp.PacketConn
	privileged bool
	id         int

	seq     atomic.Uint32
	writeMu sync.Mutex // serialises writes so a per-packet TTL never leaks to other packets
	mu      sync.Mutex
	pending map[uint16]waiter
	done    chan struct{}
}

// NewPinger opens an ICMP socket for family (4 or 6). It prefers a raw socket
// (needed for traceroute) and falls back to an unprivileged ping socket.
func NewPinger(family int) (*Pinger, error) {
	type attempt struct {
		network, addr string
		privileged    bool
	}
	var attempts []attempt
	switch family {
	case 4:
		attempts = []attempt{{"ip4:icmp", "0.0.0.0", true}, {"udp4", "0.0.0.0", false}}
	case 6:
		attempts = []attempt{{"ip6:ipv6-icmp", "::", true}, {"udp6", "::", false}}
	default:
		return nil, fmt.Errorf("unknown address family %d", family)
	}

	var errs []error
	for _, a := range attempts {
		conn, err := icmp.ListenPacket(a.network, a.addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.network, err))
			continue
		}
		p := &Pinger{
			family:     family,
			conn:       conn,
			privileged: a.privileged,
			id:         rand.IntN(0xffff) + 1,
			pending:    make(map[uint16]waiter),
			done:       make(chan struct{}),
		}
		p.seq.Store(uint32(rand.IntN(0xffff)))
		go p.readLoop()
		return p, nil
	}
	return nil, fmt.Errorf("open ICMPv%d socket: %w", family, errors.Join(errs...))
}

// Privileged reports whether the pinger uses a raw socket.
func (p *Pinger) Privileged() bool { return p.privileged }

// Mode describes the socket type for diagnostics.
func (p *Pinger) Mode() string {
	if p.privileged {
		return "raw"
	}
	return "unprivileged"
}

// Close closes the socket.
func (p *Pinger) Close() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	close(p.done)
	return p.conn.Close()
}

// PingAll sends one echo request to each destination and waits up to timeout
// for the replies. Results are returned in the same order as dsts.
func (p *Pinger) PingAll(ctx context.Context, dsts []netip.Addr, timeout time.Duration) []Result {
	ttls := make([]int, len(dsts))
	replies, sent, errs := p.exchange(ctx, dsts, ttls, timeout)
	out := make([]Result, len(dsts))
	for i := range dsts {
		switch r, ok := replies[i]; {
		case errs[i] != nil:
			out[i] = Result{Err: errs[i]}
		case !ok:
			out[i] = Result{Err: errTimeout}
		case r.kind == kindEcho:
			out[i] = Result{OK: true, RTT: r.at.Sub(sent[i])}
		case r.kind == kindUnreachable:
			out[i] = Result{Err: fmt.Errorf("destination unreachable (from %s)", r.from)}
		default:
			out[i] = Result{Err: fmt.Errorf("time exceeded (from %s)", r.from)}
		}
	}
	return out
}

var errTimeout = errors.New("timeout")

// Trace sends TTL-limited echo requests (TTL 1..maxHops) to dst in one burst
// and reports which router answered at each hop.
func (p *Pinger) Trace(ctx context.Context, dst netip.Addr, maxHops int, timeout time.Duration) ([]Hop, error) {
	if !p.privileged {
		return nil, ErrTraceUnsupported
	}
	dsts := make([]netip.Addr, maxHops)
	ttls := make([]int, maxHops)
	for i := range dsts {
		dsts[i], ttls[i] = dst, i+1
	}
	replies, sent, errs := p.exchange(ctx, dsts, ttls, timeout)
	var hops []Hop
	for i := range dsts {
		if errs[i] != nil {
			return hops, errs[i]
		}
		h := Hop{TTL: i + 1}
		if r, ok := replies[i]; ok {
			h.Addr, h.RTT = r.from, r.at.Sub(sent[i])
			h.Reached = r.kind == kindEcho
		}
		hops = append(hops, h)
		if h.Reached {
			break
		}
	}
	return hops, nil
}

// exchange sends one echo request per dst (with ttls[i] if non-zero) and
// collects the first reply for each until all answered or timeout elapsed.
func (p *Pinger) exchange(ctx context.Context, dsts []netip.Addr, ttls []int, timeout time.Duration) (map[int]reply, []time.Time, []error) {
	n := len(dsts)
	ch := make(chan reply, n)
	seqs := make([]uint16, n)
	sent := make([]time.Time, n)
	errs := make([]error, n)

	p.mu.Lock()
	for i, dst := range dsts {
		seq := uint16(p.seq.Add(1))
		seqs[i] = seq
		p.pending[seq] = waiter{idx: i, dst: dst, ch: ch}
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		for _, seq := range seqs {
			delete(p.pending, seq)
		}
		p.mu.Unlock()
	}()

	outstanding := 0
	p.writeMu.Lock()
	for i, dst := range dsts {
		if ttls[i] > 0 {
			if err := p.setTTL(ttls[i]); err != nil {
				errs[i] = err
				continue
			}
		}
		sent[i] = time.Now()
		if err := p.send(dst, seqs[i]); err != nil {
			errs[i] = err
			continue
		}
		outstanding++
	}
	for _, ttl := range ttls {
		if ttl > 0 {
			_ = p.setTTL(defaultTTL)
			break
		}
	}
	p.writeMu.Unlock()

	replies := make(map[int]reply, n)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for len(replies) < outstanding {
		select {
		case r := <-ch:
			if _, dup := replies[r.idx]; !dup {
				replies[r.idx] = r
			}
		case <-timer.C:
			return replies, sent, errs
		case <-ctx.Done():
			return replies, sent, errs
		case <-p.done:
			return replies, sent, errs
		}
	}
	return replies, sent, errs
}

func (p *Pinger) setTTL(ttl int) error {
	if p.family == 4 {
		return p.conn.IPv4PacketConn().SetTTL(ttl)
	}
	return p.conn.IPv6PacketConn().SetHopLimit(ttl)
}

func (p *Pinger) send(dst netip.Addr, seq uint16) error {
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if p.family == 6 {
		typ = ipv6.ICMPTypeEchoRequest
	}
	payload := make([]byte, payloadSize)
	copy(payload, "unraid-internet-monitor")
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: p.id, Seq: int(seq), Data: payload}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return err
	}
	var addr net.Addr = &net.IPAddr{IP: dst.AsSlice()}
	if !p.privileged {
		addr = &net.UDPAddr{IP: dst.AsSlice()}
	}
	_, err = p.conn.WriteTo(b, addr)
	return err
}

func (p *Pinger) readLoop() {
	proto := protoICMP
	if p.family == 6 {
		proto = protoICMPv6
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := p.conn.ReadFrom(buf)
		at := time.Now()
		if err != nil {
			select {
			case <-p.done:
				return
			default:
			}
			time.Sleep(10 * time.Millisecond) // avoid spinning on a persistent error
			continue
		}
		from := addrOf(peer)
		msg, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		switch body := msg.Body.(type) {
		case *icmp.Echo:
			if msg.Type != ipv4.ICMPTypeEchoReply && msg.Type != ipv6.ICMPTypeEchoReply {
				continue
			}
			// Unprivileged sockets get their ID rewritten by the kernel, which
			// also guarantees the reply is ours.
			if p.privileged && body.ID != p.id {
				continue
			}
			p.dispatch(uint16(body.Seq), from, at, kindEcho)
		case *icmp.TimeExceeded:
			if id, seq, ok := innerEcho(p.family, body.Data); ok && id == p.id {
				p.dispatch(seq, from, at, kindTimeExceeded)
			}
		case *icmp.DstUnreach:
			if id, seq, ok := innerEcho(p.family, body.Data); ok && id == p.id {
				p.dispatch(seq, from, at, kindUnreachable)
			}
		}
	}
}

func (p *Pinger) dispatch(seq uint16, from netip.Addr, at time.Time, kind replyKind) {
	p.mu.Lock()
	w, ok := p.pending[seq]
	if ok && kind == kindEcho && w.dst != from {
		ok = false // a stray reply with a colliding sequence number
	}
	if ok {
		delete(p.pending, seq)
	}
	p.mu.Unlock()
	if ok {
		select {
		case w.ch <- reply{idx: w.idx, at: at, from: from, kind: kind}:
		default:
		}
	}
}

// innerEcho extracts the echo ID and sequence from the original datagram
// quoted inside an ICMP error message.
func innerEcho(family int, data []byte) (id int, seq uint16, ok bool) {
	var hdrLen int
	switch family {
	case 4:
		if len(data) < 20 || data[0]>>4 != 4 {
			return 0, 0, false
		}
		hdrLen = int(data[0]&0x0f) * 4
		if data[9] != protoICMP {
			return 0, 0, false
		}
	case 6:
		if len(data) < 40 || data[0]>>4 != 6 || data[6] != protoICMPv6 {
			return 0, 0, false
		}
		hdrLen = 40
	}
	if len(data) < hdrLen+8 {
		return 0, 0, false
	}
	echo := data[hdrLen:]
	return int(binary.BigEndian.Uint16(echo[4:6])), binary.BigEndian.Uint16(echo[6:8]), true
}

func addrOf(a net.Addr) netip.Addr {
	var ip net.IP
	switch v := a.(type) {
	case *net.IPAddr:
		ip = v.IP
	case *net.UDPAddr:
		ip = v.IP
	}
	addr, _ := netip.AddrFromSlice(ip)
	return addr.Unmap()
}
