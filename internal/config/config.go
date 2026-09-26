// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
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
