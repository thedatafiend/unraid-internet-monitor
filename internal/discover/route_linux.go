package discover

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// RouteTo asks the kernel which route it would use to reach dst, like
// `ip route get`. Unlike reading /proc/net/route, this honours policy routing
// rules and every routing table (Tailscale, WireGuard and VPN clients often
// install their routes outside the main table).
func RouteTo(dst netip.Addr) (EgressRoute, error) {
	dst = dst.Unmap()
	family, bits := syscall.AF_INET, 32
	if dst.Is6() {
		family, bits = syscall.AF_INET6, 128
	}

	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return EgressRoute{}, fmt.Errorf("netlink socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return EgressRoute{}, fmt.Errorf("netlink bind: %w", err)
	}

	addr := dst.AsSlice()
	attrLen := syscall.SizeofRtAttr + len(addr)
	msgLen := syscall.NLMSG_HDRLEN + syscall.SizeofRtMsg + rtaAlign(attrLen)
	b := make([]byte, msgLen)
	ne := binary.NativeEndian
	// struct nlmsghdr
	ne.PutUint32(b[0:], uint32(msgLen))
	ne.PutUint16(b[4:], syscall.RTM_GETROUTE)
	ne.PutUint16(b[6:], syscall.NLM_F_REQUEST)
	ne.PutUint32(b[8:], 1) // seq
	// struct rtmsg
	rt := b[syscall.NLMSG_HDRLEN:]
	rt[0] = byte(family)
	rt[1] = byte(bits)
	ne.PutUint32(rt[8:], rtmFLookupTable) // report the table the route came from
	// struct rtattr RTA_DST
	at := rt[syscall.SizeofRtMsg:]
	ne.PutUint16(at[0:], uint16(attrLen))
	ne.PutUint16(at[2:], syscall.RTA_DST)
	copy(at[syscall.SizeofRtAttr:], addr)

	if err := syscall.Sendto(fd, b, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return EgressRoute{}, fmt.Errorf("netlink send: %w", err)
	}
	buf := make([]byte, 8192)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return EgressRoute{}, fmt.Errorf("netlink recv: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return EgressRoute{}, err
	}
	for i := range msgs {
		m := &msgs[i]
		switch m.Header.Type {
		case syscall.NLMSG_ERROR:
			if len(m.Data) >= 4 {
				if errno := -int32(ne.Uint32(m.Data[0:4])); errno != 0 {
					return EgressRoute{}, fmt.Errorf("no route to %s: %w", dst, syscall.Errno(errno))
				}
			}
		case syscall.RTM_NEWROUTE:
			return parseRoute(m)
		}
	}
	return EgressRoute{}, fmt.Errorf("no route to %s: empty netlink reply", dst)
}

const rtmFLookupTable = 0x1000 // RTM_F_LOOKUP_TABLE

func rtaAlign(n int) int { return (n + syscall.RTA_ALIGNTO - 1) &^ (syscall.RTA_ALIGNTO - 1) }

func parseRoute(m *syscall.NetlinkMessage) (EgressRoute, error) {
	if len(m.Data) < syscall.SizeofRtMsg {
		return EgressRoute{}, fmt.Errorf("short netlink route message")
	}
	r := EgressRoute{Table: int(m.Data[4])}
	attrs, err := syscall.ParseNetlinkRouteAttr(m)
	if err != nil {
		return EgressRoute{}, err
	}
	ne := binary.NativeEndian
	for _, a := range attrs {
		switch a.Attr.Type {
		case syscall.RTA_GATEWAY:
			r.Gateway, _ = netip.AddrFromSlice(a.Value)
			r.Gateway = r.Gateway.Unmap()
		case syscall.RTA_PREFSRC:
			r.Src, _ = netip.AddrFromSlice(a.Value)
			r.Src = r.Src.Unmap()
		case syscall.RTA_OIF:
			if len(a.Value) >= 4 {
				if ifc, err := net.InterfaceByIndex(int(ne.Uint32(a.Value))); err == nil {
					r.Iface = ifc.Name
				}
			}
		case syscall.RTA_TABLE:
			if len(a.Value) >= 4 {
				r.Table = int(ne.Uint32(a.Value))
			}
		}
	}
	return r, nil
}
