//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const maxGuestHealthResponseBytes = 4096

var dialVsock = vsock.DialContext

func (c *Connector) waitForHealth(ctx context.Context, vsockPath string, machineExit *machineExit, logf func(string, ...interface{})) error {
	healthCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	stats := newHealthProbeStats()
	for {
		if err, ok := machineExit.Err(); ok {
			stats.machineExited = true
			stats.lastErr = err
			result := stats.failureError("the Firecracker machine exited during guest health wait", err)
			stats.log(logf, "failed")
			return result
		}
		stats.attempts++
		attemptStarted := time.Now()
		attemptCtx, cancelAttempt := healthAttemptContext(healthCtx, c.cfg.HealthAttemptTimeout)
		conn, err := dialVsock(attemptCtx, vsockPath, c.cfg.HealthPort)
		if err != nil {
			cancelAttempt()
			stats.recordError("dial", err)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			if healthCtx.Err() != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, healthCtx.Err(), err, machineExit)
				stats.log(logf, "failed")
				return result
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			continue
		}
		if deadline, ok := attemptCtx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		response, readErr := readHealth(conn)
		closeErr := conn.Close()
		cancelAttempt()
		if readErr != nil {
			stats.recordError(healthProbeErrorBucket(readErr), readErr)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			if healthCtx.Err() != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, healthCtx.Err(), readErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			continue
		}
		if closeErr != nil {
			stats.recordError("close", closeErr)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			result := stats.failureError("close guest health connection", closeErr)
			stats.log(logf, "failed")
			return result
		}
		if response.Status == "ok" && response.Component == "guestd" {
			stats.log(logf, "ready")
			return nil
		}
		if response.Status != "starting" {
			stats.recordStatus(response.Status)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			result := stats.failureError(fmt.Sprintf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message), nil)
			stats.log(logf, "failed")
			return result
		}
		stats.recordStarting(response)
		if err := sleepHealthRetry(healthCtx); err != nil {
			result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
			stats.log(logf, "failed")
			return result
		}
	}
}

func healthAttemptContext(ctx context.Context, attemptTimeout time.Duration) (context.Context, context.CancelFunc) {
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultHealthAttemptTimeout
	}
	return context.WithTimeout(ctx, attemptTimeout)
}

func (c *Connector) connectGuestPort(ctx context.Context, vsockPath string, machineExit *machineExit) (vm.Stream, error) {
	return c.connectGuestPortAt(ctx, vsockPath, c.cfg.GuestPort, machineExit)
}

func (c *Connector) connectGuestPortAt(ctx context.Context, vsockPath string, port uint32, machineExit *machineExit) (vm.Stream, error) {
	connectCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	if machineExit != nil {
		go func() {
			select {
			case <-machineExit.done:
				cancel()
			case <-connectCtx.Done():
			}
		}()
	}
	var lastErr error
	for {
		if err, ok := machineExit.Err(); ok {
			return nil, fmt.Errorf("the Firecracker machine exited before guest port %d connection: %w", port, err)
		}
		conn, err := dialVsock(connectCtx, vsockPath, port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if connectCtx.Err() != nil {
			if exitErr, ok := machineExit.Err(); ok {
				return nil, fmt.Errorf("the Firecracker machine exited before guest port %d connection: %w", port, exitErr)
			}
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", port, c.cfg.HealthTimeout, errors.Join(connectCtx.Err(), lastErr))
		}
		if err := sleepHealthRetry(connectCtx); err != nil {
			if exitErr, ok := machineExit.Err(); ok {
				return nil, fmt.Errorf("the Firecracker machine exited before guest port %d connection: %w", port, exitErr)
			}
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", port, c.cfg.HealthTimeout, errors.Join(err, lastErr))
		}
	}
}

func sleepHealthRetry(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type healthResponse struct {
	Status    string `json:"status"`
	Component string `json:"component"`
	Message   string `json:"message,omitempty"`
}

func readHealth(conn io.ReadWriter) (healthResponse, error) {
	req, err := http.NewRequest(http.MethodGet, "http://guestd/", nil)
	if err != nil {
		return healthResponse{}, fmt.Errorf("guest health request: %w", err)
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		return healthResponse{}, fmt.Errorf("write guest health request: %w", err)
	}
	httpResponse, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxGuestHealthResponseBytes+1))
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	if len(body) > maxGuestHealthResponseBytes {
		return healthResponse{}, fmt.Errorf("read guest health response: body exceeds %d bytes", maxGuestHealthResponseBytes)
	}
	if httpResponse.StatusCode != http.StatusOK {
		return healthResponse{}, fmt.Errorf("guest health returned HTTP %s: %s", httpResponse.Status, strings.TrimSpace(string(body)))
	}
	var response healthResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return healthResponse{}, fmt.Errorf("decode guest health response: %w", err)
	}
	return response, nil
}

