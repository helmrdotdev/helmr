package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunWorkerCapacitySelectionFindsViableWorkerPastPlannerLimit(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	seedCapacityQueryWorkers(t, ctx, pool, 1002, 1001)
	q := db.New(pool)
	bins, err := q.ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
		RowLimit: 1001,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 1001 {
		t.Fatalf("planner Worker prefix has %d rows, want 1001", len(bins))
	}
	if bins[len(bins)-1].WorkerInstanceID != capacityQueryWorkerID(1001) {
		t.Fatalf("planner Worker prefix ends at %s", pgvalue.UUIDString(bins[len(bins)-1].WorkerInstanceID))
	}

	selected, err := q.SelectRunWorkerCapacity(ctx, runCapacitySelectionParams())
	if err != nil {
		t.Fatal(err)
	}
	want := capacityQueryWorkerID(1002)
	if selected.WorkerInstanceID != want {
		t.Fatalf("selected Worker = %s, want %s", pgvalue.UUIDString(selected.WorkerInstanceID), pgvalue.UUIDString(want))
	}
}

func TestRunWorkerCapacityPressureCandidatesPageByWorkerID(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	seedCapacityQueryWorkers(t, ctx, pool, 129, 0)
	q := db.New(pool)
	params := runCapacityPressureParams()
	params.RowLimit = 128

	first, err := q.ListRunWorkerCapacityPressureCandidates(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 128 {
		t.Fatalf("first pressure page has %d rows, want 128", len(first))
	}
	if first[0].WorkerInstanceID != capacityQueryWorkerID(1) || first[127].WorkerInstanceID != capacityQueryWorkerID(128) {
		t.Fatalf("first pressure page bounds = %s..%s", pgvalue.UUIDString(first[0].WorkerInstanceID),
			pgvalue.UUIDString(first[len(first)-1].WorkerInstanceID))
	}
	params.AfterWorkerInstanceID = first[len(first)-1].WorkerInstanceID
	second, err := q.ListRunWorkerCapacityPressureCandidates(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].WorkerInstanceID != capacityQueryWorkerID(129) {
		t.Fatalf("second pressure page = %+v, want Worker 129", second)
	}
}

func TestRunWorkerCapacityRestoreCompatibilityMatchesPlanner(t *testing.T) {
	for _, test := range []struct {
		name          string
		cpuDigest     string
		workerFormat  string
		requestFormat string
	}{
		{name: "compatible", cpuDigest: dbtest.DefaultCPUConfigID, workerFormat: capacity.SubstrateFormatExt4, requestFormat: capacity.SubstrateFormatExt4},
		{name: "cpu shape mismatch", cpuDigest: dbtest.Digest("wrong-cpu"), workerFormat: capacity.SubstrateFormatExt4, requestFormat: capacity.SubstrateFormatExt4},
		{name: "substrate mismatch", cpuDigest: dbtest.DefaultCPUConfigID, workerFormat: capacity.SubstrateFormatExt4, requestFormat: "squashfs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newPostgresDB(t, ctx)
			seedCapacityQueryWorkers(t, ctx, pool, 1, 0)
			plannerCompatible := capacity.CanRestore(capacity.RestoreRequirements{
				WorkerGroupID: dbtest.DefaultWorkerGroupID, RuntimeIdentityID: dbtest.DefaultRuntimeID,
				VCPUCount: 1, CPUConfigDigest: test.cpuDigest,
				SubstrateFormat: test.requestFormat, SubstrateContract: capacity.SubstrateContractExt4,
				Resources: capacity.ResourceVector{CPUMillis: 1000, MemoryBytes: 1 << 30, GuestEphemeralDiskBytes: 32 << 30, VMSlots: 1},
			}, capacity.Pool{
				WorkerGroupID: dbtest.DefaultWorkerGroupID, RuntimeIdentityID: dbtest.DefaultRuntimeID,
				SubstrateFormat: test.workerFormat, SubstrateContract: capacity.SubstrateContractExt4,
				PerVM:     capacity.ResourceVector{CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 32 << 30, VMSlots: 1},
				CPUShapes: []runtimeid.CPUShape{{VCPUCount: 1, CPUConfigDigest: dbtest.DefaultCPUConfigID}},
			})
			immediateParams := runCapacitySelectionParams()
			immediateParams.RequiredRuntimeIdentityID = dbtest.DefaultRuntimeID
			immediateParams.RequiredWorkerGroupID = dbtest.DefaultWorkerGroupID
			immediateParams.RequiredVMVCPUCount = 1
			immediateParams.RequiredCPUConfigDigest = test.cpuDigest
			immediateParams.RequiredSubstrateFormat = test.requestFormat
			immediateParams.RequiredSubstrateContract = capacity.SubstrateContractExt4
			_, immediateErr := db.New(pool).SelectRunWorkerCapacity(ctx, immediateParams)

			pressureParams := runCapacityPressureParams()
			pressureParams.RequiredRuntimeIdentityID = immediateParams.RequiredRuntimeIdentityID
			pressureParams.RequiredWorkerGroupID = immediateParams.RequiredWorkerGroupID
			pressureParams.RequiredVMVCPUCount = immediateParams.RequiredVMVCPUCount
			pressureParams.RequiredCPUConfigDigest = immediateParams.RequiredCPUConfigDigest
			pressureParams.RequiredSubstrateFormat = immediateParams.RequiredSubstrateFormat
			pressureParams.RequiredSubstrateContract = immediateParams.RequiredSubstrateContract
			pressure, err := db.New(pool).ListRunWorkerCapacityPressureCandidates(ctx, pressureParams)
			if err != nil {
				t.Fatal(err)
			}
			if (immediateErr == nil) != plannerCompatible || (len(pressure) == 1) != plannerCompatible {
				t.Fatalf("planner = %v, immediate error = %v, pressure rows = %d", plannerCompatible, immediateErr, len(pressure))
			}
			if immediateErr != nil && !errors.Is(immediateErr, pgx.ErrNoRows) {
				t.Fatal(immediateErr)
			}
		})
	}
}

