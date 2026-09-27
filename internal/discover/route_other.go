//go:build !linux

package discover

import (
	"errors"
	"net/netip"
)

// RouteTo is only implemented on Linux.
func RouteTo(netip.Addr) (EgressRoute, error) {
	return EgressRoute{}, errors.New("route lookup is only supported on Linux")
}
