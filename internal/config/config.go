// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// IPv6 modes.
const (
	IPv6Auto = "auto"
	IPv6On   = "on"
	IPv6Off  = "off"
)

// Auto and Off are the special values accepted by GATEWAY and ISP_HOP.
const (
	Auto = "auto"
	Off  = "off"
)

// NamedHost is a user-defined custom target.
type NamedHost struct {
	Name string
	Host string
}

// Config is the fully-resolved runtime configuration.
type Config struct {
	ListenAddr    string
	DataDir       string
	RetentionDays int
	PingTargets   []string
	PingTargetsV6 []string
	IPv6          string
	CustomTargets []NamedHost
	Gateway       string // Auto, Off, or an IP
	ISPHop        string // Auto, Off, or an IP
	PingInterval  time.Duration
	PingTimeout   time.Duration
	PUID          int
	PGID          int

	// Detection.
	OutageThreshold time.Duration // all internet targets down this long = outage
	DegradedLossPct float64
	DegradedP95Ms   float64
	DegradedMin     time.Duration // degraded condition must hold this long

	// DNS, web and public-IP probes.
	DNSServers       []string // "system" means the first nameserver in /etc/resolv.conf
	DNSQuery         string
	DNSInterval      time.Duration
	HTTPTargets      []string
	HTTPInterval     time.Duration
	HTTPTimeout      time.Duration
	PublicIPInterval time.Duration // 0 disables

	// Speed test.
	SpeedtestSchedule string // daily "HH:MM" local time, or "off"
	SpeedtestDuration time.Duration
	SpeedtestStreams  int
	SpeedtestURL      string // Cloudflare-compatible __down/__up server

	// Alerts.
	DiscordWebhookURL string
	AlertMinOutage    time.Duration
	AlertCoalesce     time.Duration
	AlertISPHopChange bool
	AlertIPChange     bool
	AlertMinDownMbps  float64 // 0 = off
	AlertMinUpMbps    float64 // 0 = off
}

// Defaults returns the configuration used when no environment variables are set.
func Defaults() Config {
	return Config{
		ListenAddr:    ":8765",
		DataDir:       "/data",
		RetentionDays: 30,
		PingTargets:   []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"},
		PingTargetsV6: []string{"2606:4700:4700::1111", "2001:4860:4860::8888", "2620:fe::fe"},
		IPv6:          IPv6Auto,
		Gateway:       Auto,
		ISPHop:        Auto,
		PingInterval:  time.Second,
		PingTimeout:   2 * time.Second,
		PUID:          99,
		PGID:          100,

		OutageThreshold: 3 * time.Second,
		DegradedLossPct: 2,
		DegradedP95Ms:   100,
		DegradedMin:     2 * time.Minute,

		DNSServers:       []string{"system", "1.1.1.1"},
		DNSQuery:         "www.google.com",
		DNSInterval:      30 * time.Second,
		HTTPTargets:      []string{"https://www.google.com/generate_204"},
		HTTPInterval:     time.Minute,
		HTTPTimeout:      10 * time.Second,
		PublicIPInterval: 5 * time.Minute,

		SpeedtestSchedule: "04:00",
		SpeedtestDuration: 10 * time.Second,
		SpeedtestStreams:  6,
		SpeedtestURL:      "https://speed.cloudflare.com",

		AlertMinOutage: 30 * time.Second,
		AlertCoalesce:  5 * time.Minute,
		AlertIPChange:  true,
	}
}

