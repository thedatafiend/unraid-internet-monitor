package main

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
)

// dropPrivileges switches from root to PUID:PGID. Already-open sockets keep
// working. PUID=0 keeps root.
func dropPrivileges(cfg config.Config, log *slog.Logger) error {
	if os.Geteuid() != 0 || cfg.PUID == 0 {
		return nil
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(cfg.PGID); err != nil {
		return fmt.Errorf("setgid %d: %w", cfg.PGID, err)
	}
	if err := syscall.Setuid(cfg.PUID); err != nil {
		return fmt.Errorf("setuid %d: %w", cfg.PUID, err)
	}
	log.Info("dropped privileges", "uid", cfg.PUID, "gid", cfg.PGID)
	return nil
}
