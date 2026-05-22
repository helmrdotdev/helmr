package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/helmrdotdev/helmr/internal/guestd"
)

func main() {
	cfg := guestd.ParseFlags()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := guestd.Run(context.Background(), cfg, logger); err != nil {
		logger.Error("guestd failed", "error", err)
		os.Exit(1)
	}
}
