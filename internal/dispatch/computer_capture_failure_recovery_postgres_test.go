package dispatch_test

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
)

func TestComputerCheckpointFailureSettlesRetryingResidents(t *testing.T) {
	f, ref, _ := computertest.RegisteredCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000,retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'`)
	var originalInstances int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_instances`).Scan(&originalInstances); err != nil {
		t.Fatal(err)
	}
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
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := run.RecoverExecutionLeases(t.Context(), f.Pool, 10); err != nil || n != 2 {
		t.Fatalf("resident recovery=%d err=%v", n, err)
	}
	var retries int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE status='retry_delayed' AND current_run_lease_id IS NULL`).Scan(&retries); err != nil || retries != 2 {
		t.Fatalf("retry candidates=%d err=%v", retries, err)
	}
	for range 2 {
		if _, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var settled int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE status='system_failed' AND current_run_lease_id IS NULL AND failure->>'code'='computer_source_unavailable'`).Scan(&settled); err != nil || settled != 2 {
		t.Fatalf("settled residents=%d err=%v", settled, err)
	}
	var replacements int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_instances`).Scan(&replacements); err != nil || replacements != originalInstances {
		t.Fatalf("stale-head replacements=%d err=%v", replacements, err)
	}
}
