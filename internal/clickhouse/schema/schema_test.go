package schema

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
)

func TestUpWaitsForReadinessBeforeDDL(t *testing.T) {
	t.Parallel()

	client := &fakeMigrationClient{
		pingErrors: []error{syscall.ECONNREFUSED, io.EOF, nil},
	}
	var backoffs []time.Duration
	policy := testReadinessPolicy(func(_ context.Context, delay time.Duration) error {
		if client.execCalls != 0 {
			t.Fatalf("DDL executed before ClickHouse became ready")
		}
		backoffs = append(backoffs, delay)
		return nil
	})

	if err := up(context.Background(), client, policy); err != nil {
		t.Fatalf("up: %v", err)
	}
	if client.pingCalls != 3 {
		t.Fatalf("Ping calls = %d, want 3", client.pingCalls)
	}
	if client.execCalls == 0 {
		t.Fatal("expected migration DDL to execute after readiness")
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !reflect.DeepEqual(backoffs, want) {
		t.Fatalf("backoffs = %v, want %v", backoffs, want)
	}
}

func TestWaitReadyUsesDeterministicCappedBackoff(t *testing.T) {
	t.Parallel()

	client := &fakeMigrationClient{
		pingErrors: []error{
			syscall.ECONNREFUSED,
			syscall.ECONNREFUSED,
			syscall.ECONNREFUSED,
			syscall.ECONNREFUSED,
			syscall.ECONNREFUSED,
			syscall.ECONNREFUSED,
			nil,
		},
	}
	var backoffs []time.Duration
	policy := testReadinessPolicy(func(_ context.Context, delay time.Duration) error {
		backoffs = append(backoffs, delay)
		return nil
	})

	if err := waitReady(context.Background(), client, policy); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(backoffs, want) {
		t.Fatalf("backoffs = %v, want %v", backoffs, want)
	}
}

func TestWaitReadyFailsFastForServerError(t *testing.T) {
	t.Parallel()

	client := &fakeMigrationClient{pingErrors: []error{&ch.Exception{Code: 516, Message: "authentication failed"}}}
	waits := 0
	policy := testReadinessPolicy(func(context.Context, time.Duration) error {
		waits++
		return nil
	})

	err := waitReady(context.Background(), client, policy)
	if err == nil || !strings.Contains(err.Error(), "readiness probe failed after 1 attempts") {
		t.Fatalf("waitReady error = %v", err)
	}
	if waits != 0 || client.pingCalls != 1 {
		t.Fatalf("waits = %d, Ping calls = %d; want 0 and 1", waits, client.pingCalls)
	}
}

func TestWaitReadyRetriesAttemptTimeout(t *testing.T) {
	t.Parallel()

	client := &fakeMigrationClient{
		ping: func(ctx context.Context, attempt int) error {
			if attempt == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	}
	policy := testReadinessPolicy(func(context.Context, time.Duration) error { return nil })
	policy.attemptTimeout = time.Millisecond

	if err := waitReady(context.Background(), client, policy); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if client.pingCalls != 2 {
		t.Fatalf("Ping calls = %d, want 2", client.pingCalls)
	}
}

func TestWaitReadyHonorsCallerCancellationDuringBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeMigrationClient{pingErrors: []error{syscall.ECONNREFUSED}}
	policy := testReadinessPolicy(func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		<-waitCtx.Done()
		return waitCtx.Err()
	})

	err := waitReady(ctx, client, policy)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady error = %v, want context cancellation", err)
	}
	if client.pingCalls != 1 {
		t.Fatalf("Ping calls = %d, want 1", client.pingCalls)
	}
}

func TestWaitReadyHonorsOverallDeadline(t *testing.T) {
	t.Parallel()

	client := &fakeMigrationClient{pingErrors: []error{syscall.ECONNREFUSED}}
	policy := testReadinessPolicy(waitForReadinessBackoff)
	policy.overallTimeout = 5 * time.Millisecond

	err := waitReady(context.Background(), client, policy)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady error = %v, want overall deadline", err)
	}
	if !strings.Contains(err.Error(), "timed out after 1 attempts") {
		t.Fatalf("waitReady error = %v, want attempt count", err)
	}
}

func TestWaitReadyReportsEarlierCallerDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	client := &fakeMigrationClient{ping: func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	policy := testReadinessPolicy(waitForReadinessBackoff)

	err := waitReady(ctx, client, policy)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady error = %v, want caller deadline", err)
	}
	if !strings.Contains(err.Error(), "caller deadline exceeded after 1 attempts") {
		t.Fatalf("waitReady error = %v, want caller-deadline diagnostic", err)
	}
}

func TestUpDoesNotRetryDDL(t *testing.T) {
	t.Parallel()

	ddlErr := errors.New("invalid DDL")
	client := &fakeMigrationClient{pingErrors: []error{nil}, execErr: ddlErr}
	policy := testReadinessPolicy(func(context.Context, time.Duration) error {
		t.Fatal("unexpected readiness backoff")
		return nil
	})

	err := up(context.Background(), client, policy)
	if !errors.Is(err, ddlErr) {
		t.Fatalf("up error = %v, want DDL error", err)
	}
	if client.execCalls != 1 {
		t.Fatalf("Exec calls = %d, want exactly 1", client.execCalls)
	}
}

func testReadinessPolicy(wait func(context.Context, time.Duration) error) readinessPolicy {
	return readinessPolicy{
		overallTimeout: time.Minute,
		attemptTimeout: time.Second,
		initialBackoff: time.Second,
		maxBackoff:     30 * time.Second,
		wait:           wait,
	}
}

type fakeMigrationClient struct {
	mu         sync.Mutex
	pingErrors []error
	ping       func(context.Context, int) error
	pingCalls  int
	execCalls  int
	execErr    error
}

func (c *fakeMigrationClient) Ping(ctx context.Context) error {
	c.mu.Lock()
	c.pingCalls++
	attempt := c.pingCalls
	ping := c.ping
	var err error
	if len(c.pingErrors) > 0 {
		err = c.pingErrors[0]
		c.pingErrors = c.pingErrors[1:]
	}
	c.mu.Unlock()
	if ping != nil {
		return ping(ctx, attempt)
	}
	return err
}

func (c *fakeMigrationClient) Exec(context.Context, string, ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execCalls++
	return c.execErr
}
