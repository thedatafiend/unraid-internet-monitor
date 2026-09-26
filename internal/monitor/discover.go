package monitor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/discover"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
)

// Info describes what discovery found; it is shown on the Settings page and
// by the diag command.
type Info struct {
	SocketV4      string      `json:"socket_v4"`
	SocketV6      string      `json:"socket_v6"`
	Gateway       string      `json:"gateway"`
	GatewaySource string      `json:"gateway_source"`
	EgressIface   string      `json:"egress_iface"` // interface the kernel uses to reach the internet
	EgressSrc     string      `json:"egress_src"`
	EgressTable   string      `json:"egress_table"`
	Tunnel        string      `json:"tunnel"` // non-empty when internet traffic leaves through a VPN/overlay
	ISPHop        string      `json:"isp_hop"`
	ISPHopSource  string      `json:"isp_hop_source"`
	Trace         []probe.Hop `json:"trace"`
	IPv6Mode      string      `json:"ipv6_mode"`
	IPv6Available bool        `json:"ipv6_available"`
	DiscoveredAt  time.Time   `json:"discovered_at"`
	Warnings      []string    `json:"warnings"`
}

var knownNames = map[string]string{
	"1.1.1.1":              "Cloudflare",
	"1.0.0.1":              "Cloudflare",
	"8.8.8.8":              "Google",
	"8.8.4.4":              "Google",
	"9.9.9.9":              "Quad9",
	"2606:4700:4700::1111": "Cloudflare v6",
	"2001:4860:4860::8888": "Google v6",
	"2620:fe::fe":          "Quad9 v6",
}

var dockerBridge = netip.MustParsePrefix("172.16.0.0/12")

