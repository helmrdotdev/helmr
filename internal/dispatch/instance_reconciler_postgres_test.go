package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The dispatcher runs both recovery singletons on the run dispatch pool. Each
// holds its lock on one connection, so three connections leave the two
// runners one working connection to share. Settling the failed checkpoint's
// residents needs Run lease recovery before instance reconciliation, so both
// runners must progress on that pool.
func TestLeaseAndInstanceReconcilersProgressOnConstrainedSharedPool(t *testing.T) {
	f, ref, _ := computertest.RegisteredCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000,retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'`)
	if _, err := computer.FailCheckpoint(t.Context(), f.Pool, ref, "snapshot upload failed"); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(ref.InstanceID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computer.RecordInstanceClosed(t.Context(), f.Pool, computer.Closure{
		Observation: computer.Observation{
			Instance: computer.InstanceRef{
				Host: computer.Host{GroupID: pgvalue.MustUUIDValue(i.WorkerGroupID), HostID: pgvalue.MustUUIDValue(i.WorkerHostID), Epoch: i.WorkerEpoch},
				ID:   pgvalue.MustUUIDValue(i.ID), DesiredVersion: i.DesiredVersion,
			},
			ExpectedObservedVersion: i.ObservedVersion,
		},
		Reason: "checkpoint_failed", CleanupProof: &computer.CleanupProof{Method: computer.CleanupHostReconciled, CompletedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	config := f.Pool.Config()
	config.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	authority, err := NewRunAuthority(pool, key)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	leases, err := run.NewLeaseReconciler(pool, log)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	instances, err := NewInstanceReconciler(authority, log)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	instances.interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)
	go func() { done <- leases.Run(ctx) }()
	go func() { done <- instances.Run(ctx) }()
	joined := 0
	defer func() {
		cancel()
		if joined == 2 {
			pool.Close()
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	for {
		var settled int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE status='system_failed' AND current_run_lease_id IS NULL AND failure->>'code'='computer_source_unavailable'`).Scan(&settled); err != nil {
			t.Fatal(err)
		}
		if settled == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settled residents=%d", settled)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	timeout := time.After(30 * time.Second)
	for joined < 2 {
		select {
		case err := <-done:
			joined++
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("runner returned %v", err)
			}
		case <-timeout:
			t.Fatalf("%d of 2 runners joined after cancellation", joined)
		}
	}
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("connections still acquired after join: %d", acquired)
	}
	for _, name := range []string{"helmr.dispatcher.run_resume_recovery", instanceReconciliationLockName} {
		guard, locked, err := pglock.TryAcquire(t.Context(), f.Pool, pglock.Key(name))
		if err != nil || !locked {
			t.Fatalf("%s after join: locked=%v err=%v", name, locked, err)
		}
		if err := guard.Unlock(); err != nil {
			t.Fatal(err)
		}
	}
}
