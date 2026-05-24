package guestd

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

type Config struct {
	AdapterRuntimePath  string
	AdapterRegisterPath string
	AdapterPath         string
	RuntimePath         string
	VsockPort           uint
	HealthPort          uint
}

func ParseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.AdapterRuntimePath, "adapter-runtime-path", "/usr/bin/node", "adapter runtime executable path")
	flag.StringVar(&cfg.AdapterRegisterPath, "adapter-register-path", "/opt/helmr/adapter/register.mjs", "adapter runtime register hook path")
	flag.StringVar(&cfg.AdapterPath, "adapter-path", "/opt/helmr/adapter/main.js", "adapter entrypoint path")
	flag.StringVar(&cfg.RuntimePath, "runtime-path", "/opt/helmr-runtime", "runtime bundle path")
	flag.UintVar(&cfg.VsockPort, "vsock-port", 5000, "guest task vsock port")
	flag.UintVar(&cfg.HealthPort, "health-port", 5001, "health check vsock port")
	flag.Parse()
	return cfg
}

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.AdapterRuntimePath) == "" {
		return errors.New("adapter runtime path is required")
	}
	if strings.TrimSpace(cfg.AdapterPath) == "" {
		return errors.New("adapter path is required")
	}
	healthListener, err := vsock.Listen(uint32(cfg.HealthPort), nil)
	if err != nil {
		return fmt.Errorf("listen health vsock: %w", err)
	}
	defer healthListener.Close()
	var ready atomic.Bool
	go serveHealth(healthListener, ready.Load)

	runListener, err := vsock.Listen(uint32(cfg.VsockPort), nil)
	if err != nil {
		return fmt.Errorf("listen guest task vsock: %w", err)
	}
	defer runListener.Close()
	ready.Store(true)
	logger.Info("guestd ready", "vsock_port", cfg.VsockPort, "health_port", cfg.HealthPort)

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
