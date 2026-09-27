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
	RoleDNS      = "dns"  // a DNS resolver, probed with a UDP query
	RoleHTTP     = "http" // a web endpoint, probed with a fresh HTTPS request
)

// Target kinds.
const (
	KindICMP = "icmp"
	KindDNS  = "dns"
	KindHTTP = "http"
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
	URL     string     `json:"url,omitempty"` // HTTP targets only
	Family  string     `json:"family"`
	Enabled bool       `json:"enabled"`
}

// Address returns what is stored in the targets table's address column.
func (t Target) Address() string {
	if t.Kind == KindHTTP {
		return t.URL
	}
	return t.Addr.String()
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

// Event kinds.
const (
	EventOutage       = "outage"         // all internet targets of a family unreachable
	EventPartial      = "partial"        // one target (or all of IPv6) unreachable while the internet is up
	EventDegraded     = "degraded"       // sustained loss or high latency
	EventISPHopChange = "isp_hop_change" // the ISP edge router changed
	EventIPChange     = "ip_change"      // the public IP address changed
)

// Outage classifications: where the break most likely is.
const (
	ClassLocal    = "local"    // the gateway was down too: LAN, router or modem
	ClassISPEdge  = "isp_edge" // gateway up, ISP edge router down
	ClassUpstream = "upstream" // gateway and ISP edge up, internet unreachable
)

// Event is an outage, degradation or change, open while EndedAt is nil.
type Event struct {
	ID        int64          `json:"id"`
	Kind      string         `json:"kind"`
	Scope     string         `json:"scope"` // ip4, ip6 or a target name
	Class     string         `json:"class,omitempty"`
	StartedAt int64          `json:"started_at"`
	EndedAt   *int64         `json:"ended_at"`
	Details   map[string]any `json:"details"`
}

// Duration returns the event's length in seconds, measured to now while it is open.
func (e Event) Duration(now int64) int64 {
	if e.EndedAt != nil {
		return *e.EndedAt - e.StartedAt
	}
	return now - e.StartedAt
}

// OutboxItem is a queued alert.
type OutboxItem struct {
	ID        int64
	CreatedAt int64
	Kind      string
	Payload   []byte
	Attempts  int
}

// Hop is one row of a traceroute (RTT in milliseconds).
type Hop struct {
	TTL     int     `json:"ttl"`
	Addr    string  `json:"address,omitempty"` // empty when the hop did not answer
	RTTMs   float64 `json:"rtt_ms,omitempty"`
	Reached bool    `json:"reached,omitempty"`
}

// Trace is a stored traceroute.
type Trace struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Reason  string `json:"reason"` // outage | discovery
	EventID *int64 `json:"event_id,omitempty"`
	Dst     string `json:"dst"`
	Hops    []Hop  `json:"hops"`
}

// HTTPSample is one web request broken into phases (milliseconds).
type HTTPSample struct {
	TargetID  int64    `json:"target_id"`
	TS        int64    `json:"ts"`
	OK        bool     `json:"ok"`
	Status    int      `json:"status,omitempty"`
	DNSMs     *float64 `json:"dns_ms"`
	ConnectMs *float64 `json:"connect_ms"`
	TLSMs     *float64 `json:"tls_ms"`
	TTFBMs    *float64 `json:"ttfb_ms"`
	TotalMs   *float64 `json:"total_ms"`
	Error     string   `json:"error,omitempty"`
}

// PublicIP is the address the internet sees, recorded when it changes.
type PublicIP struct {
	TS   int64  `json:"ts"`
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// SpeedTest is one throughput measurement with latency under load.
type SpeedTest struct {
	ID           int64    `json:"id"`
	TS           int64    `json:"ts"`
	Trigger      string   `json:"trigger"` // scheduled | manual
	DownMbps     *float64 `json:"down_mbps"`
	UpMbps       *float64 `json:"up_mbps"`
	IdleMs       *float64 `json:"idle_ms"`
	LoadedDownMs *float64 `json:"loaded_down_ms"`
	LoadedUpMs   *float64 `json:"loaded_up_ms"`
	Grade        string   `json:"grade,omitempty"` // bufferbloat grade A+..F
	BytesDown    int64    `json:"bytes_down"`
	BytesUp      int64    `json:"bytes_up"`
	Server       string   `json:"server,omitempty"` // Cloudflare location, e.g. DEN
	DurationS    float64  `json:"duration_s"`
	Error        string   `json:"error,omitempty"`
}
