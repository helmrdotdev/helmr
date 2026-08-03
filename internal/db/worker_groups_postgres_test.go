package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestWorkerGroupObservationTTLIsPositiveClaimAuthority(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	groupID := dbtest.DefaultWorkerGroupID
	params := db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		ObservationTtlSeconds: 120,
		AllowsRun:             true, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredVmSlots: 1, RequiredBuildExecutors: 1,
	}
	first, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != first.ClaimVersion {
		t.Fatalf("unchanged TTL advanced claim version: first=%d replay=%d", first.ClaimVersion, replayed.ClaimVersion)
	}
	params.ObservationTtlSeconds = 60
	changed, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ClaimVersion != first.ClaimVersion+1 {
		t.Fatalf("changed TTL claim version=%d, want %d", changed.ClaimVersion, first.ClaimVersion+1)
	}
	params.ObservationTtlSeconds = 0
	if _, err := q.ReconcileWorkerGroup(ctx, params); err == nil {
		t.Fatal("zero observation TTL unexpectedly reconciled")
	}
}

func TestWorkerGroupReconcileDoesNotReactivateDrainingGroup(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	groupID := dbtest.DefaultWorkerGroupID
	params := db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		ObservationTtlSeconds: 120,
		AllowsRun:             true, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredVmSlots: 1, RequiredBuildExecutors: 1,
	}
	if _, err := q.ReconcileWorkerGroup(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET state = 'draining', updated_at = now()
		 WHERE id = $1
	`, groupID); err != nil {
		t.Fatal(err)
	}
	reconciled, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.State != db.WorkerGroupStateDraining {
		t.Fatalf("reconciled state = %s, want draining", reconciled.State)
	}
}

func TestWorkerGroupHasOneActiveGroupPerRoleAndRegion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allowsRun   bool
		allowsBuild bool
		vmSlots     int32
		buildSlots  int32
	}{
		{name: "run", allowsRun: true, vmSlots: 1},
		{name: "build", allowsBuild: true, buildSlots: 1},
		{name: "dual-role", allowsRun: true, allowsBuild: true, vmSlots: 1, buildSlots: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newPostgresDB(t, ctx)
			_, err := db.New(pool).ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
				ID: "second-" + tc.name, RegionID: dbtest.DefaultRegionID, Name: "second-" + tc.name,
				ObservationTtlSeconds: 120, AllowsRun: tc.allowsRun, AllowsBuild: tc.allowsBuild,
				RequiredCpuMillis: 1, RequiredMemoryBytes: 1, RequiredGuestEphemeralDiskBytes: 1,
				RequiredVmSlots: tc.vmSlots, RequiredBuildExecutors: tc.buildSlots,
				ProtocolVersion: auth.WorkerProtocolVersion,
			})
			if err == nil {
				t.Fatalf("second active %s group unexpectedly reconciled", tc.name)
			}
		})
	}
}

func TestWorkerGroupReplacementAllowsDrainingPredecessor(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	workerID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (id, resource_id, worker_group_id, state)
		VALUES ($1, $2, $3, 'registering')
	`, workerID, "predecessor-"+workerID.String(), dbtest.DefaultWorkerGroupID)
	mustExec(t, ctx, pool, `UPDATE worker_groups SET state = 'draining' WHERE id = $1`, dbtest.DefaultWorkerGroupID)
	replacementID := "replacement-" + shortUUID(uuid.Must(uuid.NewV7()))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := workergroup.Reconcile(ctx, db.New(tx), dbtest.DefaultRegionID, []workergroup.Desired{{
		Spec: workergroup.Spec{ID: replacementID, Name: replacementID, AllowsRun: true, AllowsBuild: true},
		Capacity: workergroup.Capacity{
			MilliCPU: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1,
			VMSlots: 1, BuildExecutors: 1,
		},
		ObservationTTLSeconds: 120,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var predecessorState, replacementState string
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_groups WHERE id = $1`, dbtest.DefaultWorkerGroupID).Scan(&predecessorState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_groups WHERE id = $1`, replacementID).Scan(&replacementState); err != nil {
		t.Fatal(err)
	}
	if predecessorState != "draining" || replacementState != "active" {
		t.Fatalf("replacement states = predecessor:%s replacement:%s", predecessorState, replacementState)
	}
	if _, err := pool.Exec(ctx, `UPDATE worker_groups SET state = 'active' WHERE id = $1`, dbtest.DefaultWorkerGroupID); err == nil {
		t.Fatal("draining predecessor reactivated while replacement was active")
	}
}

func TestDeploymentWorkerGroupLifecycleIsFencedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	initial, err := q.GetWorkerGroupLifecycle(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStatePaused),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != db.WorkerGroupStatePaused || paused.ClaimVersion != initial.ClaimVersion+1 || !paused.TransitionApplied {
		t.Fatalf("paused = %+v", paused)
	}
	replayed, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStatePaused),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != paused.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed pause = %+v", replayed)
	}
	active, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateActive),
		ExpectedClaimVersion: paused.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.State != db.WorkerGroupStateActive || active.ClaimVersion != paused.ClaimVersion+1 || !active.TransitionApplied {
		t.Fatalf("active = %+v", active)
	}
	draining, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDraining),
		ExpectedClaimVersion: active.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.State != db.WorkerGroupStateDraining || draining.ClaimVersion != active.ClaimVersion+1 || !draining.TransitionApplied {
		t.Fatalf("draining = %+v", draining)
	}
	if _, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateActive),
		ExpectedClaimVersion: initial.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale reactivation error = %v", err)
	}
	disabled, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
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
	initial, err := q.GetWorkerInstanceLifecycle(ctx, db.GetWorkerInstanceLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentialID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, claim_version,
			allows_run, allows_build, secret_hash
		) VALUES ($1, $2, $3, $4, $5, false, true, $6)
	`, credentialID, dbtest.DefaultWorkerGroupID, workerID, uuid.NewString(), initial.ClaimVersion, []byte("loss-secret")); err != nil {
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
	workerID := uuid.Must(uuid.NewV7())
	resourceID := "registering-lost-" + workerID.String()
	secretHash := []byte("registering-lost-secret")
	credential := enrollTestWorker(t, ctx, q, workerID, resourceID, true, false, secretHash)
	initial, err := q.GetWorkerInstanceLifecycle(ctx, db.GetWorkerInstanceLifecycleParams{
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
		SupportsRun: true, WorkerInstanceID: credential.WorkerInstanceID, SecretHash: secretHash,
		ProtocolVersion: auth.WorkerProtocolVersion, ServiceID: pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost registering credential authentication error = %v, want pgx.ErrNoRows", err)
	}
	nonce := createTestEnrollmentNonce(t, ctx, q, dbtest.DefaultWorkerGroupID)
	replacementID := uuid.Must(uuid.NewV7())
	replacement, err := q.EnrollWorkerInstance(ctx, enrollmentParams(
		nonce, replacementID, resourceID, true, false, []byte("replacement-secret"),
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
