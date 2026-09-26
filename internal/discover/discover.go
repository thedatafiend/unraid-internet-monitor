// Package discover finds the default gateway, IPv6 availability and the ISP
// edge router from the host's routing tables and a short traceroute.
package discover

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
)

// Route is a default route.
type Route struct {
	Gateway netip.Addr
	Iface   string
}

// DefaultRouteV4 reads /proc/net/route.
func DefaultRouteV4() (Route, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return Route{}, err
	}
	defer f.Close()
	return parseRouteV4(f)
}

const (
	rtfUp      = 0x1
	rtfGateway = 0x2
)

func parseRouteV4(r io.Reader) (Route, error) {
	best, bestMetric := Route{}, -1
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		metric, err2 := strconv.Atoi(f[6])
		gw, err3 := hex.DecodeString(f[2])
		if err1 != nil || err2 != nil || err3 != nil || len(gw) != 4 {
			continue
		}
		if flags&rtfUp == 0 || flags&rtfGateway == 0 {
			continue
		}
		if bestMetric == -1 || metric < bestMetric {
			// /proc/net/route stores addresses in host (little-endian) order.
			best = Route{Gateway: netip.AddrFrom4([4]byte{gw[3], gw[2], gw[1], gw[0]}), Iface: f[0]}
			bestMetric = metric
		}
	}
	if err := sc.Err(); err != nil {
		return Route{}, err
	}
	if bestMetric == -1 {
		return Route{}, fmt.Errorf("no IPv4 default route")
	}
	return best, nil
}

// DefaultRouteV6 reads /proc/net/ipv6_route. Unreachable default routes on
// the loopback interface are ignored.
func DefaultRouteV6() (Route, error) {
	f, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		return Route{}, err
	}
	defer f.Close()
	return parseRouteV6(f)
}

func parseRouteV6(r io.Reader) (Route, error) {
	best, bestMetric := Route{}, uint64(0)
	found := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[0] != strings.Repeat("0", 32) || f[1] != "00" || f[9] == "lo" {
			continue
		}
		metric, err1 := strconv.ParseUint(f[5], 16, 32)
		flags, err2 := strconv.ParseUint(f[8], 16, 32)
		nh, err3 := hex.DecodeString(f[4])
		if err1 != nil || err2 != nil || err3 != nil || len(nh) != 16 || flags&rtfUp == 0 {
			continue
		}
		if !found || metric < bestMetric {
			best = Route{Gateway: netip.AddrFrom16([16]byte(nh)), Iface: f[9]}
			bestMetric, found = metric, true
		}
	}
	if err := sc.Err(); err != nil {
		return Route{}, err
	}
	if !found {
		return Route{}, fmt.Errorf("no IPv6 default route")
	}
	return best, nil
}

// HasGlobalIPv6 reports whether any interface other than lo has a global
// unicast IPv6 address (per /proc/net/if_inet6).
func HasGlobalIPv6() bool {
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return false
	}
	defer f.Close()
	return parseHasGlobalIPv6(f)
}

func parseHasGlobalIPv6(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 || f[5] == "lo" {
			continue
		}
		b, err := hex.DecodeString(f[0])
		if err != nil || len(b) != 16 {
			continue
		}
		addr := netip.AddrFrom16([16]byte(b))
		if addr.IsGlobalUnicast() && !addr.IsPrivate() {
			return true
		}
	}
	return false
}

// IPv6Available reports whether the host can reach the IPv6 internet: it has
// a default route and a global address.
func IPv6Available() bool {
	_, err := DefaultRouteV6()
	return err == nil && HasGlobalIPv6()
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isLocal reports whether addr belongs to the home network rather than the ISP.
// CGNAT space (100.64/10) is ISP-owned, so it is not local.
func isLocal(addr netip.Addr) bool {
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified()
}

// PickISPHop returns the first traceroute hop outside the home network: the
// ISP's edge router. The gateway is always treated as home network, even if
// it has a public address. It returns an invalid Addr when no such hop
// answered before the destination itself.
func PickISPHop(hops []probe.Hop, gateway netip.Addr) netip.Addr {
	for _, h := range hops {
		if h.Reached {
			return netip.Addr{}
		}
		if h.Addr.IsValid() && h.Addr != gateway && !isLocal(h.Addr) {
			return h.Addr
		}
	}
	return netip.Addr{}
}

// IsCGNAT reports whether addr is in 100.64.0.0/10. Tailscale also uses this
// range, which matters when diagnosing exit-node routing.
func IsCGNAT(addr netip.Addr) bool { return cgnat.Contains(addr) }
