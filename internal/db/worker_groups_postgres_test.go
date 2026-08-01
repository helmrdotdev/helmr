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
	"github.com/jackc/pgx/v5"
)

func TestWorkerGroupObservationTTLIsPositiveClaimAuthority(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	groupID := "ttl-" + shortUUID(uuid.Must(uuid.NewV7()))
	params := db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		ObservationTtlSeconds: 120,
		AllowsBuild:           true, ProtocolVersion: auth.WorkerProtocolVersion,
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredBuildExecutors: 1,
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
	groupID := "drift-" + shortUUID(uuid.Must(uuid.NewV7()))
	params := db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		ObservationTtlSeconds: 120,
		AllowsBuild:           true, ProtocolVersion: auth.WorkerProtocolVersion,
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredBuildExecutors: 1,
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

func TestDeploymentWorkerGroupLifecycleIsFencedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	initial, err := q.GetWorkerGroupLifecycle(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDraining),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != db.WorkerGroupStateDraining || stopped.ClaimVersion != initial.ClaimVersion+1 || !stopped.TransitionApplied {
		t.Fatalf("stopped = %+v", stopped)
	}
	replayed, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDraining),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != stopped.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed stop = %+v", replayed)
	}
	if _, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateActive),
		ExpectedClaimVersion: initial.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale reactivation error = %v", err)
	}
	active, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateActive),
		ExpectedClaimVersion: stopped.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.State != db.WorkerGroupStateActive || active.ClaimVersion != stopped.ClaimVersion+1 || !active.TransitionApplied {
		t.Fatalf("reactivated = %+v", active)
	}
	if _, err := q.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDraining),
		ExpectedClaimVersion: initial.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delayed stop error = %v", err)
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
	`, credentialID, dbtest.DefaultWorkerGroupID, workerID, uuid.NewString(), initial.ClaimVersion, []byte("drift-secret")); err != nil {
		t.Fatal(err)
	}
	lost, err := q.LoseWorkerInstanceForDrift(ctx, db.LoseWorkerInstanceForDriftParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lost.ID != pgvalue.UUID(workerID) || lost.State != db.WorkerInstanceStateLost || lost.ClaimVersion != initial.ClaimVersion+1 || !lost.TransitionApplied {
		t.Fatalf("lost = %+v", lost)
	}
	replayed, err := q.LoseWorkerInstanceForDrift(ctx, db.LoseWorkerInstanceForDriftParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != lost.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed loss = %+v", replayed)
	}
	if _, err := q.LoseWorkerInstanceForDrift(ctx, db.LoseWorkerInstanceForDriftParams{
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
		t.Fatal("drift loss did not revoke the Worker credential")
	}
}

func TestDeploymentWorkerInstanceLossTerminallyFencesRegisteringIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.Must(uuid.NewV7())
	resourceID := "registering-drift-" + workerID.String()
	secretHash := []byte("registering-drift-secret")
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
	lost, err := q.LoseWorkerInstanceForDrift(ctx, db.LoseWorkerInstanceForDriftParams{
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
	if _, err := q.EnrollWorkerInstance(ctx, enrollmentParams(
		nonce, uuid.Must(uuid.NewV7()), resourceID, true, false, []byte("replacement-secret"),
	)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost registering identity re-enrollment error = %v, want pgx.ErrNoRows", err)
	}
	proof, err := q.GetFleetTerminationProof(ctx, db.GetFleetTerminationProofParams{
		WorkerInstanceID: credential.WorkerInstanceID, WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proof.FencedForTermination || proof.AuthorityCount != 0 || proof.CurrentEpoch.Valid {
		t.Fatalf("termination proof = %+v, want pre-epoch terminal fence", proof)
	}
	claimed, err := q.ClaimFleetWorkerTermination(ctx, db.ClaimFleetWorkerTerminationParams{
		WorkerInstanceID: credential.WorkerInstanceID, WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.FencedForTermination || claimed.CurrentEpoch.Valid || claimed.ResourceID != resourceID {
		t.Fatalf("termination claim = %+v, want pre-epoch terminal fence", claimed)
	}
}
