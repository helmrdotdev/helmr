package db_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
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
	hostSecretID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_host_secrets (
			id, worker_group_id, worker_host_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, 1, $5)
	`, hostSecretID, dbtest.DefaultWorkerGroupID, workerID, uuid.New().String(), []byte("drain-secret"))

	draining, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{DrainReason: "shutdown",
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.Status != db.WorkerHostStatusDraining || draining.ClaimVersion != 2 {
		t.Fatalf("draining row = %+v", draining)
	}

	params := db.CompleteWorkerDrainParams{
		WorkerHostID:         pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		WorkerEpoch:          pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: draining.ClaimVersion,
	}
	completed, err := q.CompleteWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != db.WorkerHostStatusTerminationReady || completed.ClaimVersion != 3 || !completed.TerminationReadyAt.Valid {
		t.Fatalf("terminal receipt = %+v", completed)
	}
	replayed, err := q.CompleteWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != completed.Status || replayed.ClaimVersion != completed.ClaimVersion || replayed.TerminationReadyAt != completed.TerminationReadyAt {
		t.Fatalf("replayed receipt = %+v, want %+v", replayed, completed)
	}
	params.ExpectedClaimVersion = completed.ClaimVersion
	if _, err := q.CompleteWorkerDrain(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new completion error = %v, want pgx.ErrNoRows", err)
	}

	var revoked bool
	if err := pool.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL
		  FROM worker_host_secrets
		 WHERE id = $1
	`, hostSecretID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("terminal receipt did not revoke the worker credential")
	}
}

func TestWorkerDrainCurrentClaimPreservesFences(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	params := db.DrainWorkerHostParams{DrainReason: "shutdown",
		ID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1,
	}
	first, err := q.DrainWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range []int64{first.ClaimVersion, 1, first.ClaimVersion} {
		params.ExpectedClaimVersion = claim
		got, err := q.DrainWorkerHost(ctx, params)
		if err != nil {
			t.Fatalf("drain claim %d: %v", claim, err)
		}
		if got.ClaimVersion != first.ClaimVersion || got.DrainingAt != first.DrainingAt {
			t.Fatalf("reentry changed transition: %+v", got)
		}
	}
	for _, name := range []string{"worker", "group", "epoch", "older claim", "future claim"} {
		t.Run(name, func(t *testing.T) {
			bad := params
			switch name {
			case "worker":
				bad.ID = pgvalue.UUID(uuid.NewV7())
			case "group":
				bad.WorkerGroupID = pgvalue.UUID(uuid.NewV7())
			case "epoch":
				bad.ExpectedEpoch.Int64++
			case "older claim":
				bad.ExpectedClaimVersion = 0
			case "future claim":
				bad.ExpectedClaimVersion++
			}
			if _, err := q.DrainWorkerHost(ctx, bad); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid fence error=%v, want no rows", err)
			}
		})
	}
	dbtest.MustExec(t, ctx, pool, `UPDATE worker_hosts
 SET status='termination_ready',claim_version=claim_version+1,termination_ready_at=now() WHERE id=$1`, workerID)
	for _, claim := range []int64{first.ClaimVersion, first.ClaimVersion + 1} {
		params.ExpectedClaimVersion = claim
		if _, err := q.DrainWorkerHost(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("terminal drain claim %d error=%v, want no rows", claim, err)
		}
	}
}

func TestWorkerDrainPreservesPhysicalStateAndClosesAdmission(t *testing.T) {
	f := agenttest.New(t)
	snapshot := func() []byte {
		var value []byte
		if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l) FROM computer_leases l WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	params := db.DrainWorkerHostParams{DrainReason: "shutdown", ID: pgvalue.UUID(f.Worker), WorkerGroupID: pgvalue.UUID(f.Group), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1}
	for range 2 {
		got, err := db.New(f.Pool).DrainWorkerHost(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != db.WorkerHostStatusDraining {
			t.Fatalf("host still admits work: %+v", got)
		}
		if !bytes.Equal(before, snapshot()) {
			t.Fatal("drain changed physical lease")
		}
	}
}

func TestWorkerFencePublishesExactLostReceiptAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	hostSecretID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_host_secrets (
			id, worker_group_id, worker_host_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, 1, $5)
	`, hostSecretID, dbtest.DefaultWorkerGroupID, workerID, uuid.New().String(), []byte("fence-secret"))

	params := db.FenceWorkerHostParams{
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
	}
	lost, err := q.FenceWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if lost.Status != db.WorkerHostStatusLost || lost.ClaimVersion != 2 || !lost.LostAt.Valid {
		t.Fatalf("lost receipt = %+v", lost)
	}
	replayed, err := q.FenceWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != lost.Status || replayed.ClaimVersion != lost.ClaimVersion || replayed.LostAt != lost.LostAt {
		t.Fatalf("replayed lost receipt = %+v, want %+v", replayed, lost)
	}
	params.ExpectedClaimVersion = lost.ClaimVersion
	if _, err := q.FenceWorkerHost(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new fence error = %v, want pgx.ErrNoRows", err)
	}

	if _, err := q.AuthorizeWorkerFenceReplay(ctx, db.AuthorizeWorkerFenceReplayParams{
		HostSecretID: pgvalue.UUID(hostSecretID), ClaimVersion: 1,
		WorkerEpoch: pgtype.Int8{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("authorize exact fence replay: %v", err)
	}
}

func TestWorkerDrainRequiresPriorEpochPhysicalAllocationFenced(t *testing.T) {
	f := agenttest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, f.Worker)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_host_secrets(id,worker_group_id,worker_host_id,key_prefix,claim_version,secret_hash) VALUES($1,$2,$3,$4,1,$5)`, uuid.NewV7(), f.Group, f.Worker, uuid.NewV7().String(), []byte("drain-test"))
	q := db.New(f.Pool)
	row, err := q.DrainWorkerHost(t.Context(), db.DrainWorkerHostParams{DrainReason: "shutdown", ID: pgvalue.UUID(f.Worker), WorkerGroupID: pgvalue.UUID(f.Group), ExpectedEpoch: pgtype.Int8{Int64: 2, Valid: true}, ExpectedClaimVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	params := db.CompleteWorkerDrainParams{WorkerHostID: row.ID, WorkerGroupID: row.WorkerGroupID, WorkerEpoch: row.CurrentEpoch, ExpectedClaimVersion: row.ClaimVersion}
	if _, err = q.CompleteWorkerDrain(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unfenced prior epoch allowed drain: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='host physical stop' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	if row, err := q.CompleteWorkerDrain(t.Context(), params); err != nil || row.Status != db.WorkerHostStatusTerminationReady {
		t.Fatalf("fenced drain=%+v %v", row, err)
	}
}
