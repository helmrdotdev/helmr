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

func TestConfirmWorkerHostProviderAbsentReclaimsIndependentlyOfLiveLease(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	queries := New(fixture.Pool)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())

	var instanceID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `SELECT computer_instance_id FROM run_leases WHERE id = $1`, work.LeaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	workSet, err := queries.ListCapacityWorkerHosts(ctx, ListCapacityWorkerHostsParams{
		WorkerGroupID:         pgvalue.UUID(runtest.WorkerGroupID),
		HasUnreclaimedRuntime: true,
		ResourceIds:           []string{},
		Statuses:              []string{},
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
		INSERT INTO worker_host_credentials (
			id, worker_group_id, worker_host_id, key_prefix, secret_hash
		) VALUES ($1, $2, $3, $4, $5)
	`, credentialID, runtest.WorkerGroup, fixture.WorkerID, uuid.New().String(), []byte("provider-absence-secret")); err != nil {
		t.Fatal(err)
	}

	first, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != WorkerHostStatusLost || first.ClaimVersion != 2 || !first.LostAt.Valid {
		t.Fatalf("first provider absence receipt = %+v", first)
	}
	var instanceState, mountStatus, admissionState, leaseStatus string
	var reclaimedAt pgtype.Timestamptz
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT i.observed_state, i.reclaimed_at, i.mount_state, i.admission_state, l.status
		FROM computer_instances i JOIN run_leases l ON l.computer_instance_id = i.id
		WHERE l.id = $1
	`, work.LeaseID).Scan(&instanceState, &reclaimedAt, &mountStatus, &admissionState, &leaseStatus); err != nil {
		t.Fatal(err)
	}
	if instanceState != "lost" || !reclaimedAt.Valid || mountStatus != "lost" || admissionState != "closed" || leaseStatus != "assigned" {
		t.Fatalf("physical exclusion = state %q reclaimed=%v mount=%q admission=%q logical lease=%q", instanceState, reclaimedAt.Valid, mountStatus, admissionState, leaseStatus)
	}

	var processReconciled bool
	if err := fixture.Pool.QueryRow(ctx, `SELECT process_reconciled_at=$2 FROM run_leases WHERE id=$1`, work.LeaseID, reclaimedAt).Scan(&processReconciled); err != nil || !processReconciled {
		t.Fatalf("provider exclusion left process unreconciled: %v %v", processReconciled, err)
	}
	var credentialRevoked bool
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL
		  FROM worker_host_credentials
		 WHERE id = $1
	`, credentialID).Scan(&credentialRevoked); err != nil {
		t.Fatal(err)
	}
	if !credentialRevoked {
		t.Fatal("provider absence did not revoke the Worker credential")
	}
	if _, err := queries.AuthenticateWorkerHostCredential(ctx, AuthenticateWorkerHostCredentialParams{
		WorkerHostID: pgvalue.UUID(fixture.WorkerID),
		SecretHash:   []byte("provider-absence-secret"),
		ServiceID:    pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("provider-absent credential authentication error = %v, want pgx.ErrNoRows", err)
	}
	var firstObservedVersion int64
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT observed_version FROM computer_instances WHERE id = $1
	`, instanceID).Scan(&firstObservedVersion); err != nil {
		t.Fatal(err)
	}
	stableReplay, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	var replayedObservedVersion int64
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT observed_version FROM computer_instances WHERE id = $1
	`, instanceID).Scan(&replayedObservedVersion); err != nil {
		t.Fatal(err)
	}
	if stableReplay.ClaimVersion != first.ClaimVersion || replayedObservedVersion != firstObservedVersion {
		t.Fatalf("live-lease replay mutated receipt/instance: receipt=%+v versions=%d->%d",
			stableReplay, firstObservedVersion, replayedObservedVersion)
	}

	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET status = 'lost', terminal_at = now(),
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
		  FROM computer_instances
		 WHERE id = $1
	`, instanceID).Scan(&reclaimedAt, &evidence); err != nil {
		t.Fatal(err)
	}
	if !reclaimedAt.Valid || string(evidence) == "" {
		t.Fatalf("replayed cleanup = reclaimed:%v evidence:%s", reclaimedAt.Valid, evidence)
	}
	var method string
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaim_evidence ->> 'method'
		  FROM computer_instances
		 WHERE id = $1
	`, instanceID).Scan(&method); err != nil {
		t.Fatal(err)
	}
	if method != "provider_absent" {
		t.Fatalf("reclaim method = %q", method)
	}

	rows, err := queries.ListCapacityWorkerHosts(ctx, ListCapacityWorkerHostsParams{
		WorkerGroupID:         pgvalue.UUID(runtest.WorkerGroupID),
		HasUnreclaimedRuntime: true,
		ResourceIds:           []string{},
		Statuses:              []string{},
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

func TestConfirmWorkerHostProviderAbsentRejectsTerminalReadyAndUnknownWorker(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE worker_hosts
		   SET status = 'termination_ready', draining_at = now(), termination_ready_at = now()
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

func TestConfirmWorkerHostProviderAbsentPreservesFailedInstanceDiagnostics(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	var instanceID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT computer_instance_id FROM run_leases WHERE id = $1
	`, work.LeaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET status = 'lost', terminal_at = now(), terminal_reason_code = 'worker_lost'
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE computer_instances
		   SET observed_state = 'failed', observed_version = observed_version + 1,
		       observed_at = now(), terminal_at = now(),
		       terminal_reason_code = 'computer_mount_failed',
		       terminal_error = '{"code":"preserve-me"}'::jsonb
		 WHERE id = $1
	`, instanceID); err != nil {
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
		  FROM computer_instances
		 WHERE id = $1
	`, instanceID).Scan(&state, &reason, &code, &terminalAt, &reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || reason != "computer_mount_failed" || code != "preserve-me" ||
		!terminalAt.Valid || !reclaimedAt.Valid {
		t.Fatalf("preserved failed instance = state:%q reason:%q code:%q terminal:%v reclaimed:%v",
			state, reason, code, terminalAt.Valid, reclaimedAt.Valid)
	}
}

