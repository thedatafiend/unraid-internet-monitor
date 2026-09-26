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

	if _, err := parseRouteV4(strings.NewReader("Iface\tDestination\n")); err == nil {
		t.Fatal("expected error for table without default route")
	}
}

func TestParseRouteV6(t *testing.T) {
	const table = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo
20010db8000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001      br0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe80000000000000021122fffe334455 00000400 00000001 00000000 00000003      br0
`
	r, err := parseRouteV6(strings.NewReader(table))
	if err != nil {
		t.Fatal(err)
	}
	if r.Gateway != netip.MustParseAddr("fe80::211:22ff:fe33:4455") || r.Iface != "br0" {
		t.Fatalf("got %+v", r)
	}

	onlyLo := strings.SplitN(table, "\n", 2)[0]
	if _, err := parseRouteV6(strings.NewReader(onlyLo)); err == nil {
		t.Fatal("expected unreachable lo route to be ignored")
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
