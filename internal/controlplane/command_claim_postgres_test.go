package controlplane

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommandClaimSharesInstanceAndReplays(t *testing.T) {
	f := runtest.New(t)
	member := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, member.RunID)
	worker := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
	var requests []commandClaim
	for range 3 {
		id, claim := uuid.NewV7(), uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,computer_instance_id,writer_generation,status)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text,computer_instance_id,writer_generation,'starting' FROM run_leases WHERE id=$1`, member.LeaseID, id, claim)
		r := commandClaim{OrgID: pgvalue.UUID(f.OrgID), CommandID: pgvalue.UUID(id)}
		if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,writer_generation FROM computer_commands WHERE id=$1`, id).Scan(&r.ComputerInstanceID, &r.WriterGeneration); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, r)
	}
	execute := func(r commandClaim, w workerActor) (commandClaimAuthority, error) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return commandClaimAuthority{}, err
		}
		defer tx.Rollback(t.Context())
		a, err := claimCommand(t.Context(), tx, w, r)
		if err != nil {
			return a, err
		}
		return a, tx.Commit(t.Context())
	}
	for _, r := range requests[:2] {
		foreign := r
		foreign.OrgID = pgvalue.UUID(uuid.NewV7())
		if _, err := execute(foreign, worker); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("foreign organization: %v", err)
		}
		replaced := r
		replaced.ComputerInstanceID = pgvalue.UUID(uuid.NewV7())
		if _, err := execute(replaced, worker); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("other instance: %v", err)
		}
		wrong := r
		wrong.WriterGeneration++
		if _, err := execute(wrong, worker); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("wrong generation: %v", err)
		}
		stale := worker
		stale.ClaimVersion++
		if _, err := execute(r, stale); !errors.Is(err, errStaleWorkerClaims) {
			t.Fatalf("stale worker: %v", err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, r.ComputerInstanceID)
		if _, err := execute(r, worker); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expired writer: %v", err)
		}
		var status string
		if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, r.CommandID).Scan(&status); err != nil || status != "starting" {
			t.Fatalf("expiry mutation: %s %v", status, err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, r.ComputerInstanceID)
		for _, barrier := range []string{"draining", "checkpointing", "closed"} {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, r.ComputerInstanceID, barrier)
			if _, err := execute(r, worker); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s admission: %v", barrier, err)
			}
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='open' WHERE id=$1`, r.ComputerInstanceID)
		for _, status := range []string{"draining", "paused"} {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status=$2 WHERE id=$1`, runtest.WorkerGroupID, status)
			if _, err := execute(r, worker); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("new command on %s group: %v", status, err)
			}
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status='active' WHERE id=$1`, runtest.WorkerGroupID)
		first, err := execute(r, worker)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := execute(r, worker)
		if err != nil {
			t.Fatal(err)
		}
		if first.Command.Status != "running" || replay.Command.Revision != first.Command.Revision || replay.Command.StartedAt != first.Command.StartedAt || replay.Instance.ID != first.Instance.ID {
			t.Fatalf("claim/replay mismatch: %+v %+v", first.Command, replay.Command)
		}
	}
	var running int
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE computer_instance_id=$1 AND status='running'`, requests[0].ComputerInstanceID).Scan(&running); err != nil || running != 2 {
		t.Fatalf("concurrent commands=%d %v", running, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='running' AND l.status='running' AND i.desired_state='ready' AND i.mount_state='mounted' AND i.admission_state='open' AND i.capture_checkpoint_id IS NULL AND i.reclaimed_at IS NULL FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$1`, member.LeaseID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("peer/physical changed=%v %v", !unchanged, err)
	}
	drained, err := db.New(f.Pool).DrainWorkerHost(t.Context(), db.DrainWorkerHostParams{
		ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execute(requests[0], worker); !errors.Is(err, errStaleWorkerClaims) {
		t.Fatalf("stale credential after drain: %v", err)
	}
	worker.ClaimVersion = drained.ClaimVersion
	for _, status := range []string{"active", "draining", "paused"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status=$2 WHERE id=$1`, runtest.WorkerGroupID, status)
		var revision int64
		var started time.Time
		if err := f.Pool.QueryRow(t.Context(), `SELECT revision,started_at FROM computer_commands WHERE id=$1`, requests[0].CommandID).Scan(&revision, &started); err != nil {
			t.Fatal(err)
		}
		replay, err := execute(requests[0], worker)
		if err != nil {
			t.Fatal(err)
		}
		if replay.Command.Revision != revision || !replay.Command.StartedAt.Time.Equal(started) || replay.Instance.ID != requests[0].ComputerInstanceID || replay.Instance.WriterGeneration != requests[0].WriterGeneration {
			t.Fatalf("draining replay changed receipt or writer: %+v", replay)
		}
		if _, err := execute(requests[2], worker); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("starting command entered drain: %v", err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status='active' WHERE id=$1`, runtest.WorkerGroupID)
	staleEpoch := worker
	staleEpoch.WorkerEpoch++
	if _, err := execute(requests[0], staleEpoch); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale epoch during drain: %v", err)
	}
	wrongGeneration := requests[0]
	wrongGeneration.WriterGeneration++
	if _, err := execute(wrongGeneration, worker); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale writer during drain: %v", err)
	}
	for _, barrier := range []string{"checkpointing", "closed"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, requests[0].ComputerInstanceID, barrier)
		if _, err := execute(requests[0], worker); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("running command entered %s: %v", barrier, err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=$1`, requests[0].ComputerInstanceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, requests[0].CommandID)
	if _, err := execute(requests[0], worker); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelled launch admitted: %v", err)
	}

	cancelGrant := func(r commandClaim) (commandClaimAuthority, error) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return commandClaimAuthority{}, err
		}
		defer tx.Rollback(t.Context())
		a, err := claimCommandCancellation(t.Context(), tx, worker, r)
		if err != nil {
			return a, err
		}
		return a, tx.Commit(t.Context())
	}
	for _, state := range []string{"open", "draining"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, requests[0].ComputerInstanceID, state)
		if a, err := cancelGrant(requests[0]); err != nil || a.Command.Status != "stopping" || len(a.Secrets) != 0 {
			t.Fatalf("cancel grant: %+v %v", a, err)
		}
	}
	if _, err := cancelGrant(requests[1]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("uncancelled peer granted: %v", err)
	}
	stale := requests[0]
	stale.WriterGeneration++
	if _, err := cancelGrant(stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale cancellation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, requests[0].ComputerInstanceID)
	if _, err := cancelGrant(requests[0]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired cancellation: %v", err)
	}
}
