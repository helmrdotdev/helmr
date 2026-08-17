package schema

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/clickhouse"
)

// Migrations contains ClickHouse DDL for the managed-cloud telemetry store.
//
//go:embed migrations/*.sql
var Migrations embed.FS

func Up(ctx context.Context, cfg clickhouse.Config) error {
	client, err := clickhouse.New(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	return up(ctx, client, defaultReadinessPolicy)
}

type migrationClient interface {
	Ping(context.Context) error
	Exec(context.Context, string, ...any) error
}

type readinessPolicy struct {
	overallTimeout time.Duration
	attemptTimeout time.Duration
	initialBackoff time.Duration
	maxBackoff     time.Duration
	wait           func(context.Context, time.Duration) error
}

var defaultReadinessPolicy = readinessPolicy{
	overallTimeout: 5 * time.Minute,
	attemptTimeout: 30 * time.Second,
	initialBackoff: time.Second,
	maxBackoff:     30 * time.Second,
	wait:           waitForReadinessBackoff,
}

func up(ctx context.Context, client migrationClient, policy readinessPolicy) error {
	if err := waitReady(ctx, client, policy); err != nil {
		return err
	}
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := Migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		for _, statement := range splitStatements(string(content)) {
			if err := client.Exec(ctx, statement); err != nil {
				return fmt.Errorf("apply clickhouse migration %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func waitReady(ctx context.Context, client migrationClient, policy readinessPolicy) error {
	if err := validateReadinessPolicy(policy); err != nil {
		return err
	}

	readyCtx, cancel := context.WithTimeout(ctx, policy.overallTimeout)
	defer cancel()

	attempts := 0
	backoff := policy.initialBackoff
	var lastErr error
	for {
		attempts++
		attemptCtx, cancelAttempt := context.WithTimeout(readyCtx, policy.attemptTimeout)
		err := client.Ping(attemptCtx)
		cancelAttempt()
		if err == nil {
			return nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return callerReadinessError(attempts, ctx.Err(), lastErr)
		}
		if readyCtx.Err() != nil {
			return fmt.Errorf("wait for clickhouse readiness timed out after %d attempts: %w", attempts, errors.Join(readyCtx.Err(), lastErr))
		}
		if !clickhouse.IsReadinessErrorRetryable(err) {
			return fmt.Errorf("clickhouse readiness probe failed after %d attempts: %w", attempts, err)
		}

		if err := policy.wait(readyCtx, backoff); err != nil {
			if ctx.Err() != nil {
				return callerReadinessError(attempts, ctx.Err(), lastErr)
			}
			return fmt.Errorf("wait for clickhouse readiness timed out after %d attempts: %w", attempts, errors.Join(readyCtx.Err(), lastErr))
		}
		if backoff < policy.maxBackoff {
			backoff *= 2
			if backoff > policy.maxBackoff {
				backoff = policy.maxBackoff
			}
		}
	}
}

func callerReadinessError(attempts int, callerErr, lastErr error) error {
	if errors.Is(callerErr, context.DeadlineExceeded) {
		return fmt.Errorf("wait for clickhouse readiness caller deadline exceeded after %d attempts: %w", attempts, errors.Join(callerErr, lastErr))
	}
	return fmt.Errorf("wait for clickhouse readiness canceled by caller after %d attempts: %w", attempts, errors.Join(callerErr, lastErr))
}

func validateReadinessPolicy(policy readinessPolicy) error {
	if policy.overallTimeout <= 0 {
		return fmt.Errorf("clickhouse readiness overall timeout must be positive")
	}
	if policy.attemptTimeout <= 0 {
		return fmt.Errorf("clickhouse readiness attempt timeout must be positive")
	}
	if policy.initialBackoff <= 0 {
		return fmt.Errorf("clickhouse readiness initial backoff must be positive")
	}
	if policy.maxBackoff < policy.initialBackoff {
		return fmt.Errorf("clickhouse readiness max backoff must be at least the initial backoff")
	}
	if policy.wait == nil {
		return fmt.Errorf("clickhouse readiness wait function is required")
	}
	return nil
}

func waitForReadinessBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func splitStatements(content string) []string {
	parts := strings.Split(content, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}
