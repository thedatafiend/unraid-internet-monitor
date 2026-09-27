//go:build !linux

package main

import (
	"log/slog"

	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
)

// dropPrivileges is a no-op off Linux; the app only targets Linux containers.
func dropPrivileges(config.Config, *slog.Logger) error { return nil }
