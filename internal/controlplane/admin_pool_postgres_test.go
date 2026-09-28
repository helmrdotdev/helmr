package controlplane

import (
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminWorkerPoolPostgresGroupDrainClearsPrimariesAtomically(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	fixture := newAdminPoolPostgresFixture(t, database.Pool, "us-east-1")
	pool := fixture.addActivePool(t, "current")

	group, err := fixture.q.SetInitialWorkerGroupPrimaryPool(t.Context(), db.SetInitialWorkerGroupPrimaryPoolParams{
		WorkerGroupID: fixture.group.ID,
		WorkerPoolID:  pool.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.PrimaryPoolID != pool.ID || group.ClaimVersion != fixture.group.ClaimVersion+1 {
		t.Fatalf("initial primary = pool:%v claim:%d, want %v/%d",
			group.PrimaryPoolID, group.ClaimVersion,
			pool.ID, fixture.group.ClaimVersion+1)
	}
	activationReplay, err := fixture.q.SetInitialWorkerGroupPrimaryPool(t.Context(), db.SetInitialWorkerGroupPrimaryPoolParams{
		WorkerGroupID: fixture.group.ID,
		WorkerPoolID:  pool.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if activationReplay.PrimaryPoolID != pool.ID || activationReplay.ClaimVersion != group.ClaimVersion {
		t.Fatalf("initial primary activation replay = %+v", activationReplay)
	}
	replacement := fixture.addActivePool(t, "replacement")
	afterReplacementSeal, err := fixture.q.SetInitialWorkerGroupPrimaryPool(t.Context(), db.SetInitialWorkerGroupPrimaryPoolParams{
		WorkerGroupID: fixture.group.ID,
		WorkerPoolID:  replacement.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if afterReplacementSeal.PrimaryPoolID != pool.ID || afterReplacementSeal.ClaimVersion != group.ClaimVersion {
		t.Fatalf("primaries after replacement seal = %+v", afterReplacementSeal)
	}

	status, err := workergroup.BeginGroupDrain(t.Context(), fixture.q, pgvalue.MustUUIDValue(group.ID), group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != db.WorkerGroupStatusDraining || status.ClaimVersion != group.ClaimVersion+1 || !status.TransitionApplied {
		t.Fatalf("group drain status = %+v", status)
	}
	draining, err := fixture.q.GetWorkerGroup(t.Context(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draining.Status != db.WorkerGroupStatusDraining || draining.ClaimVersion != group.ClaimVersion+1 ||
		draining.PrimaryPoolID.Valid {
		t.Fatalf("draining group = %+v", draining)
	}

	replay, err := workergroup.BeginGroupDrain(t.Context(), fixture.q, pgvalue.MustUUIDValue(group.ID), group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != db.WorkerGroupStatusDraining || replay.ClaimVersion != draining.ClaimVersion || replay.TransitionApplied {
		t.Fatalf("group drain replay = %+v", replay)
	}

	drained, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetStatus:             "draining",
		WorkerPoolID:             pool.ID,
		WorkerGroupID:            group.ID,
		ExpectedPoolClaimVersion: pool.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if drained.Status != "draining" || drained.ClaimVersion != pool.ClaimVersion+1 {
		t.Fatalf("drained pool = %+v", drained)
	}
}

func TestWorkerGroupPrimarySelectionPostgresIsAtomicAndReplaySafe(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	fixture := newAdminPoolPostgresFixture(t, database.Pool, "us-east-1")
	pool := fixture.addActivePool(t, "current")
	server := &Server{db: fixture.q, tx: fixture.pool}
	command := workerGroupPrimarySelectionCommand{
		workerGroupID:             pgvalue.MustUUIDValue(fixture.group.ID),
		expectedGroupClaimVersion: fixture.group.ClaimVersion,
		desired: func(db.WorkerGroup) (pgtype.UUID, error) {
			return pool.ID, nil
		},
	}
	result, err := server.reconcileWorkerGroupPrimarySelection(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !result.applied || result.group.ClaimVersion != fixture.group.ClaimVersion+1 ||
		result.group.PrimaryPoolID != pool.ID {
		t.Fatalf("primary result = %+v", result)
	}
	stored, err := fixture.q.GetWorkerGroup(t.Context(), fixture.group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClaimVersion != result.group.ClaimVersion || stored.PrimaryPoolID != pool.ID {
		t.Fatalf("stored primary selection = %+v", stored)
	}

	replay, err := server.reconcileWorkerGroupPrimarySelection(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if replay.applied || replay.group.ClaimVersion != result.group.ClaimVersion {
		t.Fatalf("replay = %+v", replay)
	}
	command.expectedGroupClaimVersion = result.group.ClaimVersion + 1
	if _, err := server.reconcileWorkerGroupPrimarySelection(t.Context(), command); err == nil {
		t.Fatal("future primary-selection claim succeeded")
	}
}

func TestWorkerGroupPrimarySelectionPostgresSerializesCompetingControllers(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	fixture := newAdminPoolPostgresFixture(t, database.Pool, "us-east-1")
	first := fixture.addActivePool(t, "first")
	second := fixture.addActivePool(t, "second")
	server := &Server{db: fixture.q, tx: fixture.pool}
	commands := []workerGroupPrimarySelectionCommand{
		{
			workerGroupID: pgvalue.MustUUIDValue(fixture.group.ID), expectedGroupClaimVersion: fixture.group.ClaimVersion,
			desired: func(db.WorkerGroup) (pgtype.UUID, error) {
				return first.ID, nil
			},
		},
		{
			workerGroupID: pgvalue.MustUUIDValue(fixture.group.ID), expectedGroupClaimVersion: fixture.group.ClaimVersion,
			desired: func(db.WorkerGroup) (pgtype.UUID, error) {
				return second.ID, nil
			},
		},
	}
	start := make(chan struct{})
	errorsByController := make([]error, len(commands))
	var wait sync.WaitGroup
	for index := range commands {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, errorsByController[index] = server.reconcileWorkerGroupPrimarySelection(t.Context(), commands[index])
		}(index)
	}
	close(start)
	wait.Wait()
	succeeded := 0
	for _, err := range errorsByController {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("controller errors = %v, want exactly one success", errorsByController)
	}
	stored, err := fixture.q.GetWorkerGroup(t.Context(), fixture.group.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstWon := stored.PrimaryPoolID == first.ID
	secondWon := stored.PrimaryPoolID == second.ID
	if stored.ClaimVersion != fixture.group.ClaimVersion+1 || firstWon == secondWon {
		t.Fatalf("stored competing primary selection = %+v", stored)
	}
}

func TestAdminWorkerPoolPostgresDisablesUnreferencedPendingPool(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	fixture := newAdminPoolPostgresFixture(t, database.Pool, "us-east-1")
	pending, err := fixture.q.CreatePendingWorkerPool(t.Context(), db.CreatePendingWorkerPoolParams{
		WorkerPoolID:              pgvalue.NewUUIDv7(),
		Name:                      "unused",
		WorkerGroupID:             fixture.group.ID,
		ExpectedGroupClaimVersion: fixture.group.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetStatus:             "disabled",
		WorkerPoolID:             pending.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: pending.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool = %+v", disabled)
	}
}

func TestAdminWorkerPoolPostgresDisablesPendingPoolWithOnlyLostWorker(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	fixture := newAdminPoolPostgresFixture(t, database.Pool, "us-east-1")
	pending, err := fixture.q.CreatePendingWorkerPool(t.Context(), db.CreatePendingWorkerPoolParams{
		WorkerPoolID:              pgvalue.NewUUIDv7(),
		Name:                      "lost-before-activation",
		WorkerGroupID:             fixture.group.ID,
		ExpectedGroupClaimVersion: fixture.group.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerID := pgvalue.NewUUIDv7()
	dbtest.MustExec(t, t.Context(), fixture.pool, `
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id, status
) VALUES ($1, 'lost-before-activation', $2, $3, 'registering')`,
		workerID, fixture.group.ID, pending.ID)
	if _, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetStatus:             "disabled",
		WorkerPoolID:             pending.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: pending.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disable pending pool with registering worker error = %v, want no rows", err)
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `
UPDATE worker_hosts
   SET status = 'lost', lost_at = now()
 WHERE id = $1`, workerID)

	disabled, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetStatus:             "disabled",
		WorkerPoolID:             pending.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: pending.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool with lost worker = %+v", disabled)
	}
}

func waitForPostgresBlock(t *testing.T, pool *pgxpool.Pool, backendPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := pool.QueryRow(t.Context(), `
SELECT cardinality(pg_blocking_pids($1)) > 0`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for concurrent pool drain to block on checkpoint lifecycle lock")
}

type adminPoolPostgresFixture struct {
	pool            *pgxpool.Pool
	q               *db.Queries
	group           db.WorkerGroup
	vmPlatformID    string
	cpuConfigDigest string
}

func newAdminPoolPostgresFixture(t *testing.T, pool *pgxpool.Pool, regionID string) adminPoolPostgresFixture {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `
INSERT INTO regions (id, display_name)
VALUES ($1, 'Worker Pool Test')
ON CONFLICT (id) DO NOTHING`, regionID)
	q := db.New(pool)
	group, err := q.CreateWorkerGroup(t.Context(), db.CreateWorkerGroupParams{
		ID:          pgvalue.UUID(uuid.NewV7()),
		RegionID:    regionID,
		Name:        "worker-pool-test",
		Description: "",
		TokenID:     pgvalue.NewUUIDv7(),
		TokenHash:   dbtest.Hash(uuid.New().String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := adminPoolPostgresFixture{
		pool: pool, q: q, group: group,
		vmPlatformID:    dbtest.Digest("worker-pool-runtime"),
		cpuConfigDigest: dbtest.Digest("worker-pool-cpu"),
	}
	_, err = q.UpsertVMPlatform(t.Context(), db.UpsertVMPlatformParams{
		ID:                    fixture.vmPlatformID,
		Arch:                  "x86_64",
		Contract:              vmplatform.Contract,
		DescriptorDigest:      dbtest.Digest("worker-pool-runtime-descriptor"),
		FirecrackerDigest:     dbtest.Digest("worker-pool-firecracker"),
		FirecrackerVersion:    "1.12.0",
		SnapshotFormatVersion: "1.0.0",
		HostKernelRelease:     "6.12.0",
		CPUTemplateKind:       "none",
		KernelDigest:          dbtest.Digest("worker-pool-kernel"),
		InitramfsDigest:       dbtest.Digest("worker-pool-initramfs"),
		RootfsDigest:          dbtest.Digest("worker-pool-rootfs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture adminPoolPostgresFixture) addActivePool(t *testing.T, name string) db.WorkerPool {
	t.Helper()
	group, err := fixture.q.GetWorkerGroup(t.Context(), fixture.group.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := fixture.q.CreatePendingWorkerPool(t.Context(), db.CreatePendingWorkerPoolParams{
		WorkerPoolID:              pgvalue.NewUUIDv7(),
		Name:                      name,
		WorkerGroupID:             fixture.group.ID,
		ExpectedGroupClaimVersion: group.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := fixture.q.InsertWorkerPoolCPUShape(t.Context(), db.InsertWorkerPoolCPUShapeParams{
		VCPUCount: 1, CPUConfigDigest: fixture.cpuConfigDigest, WorkerPoolID: pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("inserted CPU shape rows = %d, want 1", rows)
	}
	sealed, err := fixture.q.SealWorkerPool(t.Context(), db.SealWorkerPoolParams{
		VMPlatformID:                    pgvalue.Text(fixture.vmPlatformID),
		CapacityCPUMillis:               pgtype.Int8{Int64: 4_000, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: 8 << 30, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: 32 << 30, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: 1_000, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: 1 << 30, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: 4 << 30, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: 4, Valid: true},
		WorkerPoolID:                    pending.ID,
		WorkerGroupID:                   fixture.group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

type adminPoolLiveRuntime struct {
	workerID  uuid.UUID
	runtimeID uuid.UUID
}

func seedLiveRuntimeForWorkerPool(
	t *testing.T,
	product actorStartPostgresFixture,
	fixture adminPoolPostgresFixture,
	pool db.WorkerPool,
) adminPoolLiveRuntime {
	t.Helper()
	var computerSpecID uuid.UUID
	if err := product.pool.QueryRow(t.Context(), `SELECT computer_spec_id FROM computers WHERE id=$1`, product.computerIDs[0]).Scan(&computerSpecID); err != nil {
		t.Fatal(err)
	}
	live := adminPoolLiveRuntime{
		workerID:  uuid.NewV7(),
		runtimeID: uuid.NewV7(),
	}
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id, status,
    current_epoch, current_service_id, vm_platform_id,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_vm_starts,
    cpu_environment, cpu_environment_digest, observed_at,
    epoch_started_at, activated_at
) VALUES (
    $1, 'live-runtime-worker', $2, $3, 'active',
    1, $4, $5,
    4000, 8589934592, 34359738368,
    1000, 1073741824, 4294967296,
    4, 4, '{"vendor":"test"}'::jsonb, $6, now(), now(), now()
)`, live.workerID, fixture.group.ID, pool.ID, uuid.NewV7(), fixture.vmPlatformID,
		dbtest.Digest("live-runtime-cpu-environment"))
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO computer_instances (
    id, preparation_expires_at, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_host_id, vm_platform_id,
        computer_spec_id,
    worker_epoch, vm_vcpu_count, cpu_config_digest,
    reserved_cpu_millis, reserved_memory_bytes,
    reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
    computer_id, desired_reason
) VALUES (
    $1, transaction_timestamp() + interval '5 minutes', $2, $3, $4, $5, 'us-east-1', $6, $7,
        $8,
    1, 1, $9, 1000, 1073741824, 4294967296, 1, $10, 'placed'
)`, live.runtimeID, product.orgID, fixture.group.ID, product.projectID,
		product.environmentID, live.workerID, fixture.vmPlatformID,
		computerSpecID, fixture.cpuConfigDigest, product.computerIDs[0])
	return live
}

func finishLiveRuntimeForWorkerPool(t *testing.T, pool *pgxpool.Pool, live adminPoolLiveRuntime) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `
UPDATE computer_instances
   SET desired_state = 'closed',
       desired_version = 2,
       desired_reason = 'drained',
       observed_state = 'closed',
       observed_version = 1,
       observed_desired_version = 2,
       observed_at = now(),
       terminal_at = now(),
       terminal_reason_code = 'drained',
       reclaimed_at = now(),
       reclaim_evidence = '{"method":"drained"}'::jsonb,
       updated_at = now()
 WHERE id = $1`, live.runtimeID)
	dbtest.MustExec(t, t.Context(), pool, `
UPDATE worker_hosts
   SET status = 'termination_ready',
       claim_version = claim_version + 1,
       draining_at = now(),
       termination_ready_at = now(),
       updated_at = now()
 WHERE id = $1`, live.workerID)
}

type adminPoolCheckpoint struct {
	workerID                  uuid.UUID
	runtimeID                 uuid.UUID
	runLeaseID                uuid.UUID
	checkpointID              uuid.UUID
	baseComputerDiskVersionID uuid.UUID
}
