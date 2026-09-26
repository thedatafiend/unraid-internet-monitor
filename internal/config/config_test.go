package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Defaults()) {
		t.Fatalf("got %+v, want defaults", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LISTEN_ADDR":    "127.0.0.1:9000",
		"RETENTION_DAYS": "7",
		"PING_TARGETS":   " 1.1.1.1 , 8.8.4.4,",
		"IPV6":           "OFF",
		"GATEWAY":        "192.168.1.1",
		"ISP_HOP":        "Off",
		"PING_INTERVAL":  "2s",
		"CUSTOM_TARGETS": "vpn=vpn.example.com, 203.0.113.5",
		"PUID":           "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:9000" || c.RetentionDays != 7 || c.IPv6 != IPv6Off ||
		c.Gateway != "192.168.1.1" || c.ISPHop != Off || c.PingInterval != 2*time.Second || c.PUID != 0 {
		t.Fatalf("unexpected config %+v", c)
	}
	if want := []string{"1.1.1.1", "8.8.4.4"}; !reflect.DeepEqual(c.PingTargets, want) {
		t.Fatalf("PingTargets = %v, want %v", c.PingTargets, want)
	}
	want := []NamedHost{{"vpn", "vpn.example.com"}, {"203.0.113.5", "203.0.113.5"}}
	if !reflect.DeepEqual(c.CustomTargets, want) {
		t.Fatalf("CustomTargets = %v, want %v", c.CustomTargets, want)
	}
}

func TestLoadErrors(t *testing.T) {
	_, err := Load(env(map[string]string{
		"RETENTION_DAYS": "0",
		"IPV6":           "maybe",
		"PING_INTERVAL":  "10ms",
		"CUSTOM_TARGETS": "=host",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, key := range []string{"RETENTION_DAYS", "IPV6", "PING_INTERVAL", "CUSTOM_TARGETS"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}
