package db

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConfirmWorkerInstanceProviderAbsentWaitsForLiveLeaseThenReclaims(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	queries := New(fixture.Pool)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())

	var runtimeID, mountID, baseVersionID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT run_leases.runtime_instance_id, workspace_mounts.id,
		       runs.base_workspace_version_id
		  FROM run_leases
		  JOIN runs ON runs.id = run_leases.run_id
		  JOIN workspace_mounts
		    ON workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
		 WHERE run_leases.id = $1
	`, work.LeaseID).Scan(&runtimeID, &mountID, &baseVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET reserved_run_id = $2, reserved_attempt_number = 1,
		       reserved_workspace_version_id = $3,
		       reservation_expires_at = now() + interval '5 minutes'
		 WHERE id = $1
	`, runtimeID, work.RunID, baseVersionID); err != nil {
		t.Fatal(err)
	}
	workSet, err := queries.ListCapacityWorkerInstances(ctx, ListCapacityWorkerInstancesParams{
		WorkerGroupID:         pgtype.Text{String: runtest.WorkerGroup, Valid: true},
		HasUnreclaimedRuntime: true,
		ResourceIds:           []string{},
		States:                []string{},
		RowLimit:              10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(workSet) != 1 || workSet[0].ID != pgvalue.UUID(fixture.WorkerID) {
		t.Fatalf("provider work set = %+v, want Worker %s", workSet, fixture.WorkerID)
	}
	credentialID := uuid.NewV7()
	if _, err := fixture.Pool.Exec(ctx, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, secret_hash
		) VALUES ($1, $2, $3, $4, $5)
	`, credentialID, runtest.WorkerGroup, fixture.WorkerID, uuid.New().String(), []byte("provider-absence-secret")); err != nil {
		t.Fatal(err)
	}

	first, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != WorkerInstanceStateLost || first.ClaimVersion != 2 || !first.LostAt.Valid {
		t.Fatalf("first provider absence receipt = %+v", first)
	}
	var runtimeState, mountState string
	var reclaimedAt pgtype.Timestamptz
	var reservedRunID pgtype.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT runtime_instances.observed_state, runtime_instances.reclaimed_at,
		       workspace_mounts.state, runtime_instances.reserved_run_id
		  FROM runtime_instances
		  JOIN workspace_mounts ON workspace_mounts.id = $2
		 WHERE runtime_instances.id = $1
	`, runtimeID, mountID).Scan(&runtimeState, &reclaimedAt, &mountState, &reservedRunID); err != nil {
		t.Fatal(err)
	}
	if runtimeState != "lost" || reclaimedAt.Valid || mountState != "lost" || reservedRunID.Valid {
		t.Fatalf("live-lease cleanup = runtime %q reclaimed=%v mount=%q reserved=%v", runtimeState, reclaimedAt.Valid, mountState, reservedRunID.Valid)
	}
	var credentialRevoked bool
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL
		  FROM worker_instance_credentials
		 WHERE id = $1
	`, credentialID).Scan(&credentialRevoked); err != nil {
		t.Fatal(err)
	}
	if !credentialRevoked {
		t.Fatal("provider absence did not revoke the Worker credential")
	}
	if _, err := queries.AuthenticateWorkerInstanceCredential(ctx, AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: pgvalue.UUID(fixture.WorkerID),
		SecretHash:       []byte("provider-absence-secret"),
		ServiceID:        pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("provider-absent credential authentication error = %v, want pgx.ErrNoRows", err)
	}
	var firstObservedVersion int64
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT observed_version FROM runtime_instances WHERE id = $1
	`, runtimeID).Scan(&firstObservedVersion); err != nil {
		t.Fatal(err)
	}
	stableReplay, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	var replayedObservedVersion int64
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT observed_version FROM runtime_instances WHERE id = $1
	`, runtimeID).Scan(&replayedObservedVersion); err != nil {
		t.Fatal(err)
	}
	if stableReplay.ClaimVersion != first.ClaimVersion || replayedObservedVersion != firstObservedVersion {
		t.Fatalf("live-lease replay mutated receipt/runtime: receipt=%+v versions=%d->%d",
			stableReplay, firstObservedVersion, replayedObservedVersion)
	}

	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'lost', terminal_at = now(),
		       terminal_reason_code = 'worker_lost', updated_at = now()
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	replayed, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != first.ClaimVersion || replayed.LostAt != first.LostAt {
		t.Fatalf("replayed provider absence receipt = %+v, want stable %+v", replayed, first)
	}
	var evidence []byte
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaimed_at, reclaim_evidence
		  FROM runtime_instances
		 WHERE id = $1
	`, runtimeID).Scan(&reclaimedAt, &evidence); err != nil {
		t.Fatal(err)
	}
	if !reclaimedAt.Valid || string(evidence) == "" {
		t.Fatalf("replayed cleanup = reclaimed:%v evidence:%s", reclaimedAt.Valid, evidence)
	}
	var method string
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaim_evidence ->> 'method'
		  FROM runtime_instances
		 WHERE id = $1
	`, runtimeID).Scan(&method); err != nil {
		t.Fatal(err)
	}
	if method != "provider_absent" {
		t.Fatalf("reclaim method = %q", method)
	}

	rows, err := queries.ListCapacityWorkerInstances(ctx, ListCapacityWorkerInstancesParams{
		WorkerGroupID:         pgtype.Text{String: runtest.WorkerGroup, Valid: true},
		HasUnreclaimedRuntime: true,
		ResourceIds:           []string{},
		States:                []string{},
		RowLimit:              10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("fully reconciled Worker remained in provider work set: %+v", rows)
	}

	if _, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID); err != nil {
		t.Fatalf("fully repeated provider absence: %v", err)
	}
}

func TestConfirmWorkerInstanceProviderAbsentRejectsTerminalReadyAndUnknownWorker(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE worker_instances
		   SET state = 'termination_ready', draining_at = now(), termination_ready_at = now()
		 WHERE id = $1
	`, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{
		"termination ready": fixture.WorkerID,
		"unknown":           uuid.NewV7(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := confirmProviderAbsent(ctx, fixture.Pool, id); err == nil {
				t.Fatalf("provider absence for %s succeeded", id)
			}
		})
	}
}

