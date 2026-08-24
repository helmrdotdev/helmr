package guestd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mdlayher/vsock"
)

type Config struct {
	VsockPort  uint
	HealthPort uint
}

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	healthListener, err := vsock.Listen(uint32(cfg.HealthPort), nil)
	if err != nil {
		return fmt.Errorf("listen health vsock: %w", err)
	}
	defer healthListener.Close()
	var ready atomic.Bool
	go serveHealth(healthListener, ready.Load, logger)

	runListener, err := vsock.Listen(uint32(cfg.VsockPort), nil)
	if err != nil {
		return fmt.Errorf("listen guest task vsock: %w", err)
	}
	defer runListener.Close()
	ready.Store(true)
	logger.Info("guestd ready", "vsock_port", cfg.VsockPort, "health_port", cfg.HealthPort)

	registry := newWaitingRunRegistry()
	workspaceRegistry := newWorkspaceOperationRegistry()
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
			keepOpen, err := handleConnection(ctx, conn, logger, registry, workspaceRegistry)
			if keepOpen {
				closeConn = false
			}
			if err != nil {
				logger.Error("run failed", "error", err)
			}
		}()
	}
}

func serveHealth(listener net.Listener, ready func() bool, logger *slog.Logger) {
	var requestSeq atomic.Uint64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		requestID := requestSeq.Add(1)
		started := time.Now()
		isReady := ready()
		if logger != nil {
			logger.Info("guestd health request received", "request_id", requestID, "ready", isReady)
		}
		w.Header().Set("Content-Type", "application/json")
		status := "ok"
		if !isReady {
			status = "starting"
		}
		body := `{"status":"ok","component":"guestd"}`
		if status == "starting" {
			body = `{"status":"starting","component":"guestd"}`
		}
		written, err := io.WriteString(w, body)
		duration := time.Since(started)
		if err != nil {
			if logger != nil {
				logger.Error("guestd health response failed", "request_id", requestID, "ready", isReady, "status", status, "duration_ms", duration.Milliseconds(), "bytes", written, "error", err)
			}
			return
		}
		flushStarted := time.Now()
		flushErr := http.NewResponseController(w).Flush()
		flushDuration := time.Since(flushStarted)
		if flushErr != nil {
			if logger != nil {
				logger.Error("guestd health response failed", "request_id", requestID, "ready", isReady, "status", status, "duration_ms", duration.Milliseconds(), "flush_duration_ms", flushDuration.Milliseconds(), "bytes", written, "error", flushErr)
			}
			return
		}
		if logger != nil {
			logger.Info("guestd health response written", "request_id", requestID, "ready", isReady, "status", status, "duration_ms", duration.Milliseconds(), "flush_duration_ms", flushDuration.Milliseconds(), "bytes", written)
		}
	})
	_ = http.Serve(listener, mux)
}
