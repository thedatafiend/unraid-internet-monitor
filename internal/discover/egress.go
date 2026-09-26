package discover

import (
	"fmt"
	"net/netip"
	"strings"
)

// Well-known routing table IDs.
const (
	TableMain      = 254
	TableTailscale = 52 // used by tailscaled for its routes, including exit nodes
)

// EgressRoute is the route the kernel picks for one destination.
type EgressRoute struct {
	Gateway netip.Addr // invalid for a device route (tunnels, PPP)
	Iface   string
	Src     netip.Addr
	Table   int
}

// TableName returns a readable name for the route's table.
func (r EgressRoute) TableName() string {
	switch r.Table {
	case TableMain:
		return "main"
	case TableTailscale:
		return "52 (Tailscale)"
	case 0:
		return "unknown"
	}
	return fmt.Sprint(r.Table)
}

// TunnelKind returns a description when the route leaves through a VPN or
// overlay interface rather than the physical LAN, or "" otherwise.
func (r EgressRoute) TunnelKind() string {
	switch {
	case r.Table == TableTailscale || strings.HasPrefix(r.Iface, "tailscale"):
		return "Tailscale (an exit node is probably enabled on this server)"
	case strings.HasPrefix(r.Iface, "wg"):
		return "WireGuard"
	case strings.HasPrefix(r.Iface, "tun"), strings.HasPrefix(r.Iface, "tap"):
		return "a VPN tunnel"
	}
	return ""
}