func runCapacitySelectionParams() db.SelectRunWorkerCapacityParams {
	return db.SelectRunWorkerCapacityParams{
		RegionID: dbtest.DefaultRegionID, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
		RunArchitecture: "x86_64", VMRuntimeContract: runtimeid.Contract,
		RequiredCPUMillis: 1000, RequiredMemoryBytes: 1 << 30,
		RequiredGuestEphemeralDiskBytes: 32 << 30,
	}
}

func runCapacityPressureParams() db.ListRunWorkerCapacityPressureCandidatesParams {
	base := runCapacitySelectionParams()
	return db.ListRunWorkerCapacityPressureCandidatesParams{
		RegionID: base.RegionID, ObservationFreshnessSeconds: base.ObservationFreshnessSeconds,
		RunArchitecture: base.RunArchitecture, VMRuntimeContract: base.VMRuntimeContract,
		RequiredCPUMillis: base.RequiredCPUMillis, RequiredMemoryBytes: base.RequiredMemoryBytes,
		RequiredGuestEphemeralDiskBytes: base.RequiredGuestEphemeralDiskBytes, RowLimit: 128,
	}
}

func seedCapacityQueryWorkers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, count, paused int) {
	t.Helper()
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id, state,
    current_epoch, current_service_id, runtime_identity_id,
    substrate_format, substrate_contract,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_runtime_starts, cpu_environment, cpu_environment_digest,
    run_paused_reason, observed_at, epoch_started_at, activated_at
)
SELECT ('00000000-0000-7000-8000-' || lpad(value::text, 12, '0'))::uuid,
       'capacity-worker-' || value,
       $1, $2, 'active', 1,
       ('10000000-0000-7000-8000-' || lpad(value::text, 12, '0'))::uuid,
       $3, $4, $5,
       8000, 17179869184, 274877906944,
       4000, 8589934592, 34359738368,
       8, 8, '{}'::jsonb, $6,
       CASE WHEN value <= $7 THEN 'test-incompatible' ELSE NULL END,
       now(), now(), now()
  FROM generate_series(1, $8::integer) AS value`,
		dbtest.DefaultWorkerGroupID, dbtest.DefaultWorkerPoolID, dbtest.DefaultRuntimeID,
		capacity.SubstrateFormatExt4, capacity.SubstrateContractExt4,
		dbtest.DefaultCPUConfigID, paused, count,
	)
}

func capacityQueryWorkerID(value int) pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8000-%012d", value)))
}

func TestWorkerEpochOwnsLivenessAndActivationReplayPreservesIt(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	serviceID := uuid.NewV7()
	secretHash := []byte("epoch-liveness-secret")
	enrollTestWorker(t, ctx, q, workerID, "epoch-liveness-worker", secretHash)

	authenticate := func(service uuid.UUID) db.AuthenticateWorkerInstanceCredentialRow {
		t.Helper()
		row, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
			WorkerInstanceID: pgvalue.UUID(workerID), SecretHash: secretHash,
			ServiceID: pgvalue.UUID(service),
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	firstEpoch := authenticate(serviceID)
	activationParams := testWorkerActivationParams(workerID, firstEpoch.CurrentEpoch)
	activated, err := q.ActivateWorkerInstance(ctx, activationParams)
	if err != nil {
		t.Fatal(err)
	}
	if activated.ObservedAt.Valid || activated.RunPausedReason.Valid || activated.RuntimePausedReason.Valid {
		t.Fatalf("initial activation liveness = observed:%+v run:%+v runtime:%+v", activated.ObservedAt, activated.RunPausedReason, activated.RuntimePausedReason)
	}
	bins, err := q.ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
		RowLimit:                    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, bin := range bins {
		if bin.WorkerInstanceID == pgvalue.UUID(workerID) {
			t.Fatalf("unobserved active worker appeared in capacity bins: %+v", bin)
		}
	}
	authorization := db.AuthorizeWorkerActivationCredentialParams{
		CredentialID: firstEpoch.ID, ClaimVersion: firstEpoch.ClaimVersion,
		GroupClaimVersion: firstEpoch.GroupClaimVersion, WorkerEpoch: firstEpoch.CurrentEpoch,
	}
	if authorized, err := q.AuthorizeWorkerActivationCredential(ctx, authorization); err != nil {
		t.Fatal(err)
	} else if authorized.WorkerState != db.WorkerInstanceStateActive {
		t.Fatalf("activation replay authorization state = %q, want active", authorized.WorkerState)
	}
	staleAuthorization := authorization
	staleAuthorization.WorkerEpoch.Int64++
	if _, err := q.AuthorizeWorkerActivationCredential(ctx, staleAuthorization); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale activation authorization error = %v, want pgx.ErrNoRows", err)
	}
	changed := activationParams
	changed.MaxVMSlots++
	if _, err := q.ActivateWorkerInstance(ctx, changed); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("changed activation replay error = %v, want pgx.ErrNoRows", err)
	}

	sentinel := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_instances
		   SET observed_at = $2,
		       run_paused_reason = NULL,
		       runtime_paused_reason = 'startup_recovery_leak'
		 WHERE id = $1
	`, workerID, sentinel); err != nil {
		t.Fatal(err)
	}
	replayed, err := q.ActivateWorkerInstance(ctx, activationParams)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.ObservedAt.Valid || !replayed.ObservedAt.Time.Equal(sentinel) || replayed.RunPausedReason.Valid ||
		!replayed.RuntimePausedReason.Valid || replayed.RuntimePausedReason.String != "startup_recovery_leak" {
		t.Fatalf("activation replay changed liveness = observed:%+v run:%+v runtime:%+v", replayed.ObservedAt, replayed.RunPausedReason, replayed.RuntimePausedReason)
	}

	sameEpoch := authenticate(serviceID)
	if sameEpoch.CurrentEpoch != firstEpoch.CurrentEpoch {
		t.Fatalf("same service epoch = %+v, want %+v", sameEpoch.CurrentEpoch, firstEpoch.CurrentEpoch)
	}
	var preservedObservedAt pgtype.Timestamptz
	var preservedRuntimePause pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT observed_at, runtime_paused_reason FROM worker_instances WHERE id = $1`, workerID).Scan(&preservedObservedAt, &preservedRuntimePause); err != nil {
		t.Fatal(err)
	}
	if !preservedObservedAt.Valid || !preservedObservedAt.Time.Equal(sentinel) || preservedRuntimePause.String != "startup_recovery_leak" {
		t.Fatalf("same service changed liveness = observed:%+v runtime:%+v", preservedObservedAt, preservedRuntimePause)
	}

	nextEpoch := authenticate(uuid.NewV7())
	if !nextEpoch.CurrentEpoch.Valid || nextEpoch.CurrentEpoch.Int64 != firstEpoch.CurrentEpoch.Int64+1 || nextEpoch.State != db.WorkerInstanceStateRegistering {
		t.Fatalf("new service epoch = %+v", nextEpoch)
	}
	var observedAt pgtype.Timestamptz
	var runPause, runtimePause pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT observed_at, run_paused_reason, runtime_paused_reason FROM worker_instances WHERE id = $1`, workerID).Scan(&observedAt, &runPause, &runtimePause); err != nil {
		t.Fatal(err)
	}
	if observedAt.Valid || runPause.Valid || runtimePause.Valid {
		t.Fatalf("new epoch retained liveness = observed:%+v run:%+v runtime:%+v", observedAt, runPause, runtimePause)
	}
}

