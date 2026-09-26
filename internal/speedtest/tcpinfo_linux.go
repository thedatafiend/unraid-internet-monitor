package speedtest

import (
	"net"

	"golang.org/x/sys/unix"
)

const ackedAvailable = true

// bytesAcked returns how many bytes the peer has acknowledged on c: data
// that really left this machine, unlike bytes merely queued in the socket.
func bytesAcked(c *net.TCPConn) (uint64, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var acked uint64
	var ok bool
	raw.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err == nil {
			acked, ok = info.Bytes_acked, true
		}
	})
	return acked, ok
}