func TestConfirmWorkerHostProviderAbsentPreservesLostInstanceDiagnostics(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	var instanceID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT computer_instance_id FROM run_leases WHERE id = $1
	`, work.LeaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET status = 'lost', terminal_at = now(), terminal_reason_code = 'worker_lost'
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE computer_instances
		   SET observed_state = 'lost', observed_version = observed_version + 1,
		       observed_at = now(), terminal_at = now(),
		       terminal_reason_code = 'computer_instance_lost',
		       terminal_error = '{"code":"keep-lost"}'::jsonb
		 WHERE id = $1
	`, instanceID); err != nil {
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
		  FROM computer_instances
		 WHERE id = $1
	`, instanceID).Scan(&state, &reason, &code, &terminalAt, &reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if state != "lost" || reason != "computer_instance_lost" || code != "keep-lost" ||
		!terminalAt.Valid || !reclaimedAt.Valid {
		t.Fatalf("preserved lost instance = state:%q reason:%q code:%q terminal:%v reclaimed:%v",
			state, reason, code, terminalAt.Valid, reclaimedAt.Valid)
	}
}

func TestConfirmWorkerHostProviderAbsentSeesLeaseGrantedBeforeWorkerLockRelease(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	var instanceID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT computer_instance_id FROM run_leases WHERE id = $1
	`, work.LeaseID).Scan(&instanceID); err != nil {
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
		SELECT id FROM worker_hosts WHERE id = $1 FOR UPDATE
	`, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := grantTx.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, computer_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_host_id,
			worker_epoch, computer_instance_id, writer_generation, deployment_id,
			requested_cpu_millis, requested_memory_bytes,
			requested_guest_ephemeral_disk_bytes, requested_execution_slots,
			status, created_at, start_deadline_at, expires_at
		)
		SELECT $1, runs.org_id, runs.project_id, runs.environment_id, runs.id,
		       runs.computer_id, $2, 1, 1, $3, computer_instances.worker_host_id,
		       computer_instances.worker_epoch, computer_instances.id, computer_instances.writer_generation, runs.deployment_id,
		       computer_instances.reserved_cpu_millis,
		       computer_instances.reserved_memory_bytes,
		       computer_instances.reserved_guest_ephemeral_disk_bytes,
		       computer_instances.reserved_execution_slots,
		       'assigned', now(), now() + interval '5 minutes', now() + interval '10 minutes'
		  FROM runs
		  JOIN computer_instances ON computer_instances.id = $4
		 WHERE runs.id = $5
	`, work.LeaseID, runtest.Region, runtest.WorkerGroup, instanceID, work.RunID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		row ConfirmWorkerHostProviderAbsentRow
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
				 WHERE query LIKE '%ConfirmWorkerHostProviderAbsent%'
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
		if got.row.Status != WorkerHostStatusLost {
			t.Fatalf("provider absence receipt = %+v", got.row)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider absence did not complete after grant commit")
	}
	var reclaimedAt pgtype.Timestamptz
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaimed_at FROM computer_instances WHERE id = $1
	`, instanceID).Scan(&reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if !reclaimedAt.Valid {
		t.Fatal("provider absence did not reclaim the Instance after the concurrent grant")
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_leases
		   SET status = 'lost', terminal_at = now(), terminal_reason_code = 'worker_lost'
		 WHERE id = $1
	`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := confirmProviderAbsent(ctx, fixture.Pool, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Pool.QueryRow(ctx, `
		SELECT reclaimed_at FROM computer_instances WHERE id = $1
	`, instanceID).Scan(&reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if !reclaimedAt.Valid {
		t.Fatal("provider absence replay did not reclaim Instance after Lease terminalization")
	}
}

func confirmProviderAbsent(ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID) (ConfirmWorkerHostProviderAbsentRow, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ConfirmWorkerHostProviderAbsentRow{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	queries := New(tx)
	row, err := queries.ConfirmWorkerHostProviderAbsent(ctx, pgvalue.UUID(workerID))
	if err != nil {
		return ConfirmWorkerHostProviderAbsentRow{}, err
	}
	if _, err := queries.ReconcileProviderAbsentWorkerInstances(ctx, pgvalue.UUID(workerID)); err != nil {
		return ConfirmWorkerHostProviderAbsentRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfirmWorkerHostProviderAbsentRow{}, err
	}
	return row, nil
}
