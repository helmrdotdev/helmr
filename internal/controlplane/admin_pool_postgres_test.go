package controlplane

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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

	status, err := workergroup.BeginGroupDrain(t.Context(), fixture.q, group.ID, group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != db.WorkerGroupStateDraining || status.ClaimVersion != group.ClaimVersion+1 || !status.TransitionApplied {
		t.Fatalf("group drain status = %+v", status)
	}
	draining, err := fixture.q.GetWorkerGroup(t.Context(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draining.State != db.WorkerGroupStateDraining || draining.ClaimVersion != group.ClaimVersion+1 ||
		draining.PrimaryPoolID.Valid {
		t.Fatalf("draining group = %+v", draining)
	}

	replay, err := workergroup.BeginGroupDrain(t.Context(), fixture.q, group.ID, group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if replay.State != db.WorkerGroupStateDraining || replay.ClaimVersion != draining.ClaimVersion || replay.TransitionApplied {
		t.Fatalf("group drain replay = %+v", replay)
	}

	drained, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "draining",
		WorkerPoolID:             pool.ID,
		WorkerGroupID:            group.ID,
		ExpectedPoolClaimVersion: pool.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if drained.State != "draining" || drained.ClaimVersion != pool.ClaimVersion+1 {
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
		workerGroupID:             fixture.group.ID,
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
			workerGroupID: fixture.group.ID, expectedGroupClaimVersion: fixture.group.ClaimVersion,
			desired: func(db.WorkerGroup) (pgtype.UUID, error) {
				return first.ID, nil
			},
		},
		{
			workerGroupID: fixture.group.ID, expectedGroupClaimVersion: fixture.group.ClaimVersion,
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
		TargetState:              "disabled",
		WorkerPoolID:             pending.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: pending.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
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
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id, state
) VALUES ($1, 'lost-before-activation', $2, $3, 'registering')`,
		workerID, fixture.group.ID, pending.ID)
	if _, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "disabled",
		WorkerPoolID:             pending.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: pending.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disable pending pool with registering worker error = %v, want no rows", err)
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `
UPDATE worker_instances
   SET state = 'lost', lost_at = now()
 WHERE id = $1`, workerID)

	disabled, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "disabled",
		WorkerPoolID:             pending.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: pending.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool with lost worker = %+v", disabled)
	}
}

func TestAdminWorkerPoolPostgresCheckpointReadySerializesWithPoolDrain(t *testing.T) {
	product := newActorStartPostgresFixture(t, 1)
	fixture := newAdminPoolPostgresFixture(t, product.pool, "us-east-1")
	target := fixture.addActivePool(t, "checkpoint-source")
	checkpoint := seedRestorableCheckpointForWorkerPool(t, product, fixture, target)
	serviceID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), product.pool, `
UPDATE worker_instances
   SET current_epoch = 1,
       current_service_id = $2,
       epoch_started_at = now()
 WHERE id = $1`, checkpoint.workerID, serviceID)
	dbtest.MustExec(t, t.Context(), product.pool, `
UPDATE runtime_instances
   SET desired_state = 'ready',
       desired_version = 1,
       desired_reason = 'placed',
       observed_state = 'allocated',
       observed_version = 0,
       observed_desired_version = 0,
       closing_at = NULL,
       closed_at = NULL,
       reclaimed_at = NULL,
       reclaim_evidence = NULL,
       terminal_at = NULL,
       terminal_reason_code = NULL
 WHERE id = $1`, checkpoint.runtimeID)
	dbtest.MustExec(t, t.Context(), product.pool, `
UPDATE run_leases
   SET state = 'checkpointing',
       checkpointed_at = NULL,
       terminal_at = NULL,
       terminal_reason_code = NULL
 WHERE id = $1`, checkpoint.runLeaseID)
	dbtest.MustExec(t, t.Context(), product.pool, `
UPDATE run_checkpoints
   SET state = 'creating',
       private_workspace_version_id = NULL,
       ready_request_fingerprint = NULL,
       ready_at = NULL
 WHERE id = $1`, checkpoint.checkpointID)

	if _, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "draining",
		WorkerPoolID:             target.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: target.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("drain with creating checkpoint and no replacement error = %v, want no rows", err)
	}

	checkpointTx, err := product.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkpointTx.Rollback(t.Context()) })
	checkpointQueries := db.New(checkpointTx)
	var lockedID string
	if err := checkpointTx.QueryRow(t.Context(), `
SELECT id::text
  FROM run_leases
 WHERE id = $1
 FOR UPDATE`, checkpoint.runLeaseID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointQueries.LockWorkerGroupForPoolMutation(t.Context(), fixture.group.ID); err != nil {
		t.Fatal(err)
	}
	if err := checkpointTx.QueryRow(t.Context(), `
SELECT id::text
  FROM worker_instances
 WHERE id = $1
   AND worker_group_id = $2
 FOR UPDATE`, checkpoint.workerID, fixture.group.ID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointQueries.LockWorkerPool(t.Context(), db.LockWorkerPoolParams{
		WorkerGroupID: fixture.group.ID,
		WorkerPoolID:  target.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointQueries.RequireCheckpointRestoreSupplier(t.Context(), db.RequireCheckpointRestoreSupplierParams{
		SourceWorkerPoolID: target.ID,
		SourceRunLeaseID:   pgvalue.UUID(checkpoint.runLeaseID),
		WorkerGroupID:      fixture.group.ID,
		WorkerInstanceID:   pgvalue.UUID(checkpoint.workerID),
		WorkerEpoch:        1,
	}); err != nil {
		t.Fatal(err)
	}

	drainTx, err := product.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drainTx.Rollback(t.Context()) })
	var drainPID int32
	if err := drainTx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&drainPID); err != nil {
		t.Fatal(err)
	}
	drainResult := make(chan error, 1)
	go func() {
		queries := db.New(drainTx)
		if _, err := queries.LockWorkerGroupForPoolMutation(t.Context(), fixture.group.ID); err != nil {
			drainResult <- err
			return
		}
		if _, err := queries.LockWorkerPool(t.Context(), db.LockWorkerPoolParams{
			WorkerGroupID: fixture.group.ID,
			WorkerPoolID:  target.ID,
		}); err != nil {
			drainResult <- err
			return
		}
		_, err := queries.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
			TargetState:              "draining",
			WorkerPoolID:             target.ID,
			WorkerGroupID:            fixture.group.ID,
			ExpectedPoolClaimVersion: target.ClaimVersion,
		})
		drainResult <- err
	}()
	waitForPostgresBlock(t, product.pool, drainPID)

	if _, err := checkpointTx.Exec(t.Context(), `
UPDATE run_checkpoints
   SET state = 'ready',
       private_workspace_version_id = $2,
       ready_request_fingerprint = 'checkpoint-ready',
       ready_at = now()
 WHERE id = $1
   AND state = 'creating'`, checkpoint.checkpointID, checkpoint.baseVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointTx.Exec(t.Context(), `
UPDATE run_leases
   SET state = 'checkpointed',
       checkpointed_at = now(),
       terminal_at = now(),
       terminal_reason_code = 'checkpointed'
 WHERE id = $1
   AND state = 'checkpointing'`, checkpoint.runLeaseID); err != nil {
		t.Fatal(err)
	}
	if err := checkpointTx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-drainResult; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("serialized drain error = %v, want no rows", err)
	}
	if err := drainTx.Rollback(t.Context()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}

	var checkpointState, leaseState, poolState string
	if err := product.pool.QueryRow(t.Context(), `
SELECT run_checkpoints.state, run_leases.state, worker_pools.state
  FROM run_checkpoints
  JOIN run_leases ON run_leases.id = run_checkpoints.source_run_lease_id
  JOIN worker_pools ON worker_pools.id = $2
 WHERE run_checkpoints.id = $1`, checkpoint.checkpointID, target.ID).Scan(
		&checkpointState, &leaseState, &poolState,
	); err != nil {
		t.Fatal(err)
	}
	if checkpointState != "ready" || leaseState != "checkpointed" || poolState != "active" {
		t.Fatalf("serialized lifecycle = checkpoint:%s lease:%s pool:%s", checkpointState, leaseState, poolState)
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

func TestAdminWorkerPoolPostgresRestorableCheckpointRequiresAnotherCompatibleSupplier(t *testing.T) {
	product := newActorStartPostgresFixture(t, 1)
	fixture := newAdminPoolPostgresFixture(t, product.pool, "us-east-1")
	target := fixture.addActivePool(t, "current")
	_ = seedRestorableCheckpointForWorkerPool(t, product, fixture, target)

	_, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "draining",
		WorkerPoolID:             target.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: target.ClaimVersion,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("drain sole compatible supplier error = %v, want no rows", err)
	}

	fixture.addActivePool(t, "replacement")
	live := seedLiveRuntimeForWorkerPool(t, product, fixture, target)
	bins, err := fixture.q.ListWorkerCapacityBins(t.Context(), db.ListWorkerCapacityBinsParams{
		WorkerGroupID:               fixture.group.ID,
		RegionID:                    "us-east-1",
		ObservationFreshnessSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 1 || bins[0].WorkerPoolID != target.ID {
		t.Fatalf("active Pool capacity bins = %+v", bins)
	}
	drained, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "draining",
		WorkerPoolID:             target.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: target.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if drained.State != "draining" || drained.ClaimVersion != target.ClaimVersion+1 {
		t.Fatalf("drained pool with replacement = %+v", drained)
	}
	bins, err = fixture.q.ListWorkerCapacityBins(t.Context(), db.ListWorkerCapacityBinsParams{
		WorkerGroupID:               fixture.group.ID,
		RegionID:                    "us-east-1",
		ObservationFreshnessSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 0 {
		t.Fatalf("draining Pool remained placement-eligible: %+v", bins)
	}

	_, err = fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "disabled",
		WorkerPoolID:             target.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: drained.ClaimVersion,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disable Pool with live runtime error = %v, want no rows", err)
	}
	finishLiveRuntimeForWorkerPool(t, product.pool, live)
	disabled, err := fixture.q.TransitionWorkerPoolLifecycle(t.Context(), db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "disabled",
		WorkerPoolID:             target.ID,
		WorkerGroupID:            fixture.group.ID,
		ExpectedPoolClaimVersion: drained.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != "disabled" || disabled.ClaimVersion != drained.ClaimVersion+1 {
		t.Fatalf("disabled drained pool = %+v", disabled)
	}
}

type adminPoolPostgresFixture struct {
	pool              *pgxpool.Pool
	q                 *db.Queries
	group             db.WorkerGroup
	runtimeIdentityID string
	cpuConfigDigest   string
	substrateFormat   string
	substrateContract string
}

func newAdminPoolPostgresFixture(t *testing.T, pool *pgxpool.Pool, regionID string) adminPoolPostgresFixture {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `
INSERT INTO regions (id, display_name)
VALUES ($1, 'Worker Pool Test')
ON CONFLICT (id) DO NOTHING`, regionID)
	q := db.New(pool)
	group, err := q.CreateWorkerGroup(t.Context(), db.CreateWorkerGroupParams{
		ID:          uuid.Must(uuid.NewV7()).String(),
		RegionID:    regionID,
		Name:        "worker-pool-test",
		Description: "",
		TokenID:     pgvalue.NewUUIDv7(),
		TokenHash:   dbtest.Hash(uuid.NewString()),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := adminPoolPostgresFixture{
		pool: pool, q: q, group: group,
		runtimeIdentityID: dbtest.Digest("worker-pool-runtime"),
		cpuConfigDigest:   dbtest.Digest("worker-pool-cpu"),
		substrateFormat:   capacity.SubstrateFormatExt4,
		substrateContract: capacity.SubstrateContractExt4,
	}
	_, err = q.UpsertRuntimeIdentity(t.Context(), db.UpsertRuntimeIdentityParams{
		ID:                        fixture.runtimeIdentityID,
		RuntimeArch:               "x86_64",
		VMRuntimeContract:         capacity.RuntimeContract,
		VMRuntimeDescriptorDigest: dbtest.Digest("worker-pool-runtime-descriptor"),
		FirecrackerDigest:         dbtest.Digest("worker-pool-firecracker"),
		FirecrackerVersion:        "1.12.0",
		SnapshotFormatVersion:     "1.0.0",
		HostKernelRelease:         "6.12.0",
		CPUTemplateKind:           "none",
		KernelDigest:              dbtest.Digest("worker-pool-kernel"),
		InitramfsDigest:           dbtest.Digest("worker-pool-initramfs"),
		RootfsDigest:              dbtest.Digest("worker-pool-rootfs"),
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
		RuntimeIdentityID:               pgvalue.Text(fixture.runtimeIdentityID),
		SubstrateFormat:                 pgvalue.Text(fixture.substrateFormat),
		SubstrateContract:               pgvalue.Text(fixture.substrateContract),
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
	var substrateID, sandboxDefinitionID uuid.UUID
	if err := product.pool.QueryRow(t.Context(), `
SELECT id, deployment_definition_id
  FROM runtime_substrates
 WHERE environment_id = $1
   AND substrate_format = $2
   AND substrate_contract = $3`,
		product.environmentID, fixture.substrateFormat, fixture.substrateContract,
	).Scan(&substrateID, &sandboxDefinitionID); err != nil {
		t.Fatal(err)
	}
	live := adminPoolLiveRuntime{
		workerID:  uuid.Must(uuid.NewV7()),
		runtimeID: uuid.Must(uuid.NewV7()),
	}
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id, state,
    current_epoch, current_service_id, runtime_identity_id, substrate_format, substrate_contract,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_runtime_starts,
    cpu_environment, cpu_environment_digest, observed_at,
    epoch_started_at, activated_at
) VALUES (
    $1, 'live-runtime-worker', $2, $3, 'active',
    1, $4, $5, $6, $7,
    4000, 8589934592, 34359738368,
    1000, 1073741824, 4294967296,
    4, 4, '{"vendor":"test"}'::jsonb, $8, now(), now(), now()
)`, live.workerID, fixture.group.ID, pool.ID, uuid.Must(uuid.NewV7()),
		fixture.runtimeIdentityID, fixture.substrateFormat, fixture.substrateContract,
		dbtest.Digest("live-runtime-cpu-environment"))
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO runtime_instances (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, runtime_identity_id, deployment_definition_id,
    runtime_substrate_id, worker_epoch, vm_vcpu_count, cpu_config_digest,
    reserved_cpu_millis, reserved_memory_bytes,
    reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
    workspace_id, desired_reason
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', $6, $7, $8, $9,
    1, 1, $10, 1000, 1073741824, 4294967296, 1, $11, 'placed'
)`, live.runtimeID, product.orgID, fixture.group.ID, product.projectID,
		product.environmentID, live.workerID, fixture.runtimeIdentityID,
		sandboxDefinitionID, substrateID, fixture.cpuConfigDigest, product.workspaceIDs[0])
	return live
}

func finishLiveRuntimeForWorkerPool(t *testing.T, pool *pgxpool.Pool, live adminPoolLiveRuntime) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `
UPDATE runtime_instances
   SET desired_state = 'closed',
       desired_version = 2,
       desired_reason = 'drained',
       observed_state = 'closed',
       observed_version = 1,
       observed_desired_version = 2,
       observed_at = now(),
       closing_at = now(),
       closed_at = now(),
       terminal_at = now(),
       terminal_reason_code = 'drained',
       reclaimed_at = now(),
       reclaim_evidence = '{"method":"drained"}'::jsonb,
       updated_at = now()
 WHERE id = $1`, live.runtimeID)
	dbtest.MustExec(t, t.Context(), pool, `
UPDATE worker_instances
   SET state = 'termination_ready',
       claim_version = claim_version + 1,
       draining_at = now(),
       termination_ready_at = now(),
       updated_at = now()
 WHERE id = $1`, live.workerID)
}

type adminPoolCheckpoint struct {
	workerID      uuid.UUID
	runtimeID     uuid.UUID
	runLeaseID    uuid.UUID
	checkpointID  uuid.UUID
	baseVersionID uuid.UUID
}

func seedRestorableCheckpointForWorkerPool(
	t *testing.T,
	product actorStartPostgresFixture,
	fixture adminPoolPostgresFixture,
	pool db.WorkerPool,
) adminPoolCheckpoint {
	t.Helper()
	var taskDefinitionID, sandboxDefinitionID, baseVersionID uuid.UUID
	if err := product.pool.QueryRow(t.Context(), `
SELECT id
  FROM deployment_definitions
 WHERE environment_id = $1
   AND kind = 'task'
   AND declared_id = 'resize-image'`, product.environmentID).Scan(&taskDefinitionID); err != nil {
		t.Fatal(err)
	}
	if err := product.pool.QueryRow(t.Context(), `
SELECT deployment_definition_id, head_version_id
  FROM workspaces
 WHERE id = $1`, product.workspaceIDs[0]).Scan(&sandboxDefinitionID, &baseVersionID); err != nil {
		t.Fatal(err)
	}

	workerID := uuid.Must(uuid.NewV7())
	runtimeSubstrateID := uuid.Must(uuid.NewV7())
	runtimeID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runLeaseID := uuid.Must(uuid.NewV7())
	mountID := uuid.Must(uuid.NewV7())
	workspaceLeaseID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())

	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id, state, lost_at
) VALUES ($1, $2, $3, $4, 'lost', now())`,
		workerID, "retained-checkpoint-worker", fixture.group.ID, pool.ID)
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO runtime_substrates (
    id, org_id, project_id, environment_id, deployment_definition_id,
    substrate_digest, substrate_format, substrate_contract, substrate_size_bytes
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)`,
		runtimeSubstrateID, product.orgID, product.projectID, product.environmentID,
		sandboxDefinitionID, dbtest.Digest("retained-checkpoint-substrate"),
		fixture.substrateFormat, fixture.substrateContract)
	tx, err := product.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, workspace_id, base_workspace_version_id, payload,
    queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
    retry_policy, root_span_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'resize-image', 'api', $7, $8,
    '{}'::jsonb, 'default', now(), now(), 300000,
    '{"enabled":false}'::jsonb, '1111111111111111'
	)`, runID, product.orgID, product.projectID, product.environmentID,
		product.deploymentID, taskDefinitionID, product.workspaceIDs[0], baseVersionID)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`, runID, product.workspaceIDs[0], baseVersionID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO runtime_instances (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, runtime_identity_id, deployment_definition_id,
    runtime_substrate_id, worker_epoch, vm_vcpu_count, cpu_config_digest,
    reserved_cpu_millis, reserved_memory_bytes,
    reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
    workspace_id, desired_state, desired_version, desired_reason,
    observed_state, observed_version, observed_desired_version,
    closing_at, closed_at, reclaimed_at, reclaim_evidence,
    terminal_at, terminal_reason_code
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', $6, $7, $8, $9,
    1, 1, $10, 1000, 1073741824, 4294967296, 1, $11,
    'closed', 2, 'checkpointed', 'closed', 2, 2,
    now(), now(), now(), '{"method":"checkpointed"}'::jsonb,
    now(), 'checkpointed'
)`, runtimeID, product.orgID, fixture.group.ID, product.projectID,
		product.environmentID, workerID, fixture.runtimeIdentityID, sandboxDefinitionID,
		runtimeSubstrateID, fixture.cpuConfigDigest, product.workspaceIDs[0])
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id,
    region_id, lease_sequence, attempt_number, worker_group_id,
    worker_instance_id, worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes, requested_execution_slots,
    state, assigned_at, start_deadline_at, claimed_at, started_at,
    expires_at, checkpointed_at, terminal_at, terminal_reason_code
) VALUES (
    $1, $2, $3, $4, $5, $6, 'us-east-1', 1, 1, $7, $8, 1, $9, $10,
    1000, 1073741824, 4294967296, 1, 'checkpointed',
    now() - interval '1 minute', now() + interval '1 minute',
    now() - interval '1 minute', now() - interval '1 minute',
    now() + interval '2 minutes', now(), now(), 'checkpointed'
)`, runLeaseID, product.orgID, product.projectID, product.environmentID,
		runID, product.workspaceIDs[0], fixture.group.ID, workerID, runtimeID,
		fixture.runtimeIdentityID)
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO workspace_mounts (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
    runtime_instance_id, state, unmounted_at, terminal_at, terminal_reason_code
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', $6, 1, $7, $8, $9,
    'unmounted', now(), now(), 'checkpointed'
)`, mountID, product.orgID, fixture.group.ID, product.projectID,
		product.environmentID, workerID, product.workspaceIDs[0], baseVersionID, runtimeID)
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, state, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_token_hash, expires_at, released_at, terminal_at
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', $6, 1, $7, $8, $9,
    'released', $10, $11, 1, 1, 1, 'checkpointed',
    now() + interval '2 minutes', now(), now()
)`, workspaceLeaseID, product.orgID, fixture.group.ID, product.projectID,
		product.environmentID, workerID, runtimeID, product.workspaceIDs[0], mountID,
		runLeaseID, baseVersionID)
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at,
    condition_state, condition_result, condition_terminal_at,
    suspension_state, expected_run_state_version, attempt_number,
    resume_attach_id, suspension_terminal_at
) VALUES (
    $1, $2, $3, $4, 'timer', now(), 'completed', '{}'::jsonb, now(),
    'released', 1, 1, $5, now()
)`, waitID, product.environmentID, runID, product.workspaceIDs[0], uuid.Must(uuid.NewV7()))
	dbtest.MustExec(t, t.Context(), product.pool, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id,
    source_run_lease_id, source_workspace_lease_id, workspace_id,
    base_workspace_version_id, private_workspace_version_id,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, 1, $3, $4, $5, $6, $7, $7,
    'ready', '{"kind":"suspend"}'::jsonb, 'checkpoint-ready', now()
)`, checkpointID, runID, waitID, runLeaseID, workspaceLeaseID,
		product.workspaceIDs[0], baseVersionID)
	return adminPoolCheckpoint{
		workerID: workerID, runtimeID: runtimeID,
		runLeaseID: runLeaseID, checkpointID: checkpointID,
		baseVersionID: baseVersionID,
	}
}
