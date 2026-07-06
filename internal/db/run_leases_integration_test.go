package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
)

func TestLeaseRunLeaseRejectsConcurrentLeaseForSameQueueKey(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	firstWorkerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)
	secondWorkerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)

	firstRunID := ids.runID
	secondRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       queue_concurrency_limit = 1,
		       concurrency_key = 'shared-key',
		       current_run_lease_id = NULL,
		       workspace_mount_id = NULL,
		       dispatch_generation = 1,
		       queued_expires_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, firstRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id,
			public_id,
			org_id,
			worker_group_id,
			project_id,
			environment_id,
			deployment_id,
			deployment_task_id,
			workspace_id,
			deployment_version,
			api_version,
			sdk_version,
			cli_version,
			task_id,
			session_id,
			status,
			execution_status,
			payload,
			metadata,
			tags,
			locked_retry_policy,
			queue_name,
			queue_concurrency_limit,
			concurrency_key,
			priority,
			queue_timestamp,
			ttl,
			queued_expires_at,
			requested_milli_cpu,
			requested_memory_mib,
			requested_disk_mib,
			requested_execution_slots,
			runtime_id,
			runtime_arch,
			runtime_abi,
			kernel_digest,
			initramfs_digest,
			rootfs_digest,
			cni_profile,
			network_policy,
			placement,
			max_active_duration_ms,
			trace_id,
			root_span_id,
			current_attempt_number
		)
		SELECT $3,
		       $4,
		       org_id,
		       worker_group_id,
		       project_id,
		       environment_id,
		       deployment_id,
		       deployment_task_id,
		       workspace_id,
		       deployment_version,
		       api_version,
		       sdk_version,
		       cli_version,
		       task_id,
		       session_id,
		       'queued',
		       'queued',
		       payload,
		       metadata,
		       tags,
		       locked_retry_policy,
		       queue_name,
		       1,
		       'shared-key',
		       priority,
		       now(),
		       ttl,
		       NULL,
		       requested_milli_cpu,
		       requested_memory_mib,
		       requested_disk_mib,
		       requested_execution_slots,
		       runtime_id,
		       runtime_arch,
		       runtime_abi,
		       kernel_digest,
		       initramfs_digest,
		       rootfs_digest,
		       cni_profile,
		       network_policy,
		       placement,
		       max_active_duration_ms,
		       trace_id,
		       root_span_id,
		       1
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, firstRunID, secondRunID, testPublicID(t, publicid.Run)); err != nil {
		t.Fatal(err)
	}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tx1.Rollback(context.Background())
	}()
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tx2.Rollback(context.Background())
	}()
	if _, err := db.New(tx1).LeaseRunLease(ctx, leaseRunLeaseParams(ids.orgID, firstRunID, firstWorkerID, "first")); err != nil {
		t.Fatal(err)
	}

	secondResult := make(chan error, 1)
	go func() {
		_, err := db.New(tx2).LeaseRunLease(ctx, leaseRunLeaseParams(ids.orgID, secondRunID, secondWorkerID, "second"))
		secondResult <- err
	}()

	select {
	case err := <-secondResult:
		if err == nil {
			t.Fatal("second lease succeeded before first transaction committed")
		}
		t.Fatalf("second lease returned before first transaction committed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second lease error = %v, want pgx.ErrNoRows", err)
	}
	if err := tx2.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}

	var activeLeases int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM run_leases
		 WHERE org_id = $1
		   AND queue_name = 'default'
		   AND concurrency_key = 'shared-key'
		   AND status IN ('leased', 'running')
		   AND lease_expires_at > now()
	`, ids.orgID).Scan(&activeLeases); err != nil {
		t.Fatal(err)
	}
	if activeLeases != 1 {
		t.Fatalf("active leases = %d, want 1", activeLeases)
	}
}

func leaseRunLeaseParams(orgID, runID, workerID uuid.UUID, label string) db.LeaseRunLeaseParams {
	runLeaseID := uuid.Must(uuid.NewV7())
	return db.LeaseRunLeaseParams{
		WorkerInstanceID:   pgvalue.UUID(workerID),
		OrgID:              pgvalue.UUID(orgID),
		RunID:              pgvalue.UUID(runID),
		DispatchGeneration: 1,
		LeaseExpiresAt:     pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		RunLeaseID:         pgvalue.UUID(runLeaseID),
		DispatchMessageID:  "dispatch-" + label,
		DispatchLeaseID:    "lease-" + label,
		DispatchAttempt:    1,
		RunLeaseSpanID:     "3333333333333333",
	}
}
