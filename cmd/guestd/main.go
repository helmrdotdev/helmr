package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/helmrdotdev/helmr/internal/guestd"
)

func main() {
	cfg := parseFlags()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := guestd.Run(context.Background(), cfg, logger); err != nil {
		logger.Error("guestd failed", "error", err)
		os.Exit(1)
	}
}

func parseFlags() guestd.Config {
	var cfg guestd.Config
	flag.StringVar(&cfg.Profile, "profile", "", "guest execution profile")
	flag.UintVar(&cfg.VsockPort, "vsock-port", 5000, "guest task vsock port")
	flag.UintVar(&cfg.HealthPort, "health-port", 5001, "health check vsock port")
	flag.Parse()
	return cfg
}