func TestDrainingWorkerActivationSurvivesRestartAndLostResponse(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	secretHash := []byte("draining-restart-secret")
	enrollTestWorker(t, ctx, q, workerID, "draining-restart-worker", secretHash)
	authenticate := func(serviceID uuid.UUID) db.AuthenticateWorkerInstanceCredentialRow {
		t.Helper()
		row, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
			WorkerInstanceID: pgvalue.UUID(workerID),
			SecretHash:       secretHash,
			ServiceID:        pgvalue.UUID(serviceID),
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	authorizeActivation := func(row db.AuthenticateWorkerInstanceCredentialRow) db.AuthorizeWorkerActivationCredentialRow {
		t.Helper()
		authorized, err := q.AuthorizeWorkerActivationCredential(ctx, db.AuthorizeWorkerActivationCredentialParams{
			CredentialID:      row.ID,
			ClaimVersion:      row.ClaimVersion,
			GroupClaimVersion: row.GroupClaimVersion,
			WorkerEpoch:       row.CurrentEpoch,
		})
		if err != nil {
			t.Fatal(err)
		}
		return authorized
	}

	firstServiceID := uuid.NewV7()
	firstEpoch := authenticate(firstServiceID)
	firstActivation := testWorkerActivationParams(workerID, firstEpoch.CurrentEpoch)
	active, err := q.ActivateWorkerInstance(ctx, firstActivation)
	if err != nil {
		t.Fatal(err)
	}
	draining, err := q.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        firstEpoch.CurrentEpoch,
		ExpectedClaimVersion: active.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.State != db.WorkerInstanceStateDraining || !draining.DrainingAt.Valid {
		t.Fatalf("draining worker = %+v", draining)
	}

	sameEpoch := authenticate(firstServiceID)
	if authorized := authorizeActivation(sameEpoch); authorized.WorkerState != db.WorkerInstanceStateDraining {
		t.Fatalf("draining activation replay authorization = %+v", authorized)
	}
	replayedDraining, err := q.ActivateWorkerInstance(ctx, firstActivation)
	if err != nil {
		t.Fatal(err)
	}
	if replayedDraining.State != db.WorkerInstanceStateDraining || replayedDraining.DrainingAt != draining.DrainingAt {
		t.Fatalf("draining activation replay = %+v, want draining at %+v", replayedDraining, draining.DrainingAt)
	}
	mismatched := firstActivation
	mismatched.MaxVMSlots++
	if _, err := q.ActivateWorkerInstance(ctx, mismatched); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched draining activation error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := q.LockWorkerInstanceForActivation(ctx, db.LockWorkerInstanceForActivationParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		WorkerPoolID:     pgvalue.NewUUIDv7(),
		WorkerEpoch:      firstEpoch.CurrentEpoch,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched draining pool fence error = %v, want pgx.ErrNoRows", err)
	}

	nextEpoch := authenticate(uuid.NewV7())
	if nextEpoch.State != db.WorkerInstanceStateDraining ||
		nextEpoch.CurrentEpoch.Int64 != firstEpoch.CurrentEpoch.Int64+1 {
		t.Fatalf("restarted draining epoch = %+v", nextEpoch)
	}
	cleared := authorizeActivation(nextEpoch)
	if cleared.WorkerState != db.WorkerInstanceStateDraining {
		t.Fatalf("restarted draining authorization = %+v", cleared)
	}
	staleAuthorization := db.AuthorizeWorkerActivationCredentialParams{
		CredentialID:      nextEpoch.ID,
		ClaimVersion:      nextEpoch.ClaimVersion,
		GroupClaimVersion: nextEpoch.GroupClaimVersion,
		WorkerEpoch:       firstEpoch.CurrentEpoch,
	}
	if _, err := q.AuthorizeWorkerActivationCredential(ctx, staleAuthorization); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale draining epoch authorization error = %v, want pgx.ErrNoRows", err)
	}

	restartedActivation := testWorkerActivationParams(workerID, nextEpoch.CurrentEpoch)
	restarted, err := q.ActivateWorkerInstance(ctx, restartedActivation)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.State != db.WorkerInstanceStateDraining || restarted.DrainingAt != draining.DrainingAt {
		t.Fatalf("restarted draining activation = %+v", restarted)
	}
	lostResponseReplay, err := q.ActivateWorkerInstance(ctx, restartedActivation)
	if err != nil {
		t.Fatal(err)
	}
	if lostResponseReplay.State != db.WorkerInstanceStateDraining ||
		lostResponseReplay.CurrentEpoch != nextEpoch.CurrentEpoch ||
		lostResponseReplay.DrainingAt != restarted.DrainingAt {
		t.Fatalf("lost activation response replay = %+v, want %+v", lostResponseReplay, restarted)
	}
}