type healthProbeStats struct {
	started           time.Time
	attempts          int
	dialErrors        int
	writeErrors       int
	readErrors        int
	statusErrors      int
	decodeErrors      int
	closeErrors       int
	startingResponses int
	machineExited     bool
	lastBucket        string
	lastStatus        string
	lastErr           error
}

func newHealthProbeStats() *healthProbeStats {
	return &healthProbeStats{started: time.Now()}
}

func (s *healthProbeStats) elapsed() time.Duration {
	if s == nil || s.started.IsZero() {
		return 0
	}
	return time.Since(s.started)
}

func (s *healthProbeStats) recordError(bucket string, err error) {
	if s == nil {
		return
	}
	if strings.TrimSpace(bucket) == "" {
		bucket = "unknown"
	}
	switch bucket {
	case "dial":
		s.dialErrors++
	case "write":
		s.writeErrors++
	case "read":
		s.readErrors++
	case "status":
		s.statusErrors++
	case "decode":
		s.decodeErrors++
	case "close":
		s.closeErrors++
	}
	s.lastBucket = bucket
	s.lastErr = err
}

func (s *healthProbeStats) recordStarting(response healthResponse) {
	if s == nil {
		return
	}
	s.startingResponses++
	s.lastBucket = "starting"
	s.lastStatus = response.Status
	s.lastErr = fmt.Errorf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message)
}

func (s *healthProbeStats) recordStatus(status string) {
	if s == nil {
		return
	}
	s.statusErrors++
	s.lastBucket = "status"
	s.lastStatus = status
	s.lastErr = fmt.Errorf("guest health status=%q", status)
}

func (s *healthProbeStats) timeoutError(timeout time.Duration, err error, lastErr error, machineExit *machineExit) error {
	if s == nil {
		return fmt.Errorf("guest health probe timed out after %s: %w", timeout, errors.Join(err, lastErr))
	}
	if exitErr, ok := machineExit.Err(); ok {
		s.machineExited = true
		lastErr = errors.Join(lastErr, fmt.Errorf("the Firecracker machine exited: %w", exitErr))
	}
	return fmt.Errorf("guest health probe timed out after %s (%s): %w", timeout, s.summary(), errors.Join(err, lastErr))
}

func (s *healthProbeStats) failureError(message string, err error) error {
	if s == nil {
		if err == nil {
			return errors.New(message)
		}
		return fmt.Errorf("%s: %w", message, err)
	}
	if err == nil {
		return fmt.Errorf("%s (%s)", message, s.summary())
	}
	return fmt.Errorf("%s (%s): %w", message, s.summary(), err)
}

func (s *healthProbeStats) log(logf func(string, ...interface{}), status string) {
	if s == nil || logf == nil {
		return
	}
	logf("guest health probe %s %s", status, s.summary())
}

func (s *healthProbeStats) logFailedAttempt(logf func(string, ...interface{}), duration time.Duration) {
	if s == nil || logf == nil {
		return
	}
	lastErr := ""
	if s.lastErr != nil {
		lastErr = strings.ReplaceAll(s.lastErr.Error(), "\n", " ")
	}
	logf("guest health probe attempt %s attempt=%d duration_ms=%d bucket=%q error=%q",
		"failed",
		s.attempts,
		vm.RuntimeDurationMilliseconds(duration),
		s.lastBucket,
		lastErr,
	)
}

func (s *healthProbeStats) summary() string {
	if s == nil {
		return ""
	}
	lastErr := ""
	if s.lastErr != nil {
		lastErr = strings.ReplaceAll(s.lastErr.Error(), "\n", " ")
	}
	return fmt.Sprintf("attempts=%d elapsed_ms=%d dial_errors=%d write_errors=%d read_errors=%d status_errors=%d decode_errors=%d close_errors=%d starting_responses=%d machine_exited=%t last_bucket=%q last_status=%q last_error=%q",
		s.attempts,
		vm.RuntimeDurationMilliseconds(s.elapsed()),
		s.dialErrors,
		s.writeErrors,
		s.readErrors,
		s.statusErrors,
		s.decodeErrors,
		s.closeErrors,
		s.startingResponses,
		s.machineExited,
		s.lastBucket,
		s.lastStatus,
		lastErr,
	)
}

func healthProbeErrorBucket(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "write guest health request"):
		return "write"
	case strings.Contains(message, "decode guest health response"):
		return "decode"
	case strings.Contains(message, "guest health returned http"):
		return "status"
	case strings.Contains(message, "read guest health response"):
		return "read"
	default:
		return "unknown"
	}
}
