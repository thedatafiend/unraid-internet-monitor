package discover

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
)

func TestParseRouteV4(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
br0	00000000	0101A8C0	0003	0	0	0	00000000	0	0	0
wg0	00000000	0100000A	0003	0	0	10	00000000	0	0	0
br0	0001A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`
	r, err := parseRouteV4(strings.NewReader(table))
	if err != nil {
		t.Fatal(err)
	}
	if r.Gateway != netip.MustParseAddr("192.168.1.1") || r.Iface != "br0" {
		t.Fatalf("got %+v", r)
	}

	// The line the kernel prints for "default via 192.168.68.1 dev br0 proto dhcp metric 1006".
	const unraid = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"br0\t00000000\t0144A8C0\t0003\t0\t0\t1006\t00000000\t0\t0\t0\n"
	r, err = parseRouteV4(strings.NewReader(unraid))
	if err != nil || r.Gateway != netip.MustParseAddr("192.168.68.1") || r.Iface != "br0" {
		t.Fatalf("unraid-style route: got %+v, %v", r, err)
	}

	if _, err := parseRouteV4(strings.NewReader("Iface\tDestination\n")); err == nil {
		t.Fatal("expected error for table without default route")
	}
}

func TestParseHasGlobalIPv6(t *testing.T) {
	const linkLocalOnly = `00000000000000000000000000000001 01 80 10 80       lo
fe800000000000000211223344556677 02 40 20 80      br0
fd000000000000000000000000000001 02 40 00 80      br0
`
	if parseHasGlobalIPv6(strings.NewReader(linkLocalOnly)) {
		t.Fatal("link-local and ULA are not global")
	}
	global := linkLocalOnly + "20010db8000000000000000000000001 02 40 00 80      br0\n"
	if !parseHasGlobalIPv6(strings.NewReader(global)) {
		t.Fatal("expected global address to be detected")
	}
}

func TestPickISPHop(t *testing.T) {
	a := netip.MustParseAddr
	tests := []struct {
		name    string
		hops    []probe.Hop
		gateway netip.Addr
		want    netip.Addr
	}{
		{"typical", []probe.Hop{{TTL: 1, Addr: a("192.168.1.1")}, {TTL: 2, Addr: a("68.1.2.3")}, {TTL: 3, Addr: a("68.1.9.9")}}, a("192.168.1.1"), a("68.1.2.3")},
		{"double NAT and silent hop", []probe.Hop{{TTL: 1, Addr: a("172.17.0.1")}, {TTL: 2, Addr: a("192.168.0.1")}, {TTL: 3}, {TTL: 4, Addr: a("24.5.6.7")}}, a("172.17.0.1"), a("24.5.6.7")},
		{"CGNAT counts as ISP", []probe.Hop{{TTL: 1, Addr: a("192.168.1.1")}, {TTL: 2, Addr: a("100.72.0.1")}}, a("192.168.1.1"), a("100.72.0.1")},
		{"public gateway is skipped", []probe.Hop{{TTL: 1, Addr: a("203.0.113.1")}, {TTL: 2, Addr: a("68.1.2.3")}}, a("203.0.113.1"), a("68.1.2.3")},
		{"destination reached first", []probe.Hop{{TTL: 1, Addr: a("192.168.1.1")}, {TTL: 2, Addr: a("1.1.1.1"), Reached: true}}, a("192.168.1.1"), netip.Addr{}},
		{"nothing answered", []probe.Hop{{TTL: 1}, {TTL: 2}}, netip.Addr{}, netip.Addr{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PickISPHop(tt.hops, tt.gateway); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
