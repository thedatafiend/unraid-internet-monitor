package discover

import (
	"net/netip"
	"testing"
)

func TestRouteToLoopback(t *testing.T) {
	r, err := RouteTo(netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Iface != "lo" || r.Gateway.IsValid() {
		t.Fatalf("got %+v, want device route on lo", r)
	}
}

// TestRouteToInternet cross-checks the netlink answer against the main-table
// default route when the host has one and no policy routing in the way.
func TestRouteToInternet(t *testing.T) {
	def, err := DefaultRouteV4()
	if err != nil {
		t.Skipf("no main-table default route: %v", err)
	}
	r, err := RouteTo(netip.MustParseAddr("198.51.100.1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Table == TableMain && (r.Gateway != def.Gateway || r.Iface != def.Iface) {
		t.Fatalf("netlink route %+v disagrees with /proc/net/route %+v", r, def)
	}
	if !r.Src.IsValid() {
		t.Fatalf("expected a source address, got %+v", r)
	}
}

func TestEgressTunnelKind(t *testing.T) {
	tests := []struct {
		r    EgressRoute
		want bool
	}{
		{EgressRoute{Iface: "br0", Table: TableMain}, false},
		{EgressRoute{Iface: "eth0", Table: TableMain}, false},
		{EgressRoute{Iface: "tailscale0", Table: TableTailscale}, true},
		{EgressRoute{Iface: "wg0", Table: TableMain}, true},
		{EgressRoute{Iface: "tun0", Table: 100}, true},
	}
	for _, tt := range tests {
		if got := tt.r.TunnelKind() != ""; got != tt.want {
			t.Errorf("%+v: tunnel=%v, want %v", tt.r, got, tt.want)
		}
	}
}
