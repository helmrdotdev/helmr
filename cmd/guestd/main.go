package main

import (
	"context"
	"log/slog"
	"os"
)

func main() {
	cfg := parseFlags()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), cfg, logger); err != nil {
		logger.Error("guestd failed", "error", err)
		os.Exit(1)
	}
}