func testWorkerActivationParams(workerID uuid.UUID, epoch pgtype.Int8) db.ActivateWorkerInstanceParams {
	return db.ActivateWorkerInstanceParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID, WorkerEpoch: epoch,
		EpochCPUMillis: 2000, EpochMemoryBytes: 2 << 30, EpochGuestEphemeralDiskBytes: 64 << 30,
		MaxVMSlots: 1, RuntimeIdentityID: pgtype.Text{String: dbtest.DefaultRuntimeID, Valid: true},
		SubstrateFormat: "ext4", SubstrateContract: "helmr.substrate.ext4.v0",
		PerVMCPUMillis: 1000, PerVMMemoryBytes: 1 << 30, PerVMGuestEphemeralDiskBytes: 32 << 30,
		MaxRuntimeStarts:     1,
		CPUEnvironment:       []byte(`{}`),
		CPUEnvironmentDigest: pgtype.Text{String: dbtest.DefaultCPUConfigID, Valid: true},
	}
}

func TestWorkerGroupStateTransitionsAreFencedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	initial, err := q.GetWorkerGroupState(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStatePaused),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != db.WorkerGroupStatePaused || paused.ClaimVersion != initial.ClaimVersion+1 || !paused.TransitionApplied {
		t.Fatalf("paused = %+v", paused)
	}
	replayed, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStatePaused),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != paused.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed pause = %+v", replayed)
	}
	active, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateActive),
		ExpectedClaimVersion: paused.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.State != db.WorkerGroupStateActive || active.ClaimVersion != paused.ClaimVersion+1 || !active.TransitionApplied {
		t.Fatalf("active = %+v", active)
	}
	draining, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDraining),
		ExpectedClaimVersion: active.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.State != db.WorkerGroupStateDraining || draining.ClaimVersion != active.ClaimVersion+1 || !draining.TransitionApplied {
		t.Fatalf("draining = %+v", draining)
	}
	if _, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateActive),
		ExpectedClaimVersion: initial.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale reactivation error = %v", err)
	}
	drainingPool, err := q.TransitionWorkerPoolLifecycle(ctx, db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "draining",
		WorkerPoolID:             pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)),
		WorkerGroupID:            dbtest.DefaultWorkerGroupID,
		ExpectedPoolClaimVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if drainingPool.State != "draining" || drainingPool.ClaimVersion != 2 {
		t.Fatalf("draining Pool = %+v", drainingPool)
	}
	disabledPool, err := q.TransitionWorkerPoolLifecycle(ctx, db.TransitionWorkerPoolLifecycleParams{
		TargetState:              "disabled",
		WorkerPoolID:             pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)),
		WorkerGroupID:            dbtest.DefaultWorkerGroupID,
		ExpectedPoolClaimVersion: drainingPool.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabledPool.State != "disabled" || disabledPool.ClaimVersion != drainingPool.ClaimVersion+1 {
		t.Fatalf("disabled Pool = %+v", disabledPool)
	}
	disabled, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDisabled),
		ExpectedClaimVersion: draining.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != db.WorkerGroupStateDisabled || disabled.ClaimVersion != draining.ClaimVersion+1 || !disabled.TransitionApplied {
		t.Fatalf("disabled = %+v", disabled)
	}
}

