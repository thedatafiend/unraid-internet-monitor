//go:build !linux

package speedtest

import "net"

const ackedAvailable = false

func bytesAcked(*net.TCPConn) (uint64, bool) { return 0, false }
