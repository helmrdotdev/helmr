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
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerDrainPublishesExactTerminalReceiptAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	credentialID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, 1, $5)
	`, credentialID, dbtest.DefaultWorkerGroupID, workerID, uuid.NewString(), []byte("drain-secret"))

	draining, err := q.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.State != db.WorkerInstanceStateDraining || draining.ClaimVersion != 2 {
		t.Fatalf("draining row = %+v", draining)
	}

	params := db.CompleteWorkerDrainParams{
		WorkerInstanceID:     pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		WorkerEpoch:          pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: draining.ClaimVersion,
		ObservedAt:           pgvalue.Timestamptz(time.Now().UTC()),
	}
	completed, err := q.CompleteWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != db.WorkerInstanceStateTerminationReady || completed.ClaimVersion != 3 || !completed.TerminationReadyAt.Valid {
		t.Fatalf("terminal receipt = %+v", completed)
	}
	replayed, err := q.CompleteWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != completed.State || replayed.ClaimVersion != completed.ClaimVersion || replayed.TerminationReadyAt != completed.TerminationReadyAt {
		t.Fatalf("replayed receipt = %+v, want %+v", replayed, completed)
	}
	params.ExpectedClaimVersion = completed.ClaimVersion
	if _, err := q.CompleteWorkerDrain(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new completion error = %v, want pgx.ErrNoRows", err)
	}

	var revoked bool
	if err := pool.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL
		  FROM worker_instance_credentials
		 WHERE id = $1
	`, credentialID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("terminal receipt did not revoke the worker credential")
	}
}

func TestWorkerDrainReplayPublishesOwnerlessCleanupUntilRuntimeClosed(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	fixture := seedRuntimeSubstrateAuthority(t, ctx, pool)

	var runtimeID, workspaceID, baseVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT runtime_instances.id, runtime_instances.workspace_id, workspaces.head_version_id
		  FROM runtime_instances
		  JOIN workspaces ON workspaces.id = runtime_instances.workspace_id
		 WHERE runtime_instances.worker_instance_id = $1
	`, fixture.workerID).Scan(&runtimeID, &workspaceID, &baseVersionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'draining', claim_version = 2, draining_at = now()
		 WHERE id = $1
	`, fixture.workerID)
	mountID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
			runtime_instance_id, state, dirty_generation, mounted_at, stopped_at
		)
		SELECT $1, runtime_instances.org_id, runtime_instances.worker_group_id,
		       runtime_instances.project_id, runtime_instances.environment_id,
		       runtime_instances.region_id, runtime_instances.worker_instance_id,
		       runtime_instances.worker_epoch, runtime_instances.workspace_id, $2,
		       runtime_instances.id, 'unmounting', 7, now(), now()
		  FROM runtime_instances
		 WHERE runtime_instances.id = $3
	`, mountID, baseVersionID, runtimeID)

	params := db.DrainWorkerInstanceParams{
		ID:                   pgvalue.UUID(fixture.workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
	}
	if _, err := q.DrainWorkerInstance(ctx, params); err != nil {
		t.Fatal(err)
	}
	var mountState, finalizationKind, finalizationReason string
	if err := pool.QueryRow(ctx, `
		SELECT state, finalization_kind, finalization_reason_code
		  FROM workspace_mounts
		 WHERE id = $1
	`, mountID).Scan(&mountState, &finalizationKind, &finalizationReason); err != nil {
		t.Fatal(err)
	}
	if mountState != "unmounting" || finalizationKind != "discard" || finalizationReason != "worker_draining" {
		t.Fatalf("mount cleanup = (%q, %q, %q)", mountState, finalizationKind, finalizationReason)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE workspace_mounts
		   SET state = 'unmounted', unmounted_at = now(), terminal_at = now(),
		       terminal_reason_code = 'worker_unmounted', updated_at = now()
		 WHERE id = $1
	`, mountID)
	if _, err := q.DrainWorkerInstance(ctx, params); err != nil {
		t.Fatal(err)
	}
	var desiredState, desiredReason string
	var desiredVersion int64
	if err := pool.QueryRow(ctx, `
		SELECT desired_state, desired_version, desired_reason
		  FROM runtime_instances
		 WHERE id = $1
	`, runtimeID).Scan(&desiredState, &desiredVersion, &desiredReason); err != nil {
		t.Fatal(err)
	}
	if desiredState != "closed" || desiredVersion != 2 || desiredReason != "worker_draining" {
		t.Fatalf("runtime cleanup = (%q, %d, %q)", desiredState, desiredVersion, desiredReason)
	}
	if _, err := q.DrainWorkerInstance(ctx, params); err != nil {
		t.Fatal(err)
	}
	var replayVersion int64
	if err := pool.QueryRow(ctx, `SELECT desired_version FROM runtime_instances WHERE id = $1`, runtimeID).Scan(&replayVersion); err != nil {
		t.Fatal(err)
	}
	if replayVersion != desiredVersion {
		t.Fatalf("runtime desired version after exact replay = %d, want %d", replayVersion, desiredVersion)
	}
}

func TestWorkerFencePublishesExactLostReceiptAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	credentialID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, 1, $5)
	`, credentialID, dbtest.DefaultWorkerGroupID, workerID, uuid.NewString(), []byte("fence-secret"))

	params := db.FenceWorkerInstanceParams{
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
		ReasonCode:           pgtype.Text{String: "termination_drain_failed", Valid: true},
	}
	lost, err := q.FenceWorkerInstance(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if lost.State != db.WorkerInstanceStateLost || lost.ClaimVersion != 2 || !lost.LostAt.Valid {
		t.Fatalf("lost receipt = %+v", lost)
	}
	replayed, err := q.FenceWorkerInstance(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != lost.State || replayed.ClaimVersion != lost.ClaimVersion || replayed.LostAt != lost.LostAt {
		t.Fatalf("replayed lost receipt = %+v, want %+v", replayed, lost)
	}
	params.ExpectedClaimVersion = lost.ClaimVersion
	if _, err := q.FenceWorkerInstance(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new fence error = %v, want pgx.ErrNoRows", err)
	}

	if _, err := q.AuthorizeWorkerFenceReplay(ctx, db.AuthorizeWorkerFenceReplayParams{
		CredentialID: pgvalue.UUID(credentialID), ClaimVersion: 1,
		WorkerEpoch: pgtype.Int8{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("authorize exact fence replay: %v", err)
	}
}
