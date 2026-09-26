// Command internet-monitor continuously measures internet connection quality
// and serves a web UI.
//
// Usage:
//
//	internet-monitor [serve]    run the monitor (default)
//	internet-monitor diag       print discovery and a short ping test, then exit
//	internet-monitor healthcheck exit 0 if the local instance is healthy
//	internet-monitor version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/alert"
	"github.com/thedatafiend/unraid-internet-monitor/internal/api"
	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/monitor"
	"github.com/thedatafiend/unraid-internet-monitor/internal/probe"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "diag":
		err = diag()
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [serve|diag|healthcheck|version]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openPingers opens the ICMP sockets. This must happen before privileges are
// dropped because raw sockets need root or CAP_NET_RAW.
func openPingers(cfg config.Config, log *slog.Logger) (*probe.Pinger, *probe.Pinger, error) {
	p4, err := probe.NewPinger(4)
	if err != nil {
		return nil, nil, err
	}
	var p6 *probe.Pinger
	if cfg.IPv6 != config.IPv6Off {
		if p6, err = probe.NewPinger(6); err != nil {
			level := slog.LevelInfo // expected on hosts without IPv6
			if cfg.IPv6 == config.IPv6On {
				level = slog.LevelWarn
			}
			log.Log(context.Background(), level, "IPv6 ICMP unavailable", "err", err)
			p6 = nil
		}
	}
	return p4, p6, nil
}

func serve() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "listen", cfg.ListenAddr, "data", cfg.DataDir, "retention_days", cfg.RetentionDays)

	p4, p6, err := openPingers(cfg, log)
	if err != nil {
		return err
	}
	defer p4.Close()
	if p6 != nil {
		defer p6.Close()
	}

	// Bind before dropping privileges so a port below 1024 still works.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	if err := prepareDataDir(cfg); err != nil {
		return err
	}
	if err := dropPrivileges(cfg, log); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "monitor.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	var sender alert.Sender
	if cfg.DiscordWebhookURL != "" {
		sender = alert.NewDiscord(cfg.DiscordWebhookURL)
		log.Info("Discord alerts enabled")
	}
	alerts := alert.NewManager(alert.Config{
		MinOutage: cfg.AlertMinOutage, Coalesce: cfg.AlertCoalesce, ISPHopChange: cfg.AlertISPHopChange,
		IPChange: cfg.AlertIPChange,
	}, sender, st, log)
	alertsDone := make(chan struct{})
	go func() { defer close(alertsDone); alerts.Run(ctx) }()

	eng := monitor.New(cfg, p4, p6, st, alerts, log)
	srv := &http.Server{
		Handler:           api.New(eng, st, alerts, log, version, cfg),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srv.RegisterOnShutdown(eng.CloseStreams) // live streams never end on their own
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	engErr := make(chan error, 1)
	go func() { engErr <- eng.Run(ctx) }()

	select {
	case err = <-srvErr:
		stop()
	case err = <-engErr:
		stop()
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	if e := <-engErr; err == nil {
		err = e
	}
	<-alertsDone
	return err
}

// prepareDataDir creates the data directory and, when running as root, hands
// it to PUID:PGID so the database stays writable after the privilege drop.
func prepareDataDir(cfg config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	if os.Geteuid() != 0 || cfg.PUID == 0 {
		return nil
	}
	paths := []string{cfg.DataDir}
	matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "monitor.db*"))
	for _, p := range append(paths, matches...) {
		if err := os.Lchown(p, cfg.PUID, cfg.PGID); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	return nil
}

func healthcheck() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}