func TestConfirmWorkerInstanceProviderAbsentPreservesFailedRuntimeDiagnostics(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	var runtimeID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT runtime_instance_id FROM run_leases WHERE id = $1
	`, work.LeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'lost', terminal_at = now(), terminal_reason_code = 'worker_lost'
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET observed_state = 'failed', observed_version = observed_version + 1,
		       observed_at = now(), terminal_at = now(),
		       terminal_reason_code = 'workspace_mount_failed',
		       terminal_error = '{"code":"preserve-me"}'::jsonb
		 WHERE id = $1
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	var state, reason, code string
	var terminalAt, reclaimedAt pgtype.Timestamptz
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT observed_state, terminal_reason_code, terminal_error ->> 'code',
		       terminal_at, reclaimed_at
		  FROM runtime_instances
		 WHERE id = $1
	`, runtimeID).Scan(&state, &reason, &code, &terminalAt, &reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || reason != "workspace_mount_failed" || code != "preserve-me" ||
		!terminalAt.Valid || !reclaimedAt.Valid {
		t.Fatalf("preserved failed runtime = state:%q reason:%q code:%q terminal:%v reclaimed:%v",
			state, reason, code, terminalAt.Valid, reclaimedAt.Valid)
	}
}

func TestConfirmWorkerInstanceProviderAbsentPreservesLostRuntimeDiagnostics(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	var runtimeID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT runtime_instance_id FROM run_leases WHERE id = $1
	`, work.LeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'lost', terminal_at = now(), terminal_reason_code = 'worker_lost'
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET observed_state = 'lost', observed_version = observed_version + 1,
		       observed_at = now(), terminal_at = now(),
		       terminal_reason_code = 'runtime_lease_lost',
		       terminal_error = '{"code":"keep-lost"}'::jsonb
		 WHERE id = $1
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	var state, reason, code string
	var terminalAt, reclaimedAt pgtype.Timestamptz
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT observed_state, terminal_reason_code, terminal_error ->> 'code',
		       terminal_at, reclaimed_at
		  FROM runtime_instances
		 WHERE id = $1
	`, runtimeID).Scan(&state, &reason, &code, &terminalAt, &reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if state != "lost" || reason != "runtime_lease_lost" || code != "keep-lost" ||
		!terminalAt.Valid || !reclaimedAt.Valid {
		t.Fatalf("preserved lost runtime = state:%q reason:%q code:%q terminal:%v reclaimed:%v",
			state, reason, code, terminalAt.Valid, reclaimedAt.Valid)
	}
}

