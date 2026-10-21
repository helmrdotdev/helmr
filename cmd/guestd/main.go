package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/helmrdotdev/helmr/internal/bootgate"
	"github.com/helmrdotdev/helmr/internal/guestd"
)

var bootGate = flag.Bool("boot-gate", false, "wait for fresh host bootstrap before guest services")

func main() {
	cfg := parseFlags()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *bootGate {
		if err := bootgate.Run(); err != nil {
			logger.Error("guest bootstrap failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := guestd.Run(context.Background(), cfg, logger); err != nil {
		logger.Error("guestd failed", "error", err)
		os.Exit(1)
	}
}

func parseFlags() guestd.Config {
	var cfg guestd.Config
	flag.UintVar(&cfg.VsockPort, "vsock-port", 5000, "guest execution vsock port")
	flag.UintVar(&cfg.HealthPort, "health-port", 5001, "health check vsock port")
	flag.Parse()
	return cfg
}
