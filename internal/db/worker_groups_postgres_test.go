package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

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
		SupportsRun: true, WorkerInstanceID: credential.WorkerInstanceID, SecretHash: secretHash,
		ServiceID: pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost registering credential authentication error = %v, want pgx.ErrNoRows", err)
	}
	replacementID := uuid.Must(uuid.NewV7())
	replacement, err := q.EnrollWorkerInstance(ctx, enrollmentParams(
		replacementID, resourceID, true, false, []byte("replacement-secret"),
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

func enrollTestWorker(t *testing.T, ctx context.Context, q *db.Queries, workerID uuid.UUID, resourceID string, allowsRun bool, allowsBuild bool, secretHash []byte) db.EnrollWorkerInstanceRow {
	t.Helper()
	row, err := q.EnrollWorkerInstance(ctx, enrollmentParams(workerID, resourceID, allowsRun, allowsBuild, secretHash))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func enrollmentParams(workerID uuid.UUID, resourceID string, allowsRun bool, allowsBuild bool, secretHash []byte) db.EnrollWorkerInstanceParams {
	return db.EnrollWorkerInstanceParams{
		TokenHash: make([]byte, 32), AllowsRun: allowsRun, AllowsBuild: allowsBuild,
		WorkerInstanceID: pgvalue.UUID(workerID), ResourceID: resourceID,
		CurrentServiceID: pgvalue.NewUUIDv7(), CredentialID: pgvalue.NewUUIDv7(),
		KeyPrefix: uuid.NewString(), SecretHash: secretHash,
	}
}