func TestConfirmWorkerInstanceProviderAbsentSeesLeaseGrantedBeforeWorkerLockRelease(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	var runtimeID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT runtime_instance_id FROM run_leases WHERE id = $1
	`, work.LeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		DELETE FROM workspace_leases WHERE owner_run_lease_id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE runs SET current_run_lease_id = NULL WHERE id = $1
	`, work.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `DELETE FROM run_leases WHERE id = $1`, work.LeaseID); err != nil {
		t.Fatal(err)
	}

	grantTx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = grantTx.Rollback(context.Background()) }()
	if _, err := grantTx.Exec(ctx, `
		SELECT id FROM worker_instances WHERE id = $1 FOR UPDATE
	`, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := grantTx.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, runtime_identity_id,
			requested_cpu_millis, requested_memory_bytes,
			requested_guest_ephemeral_disk_bytes, requested_execution_slots,
			state, assigned_at, start_deadline_at, expires_at
		)
		SELECT $1, runs.org_id, runs.project_id, runs.environment_id, runs.id,
		       runs.workspace_id, $2, 1, 1, $3, runtime_instances.worker_instance_id,
		       runtime_instances.worker_epoch, runtime_instances.id,
		       runtime_instances.runtime_identity_id,
		       runtime_instances.reserved_cpu_millis,
		       runtime_instances.reserved_memory_bytes,
		       runtime_instances.reserved_guest_ephemeral_disk_bytes,
		       runtime_instances.reserved_execution_slots,
		       'assigned', now(), now() + interval '5 minutes', now() + interval '10 minutes'
		  FROM runs
		  JOIN runtime_instances ON runtime_instances.id = $4
		 WHERE runs.id = $5
	`, work.LeaseID, runtest.Region, runtest.WorkerGroup, runtimeID, work.RunID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		row ConfirmWorkerInstanceProviderAbsentRow
		err error
	}
	done := make(chan result, 1)
	go func() {
		row, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID)
		done <- result{row: row, err: err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err := fixture.Pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				  FROM pg_stat_activity
				 WHERE query LIKE '%ConfirmWorkerInstanceProviderAbsent%'
				   AND wait_event_type = 'Lock'
			)
		`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider absence did not block on the grant-owned Worker lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := grantTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.row.State != WorkerInstanceStateLost {
			t.Fatalf("provider absence receipt = %+v", got.row)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider absence did not complete after grant commit")
	}
	var reclaimedAt pgtype.Timestamptz
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaimed_at FROM runtime_instances WHERE id = $1
	`, runtimeID).Scan(&reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if reclaimedAt.Valid {
		t.Fatal("provider absence reclaimed a Runtime with a concurrently granted live Lease")
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'lost', terminal_at = now(), terminal_reason_code = 'worker_lost'
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaimed_at FROM runtime_instances WHERE id = $1
	`, runtimeID).Scan(&reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if !reclaimedAt.Valid {
		t.Fatal("provider absence replay did not reclaim Runtime after Lease terminalization")
	}
}

func confirmProviderAbsent(ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID) (ConfirmWorkerInstanceProviderAbsentRow, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ConfirmWorkerInstanceProviderAbsentRow{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	queries := New(tx)
	row, err := queries.ConfirmWorkerInstanceProviderAbsent(ctx, pgvalue.UUID(workerID))
	if err != nil {
		return ConfirmWorkerInstanceProviderAbsentRow{}, err
	}
	if _, err := queries.ReconcileProviderAbsentWorkerRuntimes(ctx, pgvalue.UUID(workerID)); err != nil {
		return ConfirmWorkerInstanceProviderAbsentRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfirmWorkerInstanceProviderAbsentRow{}, err
	}
	return row, nil
}