// Load builds a Config from Defaults overridden by getenv (usually os.Getenv).
func Load(getenv func(string) string) (Config, error) {
	c := Defaults()
	var errs []string
	fail := func(key string, err error) { errs = append(errs, fmt.Sprintf("%s: %v", key, err)) }

	str := func(key string, dst *string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = v
		}
	}
	list := func(key string, dst *[]string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = splitList(v)
		}
	}
	integer := func(key string, dst *int, min int) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < min {
				fail(key, fmt.Errorf("want an integer >= %d, got %q", min, v))
				return
			}
			*dst = n
		}
	}
	duration := func(key string, dst *time.Duration, min time.Duration) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < min {
				fail(key, fmt.Errorf("want a duration >= %s, got %q", min, v))
				return
			}
			*dst = d
		}
	}

	float := func(key string, dst *float64, min float64) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < min {
				fail(key, fmt.Errorf("want a number >= %g, got %q", min, v))
				return
			}
			*dst = f
		}
	}
	boolean := func(key string, dst *bool) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				fail(key, fmt.Errorf("want true or false, got %q", v))
				return
			}
			*dst = b
		}
	}

	str("LISTEN_ADDR", &c.ListenAddr)
	str("DATA_DIR", &c.DataDir)
	integer("RETENTION_DAYS", &c.RetentionDays, 1)
	list("PING_TARGETS", &c.PingTargets)
	list("PING_TARGETS_V6", &c.PingTargetsV6)
	str("IPV6", &c.IPv6)
	str("GATEWAY", &c.Gateway)
	str("ISP_HOP", &c.ISPHop)
	duration("PING_INTERVAL", &c.PingInterval, 200*time.Millisecond)
	duration("PING_TIMEOUT", &c.PingTimeout, 100*time.Millisecond)
	integer("PUID", &c.PUID, 0)
	integer("PGID", &c.PGID, 0)
	duration("OUTAGE_THRESHOLD", &c.OutageThreshold, time.Second)
	float("DEGRADED_LOSS_PCT", &c.DegradedLossPct, 0.1)
	float("DEGRADED_P95_MS", &c.DegradedP95Ms, 1)
	duration("DEGRADED_MIN", &c.DegradedMin, 10*time.Second)
	str("DISCORD_WEBHOOK_URL", &c.DiscordWebhookURL)
	duration("ALERT_MIN_OUTAGE", &c.AlertMinOutage, 0)
	duration("ALERT_COALESCE", &c.AlertCoalesce, 0)
	boolean("ALERT_ISP_HOP_CHANGE", &c.AlertISPHopChange)
	boolean("ALERT_IP_CHANGE", &c.AlertIPChange)
	list("DNS_SERVERS", &c.DNSServers)
	str("DNS_QUERY", &c.DNSQuery)
	duration("DNS_INTERVAL", &c.DNSInterval, 5*time.Second)
	list("HTTP_TARGETS", &c.HTTPTargets)
	duration("HTTP_INTERVAL", &c.HTTPInterval, 10*time.Second)
	duration("HTTP_TIMEOUT", &c.HTTPTimeout, time.Second)
	duration("PUBLIC_IP_INTERVAL", &c.PublicIPInterval, 0)
	str("SPEEDTEST_SCHEDULE", &c.SpeedtestSchedule)
	duration("SPEEDTEST_DURATION", &c.SpeedtestDuration, 3*time.Second)
	integer("SPEEDTEST_STREAMS", &c.SpeedtestStreams, 1)
	str("SPEEDTEST_URL", &c.SpeedtestURL)
	c.SpeedtestURL = strings.TrimRight(c.SpeedtestURL, "/")
	float("ALERT_MIN_DOWN_MBPS", &c.AlertMinDownMbps, 0)
	float("ALERT_MIN_UP_MBPS", &c.AlertMinUpMbps, 0)

	c.SpeedtestSchedule = strings.ToLower(c.SpeedtestSchedule)
	if c.SpeedtestSchedule != Off {
		var h, m int
		if _, err := fmt.Sscanf(c.SpeedtestSchedule, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			fail("SPEEDTEST_SCHEDULE", fmt.Errorf("want HH:MM (24-hour) or off, got %q", c.SpeedtestSchedule))
		}
	}
	if c.SpeedtestStreams > 32 {
		fail("SPEEDTEST_STREAMS", fmt.Errorf("at most 32"))
	}

	for _, v := range []*[]string{&c.DNSServers, &c.HTTPTargets} {
		if len(*v) == 1 && strings.EqualFold((*v)[0], Off) {
			*v = nil // "off" disables the probe
		}
	}
	for _, u := range c.HTTPTargets {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			fail("HTTP_TARGETS", fmt.Errorf("%q is not an http(s) URL", u))
		}
	}
	for _, srv := range c.DNSServers {
		if srv != "system" {
			if _, err := netip.ParseAddr(srv); err != nil {
				fail("DNS_SERVERS", fmt.Errorf("%q is not an IP address or \"system\"", srv))
			}
		}
	}
	if c.PublicIPInterval > 0 && c.PublicIPInterval < time.Minute {
		fail("PUBLIC_IP_INTERVAL", fmt.Errorf("want 0 (off) or at least 1m"))
	}

	if u := c.DiscordWebhookURL; u != "" && !strings.HasPrefix(u, "https://") {
		// Don't echo the value: it contains the webhook token.
		fail("DISCORD_WEBHOOK_URL", fmt.Errorf("must be an https:// Discord webhook URL"))
	}

	c.IPv6 = strings.ToLower(c.IPv6)
	switch c.IPv6 {
	case IPv6Auto, IPv6On, IPv6Off:
	default:
		fail("IPV6", fmt.Errorf("want auto, on or off, got %q", c.IPv6))
	}
	c.Gateway = strings.ToLower(c.Gateway)
	c.ISPHop = strings.ToLower(c.ISPHop)

	if v := strings.TrimSpace(getenv("CUSTOM_TARGETS")); v != "" {
		hosts, err := parseNamedHosts(v)
		if err != nil {
			fail("CUSTOM_TARGETS", err)
		}
		c.CustomTargets = hosts
	}
	if len(c.PingTargets) == 0 {
		fail("PING_TARGETS", fmt.Errorf("at least one target is required"))
	}

	if len(errs) > 0 {
		return c, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// parseNamedHosts parses "name=host,name2=host2". A bare "host" uses the host as its name.
func parseNamedHosts(v string) ([]NamedHost, error) {
	var out []NamedHost
	for _, f := range splitList(v) {
		name, host, ok := strings.Cut(f, "=")
		if !ok {
			host = name
		}
		name, host = strings.TrimSpace(name), strings.TrimSpace(host)
		if name == "" || host == "" {
			return nil, fmt.Errorf("bad entry %q, want name=host", f)
		}
		out = append(out, NamedHost{Name: name, Host: host})
	}
	return out, nil
}
