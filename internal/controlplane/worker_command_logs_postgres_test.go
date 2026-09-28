package controlplane

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestCommandLogProducerFenceReplayAndCompletion(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	commandID, claimID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at)
 VALUES($1,$2,'computer.command.start',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash(claimID.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,computer_instance_id,writer_generation,status,started_at)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text,computer_instance_id,2,'running',now() FROM run_leases WHERE id=$1`, work.LeaseID, commandID, claimID)
	var instanceID uuid.UUID
	var generation int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,writer_generation FROM computer_commands WHERE id=$1`, commandID).Scan(&instanceID, &generation); err != nil {
		t.Fatal(err)
	}
	producer := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
	request := workerapi.CommandLogAppendRequest{
		OrgID: f.OrgID.String(), CommandID: commandID.String(), ComputerInstanceID: instanceID.String(),
		WriterGeneration: generation, Stream: workerapi.LogStreamStdout,
		ObservedSeq: 0, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte{0, 255, 128},
	}
	call := func(request workerapi.CommandLogAppendRequest, want error) {
		t.Helper()
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		err = appendCommandLog(t.Context(), tx, producer, request)
		if !errors.Is(err, want) {
			t.Fatalf("append error=%v, want %v", err, want)
		}
		if err == nil {
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	call(request, nil)
	call(request, nil)
	for _, test := range []struct {
		name   string
		mutate func(*workerapi.CommandLogAppendRequest)
	}{
		{"organization", func(r *workerapi.CommandLogAppendRequest) { r.OrgID = uuid.NewV7().String() }},
		{"command", func(r *workerapi.CommandLogAppendRequest) { r.CommandID = uuid.NewV7().String() }},
		{"instance", func(r *workerapi.CommandLogAppendRequest) { r.ComputerInstanceID = uuid.NewV7().String() }},
		{"generation", func(r *workerapi.CommandLogAppendRequest) { r.WriterGeneration++ }},
		{"changed content", func(r *workerapi.CommandLogAppendRequest) { r.Content = []byte("different") }},
		{"changed timestamp", func(r *workerapi.CommandLogAppendRequest) { r.ObservedAt = r.ObservedAt.Add(time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.mutate(&changed)
			call(changed, pgx.ErrNoRows)
		})
	}
	worker := producer
	for _, test := range []struct {
		name   string
		mutate func(*workerActor)
	}{
		{"host", func(w *workerActor) { w.WorkerHostID = uuid.NewV7() }},
		{"epoch", func(w *workerActor) { w.WorkerEpoch++ }},
		{"group", func(w *workerActor) { w.WorkerGroupID = uuid.NewV7() }},
		{"worker claim", func(w *workerActor) { w.ClaimVersion++ }},
		{"group claim", func(w *workerActor) { w.GroupClaimVersion++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.mutate(&producer)
			defer func() { producer = worker }()
			unaccepted := request
			unaccepted.ObservedSeq++
			call(unaccepted, pgx.ErrNoRows)
		})
	}
	var count int
	var bytes []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),min(encode(content,'hex')) FROM telemetry_outbox WHERE command_id=$1`, commandID).Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != 1 || string(bytes) != "00ff80" {
		t.Fatalf("accepted rows=%d content=%q", count, bytes)
	}
	large := request
	large.ObservedSeq = uint64(1<<63 - 1)
	large.Content = make([]byte, telemetry.MaxRunLogContentBytes)
	call(large, nil)
	large.Content = append(large.Content, 0)
	call(large, errInvalidCommandLog)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, instanceID)
	call(request, pgx.ErrNoRows)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, instanceID)
	// Cancellation still permits the producer to flush before terminalization.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, commandID)
	request.ObservedSeq++
	call(request, nil)
	// Completion closes producer admission even before whole-instance cleanup.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=now(),terminal_reason_code='computer_command_cancelled' WHERE id=$1`, commandID)
	request.ObservedSeq++
	call(request, pgx.ErrNoRows)
}
