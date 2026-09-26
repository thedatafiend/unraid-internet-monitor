// Package model holds the plain data types shared between the probe engine,
// the store and the HTTP API.
package model

import "net/netip"

// Target roles.
const (
	RoleGateway  = "gateway"
	RoleISP      = "isp"
	RoleInternet = "internet"
	RoleCustom   = "custom"
)

// Address families.
const (
	FamilyV4 = "ip4"
	FamilyV6 = "ip6"
)

// Target is something we probe.
type Target struct {
	ID      int64      `json:"id"`
	Key     string     `json:"key"` // stable identity across restarts, e.g. "icmp:gateway:ip4" or "icmp:1.1.1.1"
	Kind    string     `json:"kind"`
	Role    string     `json:"role"`
	Name    string     `json:"name"`
	Addr    netip.Addr `json:"address"`
	Family  string     `json:"family"`
	Enabled bool       `json:"enabled"`
}

// FamilyOf returns FamilyV4 or FamilyV6 for addr.
func FamilyOf(addr netip.Addr) string {
	if addr.Unmap().Is4() {
		return FamilyV4
	}
	return FamilyV6
}

// MinuteStat is the per-target, per-minute rollup persisted to the database.
// RTT values are milliseconds and are nil when no reply was received.
type MinuteStat struct {
	TargetID int64
	TS       int64 // unix seconds, start of minute
	Sent     int
	Recv     int
	Min      *float64
	Avg      *float64
	P50      *float64
	P95      *float64
	P99      *float64
	Max      *float64
	Jitter   *float64
}

// Bucket is one downsampled point of a stored time series.
type Bucket struct {
	TS     int64
	Sent   int
	Recv   int
	Min    *float64
	Avg    *float64
	P50    *float64
	P95    *float64
	P99    *float64
	Max    *float64
	Jitter *float64
}
