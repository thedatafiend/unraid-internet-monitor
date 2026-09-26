package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/aggregate"
	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
	"github.com/thedatafiend/unraid-internet-monitor/internal/monitor"
	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
)

const diagPings = 10

// diag checks the assumptions from milestone M0 (socket privileges,
// privilege drop, gateway and ISP-hop discovery, IPv6) and prints a report.
func diag() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	fmt.Printf("internet-monitor %s diagnostics\n\n", version)
	fmt.Printf("uid/gid at start:   %d/%d\n", os.Geteuid(), os.Getegid())

	p4, p6, err := openPingers(cfg, log)
	if err != nil {
		return fmt.Errorf("cannot open an ICMP socket: %w\n  hint: run as root in the container (the default) or grant NET_RAW", err)
	}
	defer p4.Close()
	if p6 != nil {
		defer p6.Close()
	}
	if err := dropPrivileges(cfg, log); err != nil {
		return fmt.Errorf("privilege drop failed: %w", err)
	}
	fmt.Printf("uid/gid after drop: %d/%d\n", os.Geteuid(), os.Getegid())

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	targets, info := monitor.Discover(ctx, cfg, p4, p6)

	fmt.Printf("ICMP socket v4:     %s\n", info.SocketV4)
	fmt.Printf("ICMP socket v6:     %s\n", info.SocketV6)
	fmt.Printf("egress route:       dev %s src %s table %s\n", orNone(info.EgressIface), orNone(info.EgressSrc), orNone(info.EgressTable))
	fmt.Printf("tunnel in path:     %s\n", orNone(info.Tunnel))
	fmt.Printf("gateway:            %s (%s)\n", orNone(info.Gateway), orNone(info.GatewaySource))
	fmt.Printf("ISP hop:            %s (%s)\n", orNone(info.ISPHop), orNone(info.ISPHopSource))
	fmt.Printf("IPv6:               mode=%s available=%v\n", info.IPv6Mode, info.IPv6Available)

	if len(info.Trace) > 0 {
		fmt.Printf("\ntraceroute to %s:\n", cfg.PingTargets[0])
		for _, h := range info.Trace {
			if !h.Addr.IsValid() {
				fmt.Printf("  %2d  *\n", h.TTL)
				continue
			}
			fmt.Printf("  %2d  %-40s %7.2f ms\n", h.TTL, h.Addr, ms(h.RTT))
		}
	}

	fmt.Printf("\nping x%d (after privilege drop):\n", diagPings)
	fmt.Printf("  %-9s %-16s %-40s %6s %8s %8s %8s\n", "ROLE", "NAME", "ADDRESS", "LOSS", "AVG", "P95", "JITTER")
	for _, fam := range []struct {
		name string
		p    *probe.Pinger
	}{{model.FamilyV4, p4}, {model.FamilyV6, p6}} {
		var group []model.Target
		var addrs []netip.Addr
		for _, t := range targets {
			if t.Family == fam.name {
				group, addrs = append(group, t), append(addrs, t.Addr)
			}
		}
		if len(group) == 0 || fam.p == nil {
			continue
		}
		rtts := make([][]float64, len(group))
		for i := 0; i < diagPings; i++ {
			for j, r := range fam.p.PingAll(ctx, addrs, cfg.PingTimeout) {
				if r.OK {
					rtts[j] = append(rtts[j], ms(r.RTT))
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		for j, t := range group {
			s := aggregate.Summarize(diagPings, rtts[j])
			if s.Recv == 0 {
				fmt.Printf("  %-9s %-16s %-40s %5.0f%% %8s %8s %8s\n", t.Role, t.Name, t.Addr, s.LossPct(), "-", "-", "-")
				continue
			}
			fmt.Printf("  %-9s %-16s %-40s %5.0f%% %8.2f %8.2f %8.2f\n", t.Role, t.Name, t.Addr, s.LossPct(), s.Avg, s.P95, s.Jitter)
		}
	}

	if len(info.Warnings) > 0 {
		fmt.Println("\nwarnings:")
		for _, w := range info.Warnings {
			fmt.Println("  -", w)
		}
	}
	return nil
}

func ms(d time.Duration) float64 { return d.Seconds() * 1000 }

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
