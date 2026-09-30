package run

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Two connections are the reconciler's whole demand: its singleton lock holds
// one while discovery and each recovery transaction take the other in turn.
func TestLeaseReconcilerRecoversWithinTwoConnections(t *testing.T) {
	f := newPostgresFixture(t)
	lost := f.addRun(t, "starting", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE runs SET status='running',active_started_at=now()-interval '1 second' WHERE id=$1`, lost.runID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE run_leases SET status='running',started_at=claimed_at,start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1`, lost.leaseID)
	config := f.pool.Config()
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewLeaseReconciler(pool, slog.New(slog.DiscardHandler))
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	reconciler.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	joined := false
	defer func() {
		cancel()
		if joined {
			pool.Close()
		}
	}()

	deadline := time.Now().Add(30 * time.Second)
	for {
		var leaseStatus string
		if err := f.pool.QueryRow(t.Context(), `SELECT status::text FROM run_leases WHERE id=$1`, lost.leaseID).Scan(&leaseStatus); err != nil {
			t.Fatal(err)
		}
		if leaseStatus != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost lease was not recovered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		joined = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not join after cancellation")
	}
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("connections still acquired after join: %d", acquired)
	}
	guard, locked, err := pglock.TryAcquire(t.Context(), f.pool, pglock.Key(leaseRecoveryLockName))
	if err != nil || !locked {
		t.Fatalf("singleton lock after join: locked=%v err=%v", locked, err)
	}
	if err := guard.Unlock(); err != nil {
		t.Fatal(err)
	}
}
