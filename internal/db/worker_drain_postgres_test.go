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
			allows_run, allows_build, secret_hash
		) VALUES ($1, $2, $3, $4, 1, false, true, $5)
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

func TestWorkerFencePublishesExactLostReceiptAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	credentialID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, claim_version,
			allows_run, allows_build, secret_hash
		) VALUES ($1, $2, $3, $4, 1, false, true, $5)
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