// Discover resolves the full target list from config, the routing table and
// a traceroute. p6 may be nil. Returned targets have no IDs yet.
func Discover(ctx context.Context, cfg config.Config, p4, p6 *probe.Pinger) ([]model.Target, Info) {
	info := Info{IPv6Mode: cfg.IPv6, DiscoveredAt: time.Now(), SocketV4: socketMode(p4), SocketV6: socketMode(p6)}
	warn := func(format string, a ...any) { info.Warnings = append(info.Warnings, fmt.Sprintf(format, a...)) }
	var targets []model.Target

	// Internet targets (IPv4). Resolved first: the ISP-hop trace goes to the first one.
	var internet4 []netip.Addr
	for _, host := range cfg.PingTargets {
		addr, err := resolve(ctx, host, false)
		if err != nil {
			warn("internet target %q: %v", host, err)
			continue
		}
		internet4 = append(internet4, addr)
		targets = append(targets, internetTarget(host, addr))
	}

	// Ask the kernel which route internet traffic actually takes. This sees
	// policy routing (Tailscale exit nodes, VPN clients) that the main table hides.
	probeDst := netip.MustParseAddr("1.1.1.1")
	if len(internet4) > 0 {
		probeDst = internet4[0]
	}
	egress, egressErr := discover.RouteTo(probeDst)
	if egressErr == nil {
		info.EgressIface, info.EgressTable, info.Tunnel = egress.Iface, egress.TableName(), egress.TunnelKind()
		if egress.Src.IsValid() {
			info.EgressSrc = egress.Src.String()
		}
		if info.Tunnel != "" {
			warn("traffic to %s leaves through %s via %s (routing table %s), so every measurement includes that tunnel, not just your ISP. "+
				"If this is not intended, turn off the exit node/VPN for this server", probeDst, egress.Iface, info.Tunnel, info.EgressTable)
		}
	}

	// Gateway.
	var gateway netip.Addr
	switch cfg.Gateway {
	case config.Off:
	case config.Auto:
		switch {
		case egressErr == nil && info.Tunnel == "" && egress.Gateway.IsValid():
			gateway = egress.Gateway
		case egressErr == nil && info.Tunnel == "":
			warn("traffic to %s leaves via %s without a next-hop router (PPPoE or a point-to-point link?); set GATEWAY to your router's LAN IP if you have one", probeDst, egress.Iface)
		default:
			// Behind a tunnel (or netlink failed) the LAN router is still the
			// main table's default route: that is the path the tunnel itself uses.
			if r, err := discover.DefaultRouteV4(); err == nil {
				gateway = r.Gateway
			} else {
				warn("gateway auto-detect failed: %v; set GATEWAY to your router's LAN IP", err)
			}
		}
		if !gateway.IsValid() {
			break
		}
		info.Gateway, info.GatewaySource = gateway.String(), "auto"
		if dockerBridge.Contains(gateway) {
			warn("gateway %s looks like a Docker bridge; use host networking or set GATEWAY to your router's IP", gateway)
		}
		targets = append(targets, roleTarget(model.RoleGateway, "Gateway", gateway))
	default:
		addr, err := netip.ParseAddr(cfg.Gateway)
		if err != nil {
			warn("GATEWAY %q is not an IP address", cfg.Gateway)
			break
		}
		info.Gateway, info.GatewaySource = addr.String(), "config"
		gateway = addr
		targets = append(targets, roleTarget(model.RoleGateway, "Gateway", addr))
	}

	// ISP edge router.
	switch cfg.ISPHop {
	case config.Off:
	case config.Auto:
		if p4 == nil || len(internet4) == 0 {
			break
		}
		hops, err := p4.Trace(ctx, internet4[0], 8, 2*time.Second)
		info.Trace = hops
		if err != nil {
			warn("ISP hop auto-detect: %v; set ISP_HOP to an IP or off", err)
			break
		}
		if len(hops) > 0 && hops[0].Reached {
			warn("a probe limited to 1 hop was answered by %s itself, so something in the path does not decrement TTL "+
				"(a router with hardware NAT acceleration, or a tunnel/proxy answering pings on the destination's behalf). "+
				"Pings are still valid, but the ISP hop cannot be found; set ISP_HOP to an IP or off", internet4[0])
			break
		}
		hop := discover.PickISPHop(hops, gateway)
		if !hop.IsValid() {
			warn("ISP hop auto-detect: no public router answered before %s; set ISP_HOP to an IP or off", internet4[0])
			break
		}
		if discover.IsCGNAT(hop) && info.Tunnel != "" {
			warn("ISP hop %s is in 100.64/10 and traffic leaves through %s; it is probably the tunnel endpoint", hop, info.Tunnel)
		}
		info.ISPHop, info.ISPHopSource = hop.String(), "auto"
		targets = append(targets, roleTarget(model.RoleISP, "ISP edge", hop))
	default:
		addr, err := netip.ParseAddr(cfg.ISPHop)
		if err != nil {
			warn("ISP_HOP %q is not an IP address", cfg.ISPHop)
			break
		}
		info.ISPHop, info.ISPHopSource = addr.String(), "config"
		targets = append(targets, roleTarget(model.RoleISP, "ISP edge", addr))
	}

	// IPv6 (stub): internet targets only, when the host has IPv6 connectivity.
	// Link-local gateway probing needs zone handling and is left for later.
	switch cfg.IPv6 {
	case config.IPv6On:
		info.IPv6Available = true
	case config.IPv6Auto:
		if len(cfg.PingTargetsV6) > 0 {
			if dst, err := netip.ParseAddr(cfg.PingTargetsV6[0]); err == nil {
				info.IPv6Available = discover.IPv6Available(dst)
			}
		}
	}
	if info.IPv6Available {
		if p6 == nil {
			warn("IPv6 looks available but no ICMPv6 socket could be opened")
			info.IPv6Available = false
		} else {
			for _, host := range cfg.PingTargetsV6 {
				addr, err := netip.ParseAddr(host)
				if err != nil || !addr.Is6() {
					warn("PING_TARGETS_V6 entry %q is not an IPv6 address", host)
					continue
				}
				targets = append(targets, internetTarget(host, addr))
			}
		}
	}

	// Custom targets.
	for _, c := range cfg.CustomTargets {
		addr, err := resolve(ctx, c.Host, info.IPv6Available)
		if err != nil {
			warn("custom target %s (%s): %v", c.Name, c.Host, err)
			continue
		}
		targets = append(targets, model.Target{
			Key: "icmp:custom:" + c.Name, Kind: "icmp", Role: model.RoleCustom, Name: c.Name,
			Addr: addr, Family: model.FamilyOf(addr), Enabled: true,
		})
	}
	return targets, info
}

func socketMode(p *probe.Pinger) string {
	if p == nil {
		return "unavailable"
	}
	return p.Mode()
}

func internetTarget(host string, addr netip.Addr) model.Target {
	name := knownNames[addr.String()]
	if name == "" {
		name = host
	}
	return model.Target{
		Key: "icmp:" + addr.String(), Kind: "icmp", Role: model.RoleInternet, Name: name,
		Addr: addr, Family: model.FamilyOf(addr), Enabled: true,
	}
}

// roleTarget builds a gateway or ISP target. Its key is the role, not the
// address, so history stays continuous when the router or ISP hop changes.
func roleTarget(role, name string, addr netip.Addr) model.Target {
	fam := model.FamilyOf(addr)
	return model.Target{
		Key: "icmp:" + role + ":" + fam, Kind: "icmp", Role: role, Name: name,
		Addr: addr, Family: fam, Enabled: true,
	}
}

// resolve parses host as an IP or looks it up, preferring IPv4.
func resolve(ctx context.Context, host string, allow6 bool) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is6() && !allow6 {
			return netip.Addr{}, fmt.Errorf("IPv6 is not available")
		}
		return addr, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, err
	}
	var v6 netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() {
			return a, nil
		}
		if !v6.IsValid() {
			v6 = a
		}
	}
	if v6.IsValid() && allow6 {
		return v6, nil
	}
	return netip.Addr{}, fmt.Errorf("no usable address")
}