func TestDeploymentWorkerInstanceLossIsFencedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now())
	resourceID := "active-" + workerID.String()
	initial, err := q.GetWorkerInstanceStateByResource(ctx, db.GetWorkerInstanceStateByResourceParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentialID := uuid.NewV7()
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, credentialID, dbtest.DefaultWorkerGroupID, workerID, uuid.New().String(), initial.ClaimVersion, []byte("loss-secret")); err != nil {
		t.Fatal(err)
	}
	lost, err := q.MarkWorkerInstanceLost(ctx, db.MarkWorkerInstanceLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lost.ID != pgvalue.UUID(workerID) || lost.State != db.WorkerInstanceStateLost || lost.ClaimVersion != initial.ClaimVersion+1 || !lost.TransitionApplied {
		t.Fatalf("lost = %+v", lost)
	}
	replayed, err := q.MarkWorkerInstanceLost(ctx, db.MarkWorkerInstanceLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != lost.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed loss = %+v", replayed)
	}
	if _, err := q.MarkWorkerInstanceLost(ctx, db.MarkWorkerInstanceLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: lost.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new loss error = %v", err)
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM worker_instance_credentials WHERE id = $1`, credentialID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("worker loss did not revoke the Worker credential")
	}
}

func TestDeploymentWorkerInstanceLossTerminallyFencesRegisteringIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	resourceID := "registering-lost-" + workerID.String()
	secretHash := []byte("registering-lost-secret")
	credential := enrollTestWorker(t, ctx, q, workerID, resourceID, secretHash)
	initial, err := q.GetWorkerInstanceStateByResource(ctx, db.GetWorkerInstanceStateByResourceParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initial.State != db.WorkerInstanceStateRegistering || initial.CurrentEpoch.Valid {
		t.Fatalf("initial lifecycle = %+v, want pre-epoch registering", initial)
	}
	lost, err := q.MarkWorkerInstanceLost(ctx, db.MarkWorkerInstanceLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lost.State != db.WorkerInstanceStateLost || lost.CurrentEpoch.Valid || lost.ClaimVersion != initial.ClaimVersion+1 {
		t.Fatalf("lost lifecycle = %+v, want terminal pre-epoch fence", lost)
	}
	if _, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: credential.WorkerInstanceID, SecretHash: secretHash,
		ServiceID: pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost registering credential authentication error = %v, want pgx.ErrNoRows", err)
	}
	replacementID := uuid.NewV7()
	replacement, err := q.EnrollWorkerInstance(ctx, enrollmentParams(
		replacementID, resourceID, []byte("replacement-secret"),
	))
	if err != nil {
		t.Fatalf("enroll replacement identity: %v", err)
	}
	if replacement.WorkerInstanceID.Bytes != replacementID || replacement.WorkerInstanceID.Bytes == credential.WorkerInstanceID.Bytes {
		t.Fatalf("replacement Worker instance ID = %v, want new ID %s", replacement.WorkerInstanceID, replacementID)
	}
	var lostCount, registeringCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'lost'),
		       count(*) FILTER (WHERE state = 'registering')
		  FROM worker_instances
		 WHERE worker_group_id = $1 AND resource_id = $2
	`, dbtest.DefaultWorkerGroupID, resourceID).Scan(&lostCount, &registeringCount); err != nil {
		t.Fatal(err)
	}
	if lostCount != 1 || registeringCount != 1 {
		t.Fatalf("locator identities = lost %d registering %d, want 1 and 1", lostCount, registeringCount)
	}
}

func enrollTestWorker(t *testing.T, ctx context.Context, q *db.Queries, workerID uuid.UUID, resourceID string, secretHash []byte) db.EnrollWorkerInstanceRow {
	t.Helper()
	row, err := q.EnrollWorkerInstance(ctx, enrollmentParams(workerID, resourceID, secretHash))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func enrollmentParams(workerID uuid.UUID, resourceID string, secretHash []byte) db.EnrollWorkerInstanceParams {
	return db.EnrollWorkerInstanceParams{
		TokenHash:    make([]byte, 32),
		WorkerPoolID: pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)), PoolName: "default",
		WorkerInstanceID: pgvalue.UUID(workerID), ResourceID: resourceID,
		CurrentServiceID: pgvalue.NewUUIDv7(), CredentialID: pgvalue.NewUUIDv7(),
		KeyPrefix: uuid.New().String(), SecretHash: secretHash,
	}
}
