package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/mdlayher/vsock"
)

type config struct {
	bunPath     string
	adapterPath string
	runtimePath string
	vsockPort   uint
	healthPort  uint
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.bunPath, "bun-path", "/usr/bin/bun", "Bun executable path")
	flag.StringVar(&cfg.adapterPath, "adapter-path", "/opt/helmr/adapter/main.js", "adapter entrypoint path")
	flag.StringVar(&cfg.runtimePath, "runtime-path", "/opt/helmr-runtime", "runtime bundle path")
	flag.UintVar(&cfg.vsockPort, "vsock-port", 5000, "guest task vsock port")
	flag.UintVar(&cfg.healthPort, "health-port", 5001, "health check vsock port")
	flag.Parse()
	return cfg
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.bunPath) == "" {
		return errors.New("bun path is required")
	}
	if strings.TrimSpace(cfg.adapterPath) == "" {
		return errors.New("adapter path is required")
	}
	healthListener, err := vsock.Listen(uint32(cfg.healthPort), nil)
	if err != nil {
		return fmt.Errorf("listen health vsock: %w", err)
	}
	defer healthListener.Close()
	var ready atomic.Bool
	go serveHealth(healthListener, ready.Load)

	runListener, err := vsock.Listen(uint32(cfg.vsockPort), nil)
	if err != nil {
		return fmt.Errorf("listen guest task vsock: %w", err)
	}
	defer runListener.Close()
	ready.Store(true)
	logger.Info("guestd ready", "vsock_port", cfg.vsockPort, "health_port", cfg.healthPort)

	registry := newWaitingRunRegistry()
	for {
		conn, err := runListener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept guest task connection: %w", err)
		}
		go func() {
			closeConn := true
			defer func() {
				if closeConn {
					_ = conn.Close()
				}
			}()
			keepOpen, err := handleConnection(ctx, conn, cfg, logger, registry)
			if keepOpen {
				closeConn = false
			}
			if err != nil {
				logger.Error("run failed", "error", err)
			}
		}()
	}
}

func serveHealth(listener net.Listener, ready func() bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready() {
			_, _ = io.WriteString(w, `{"status":"starting","component":"guestd"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","component":"guestd"}`)
	})
	_ = http.Serve(listener, mux)
}
