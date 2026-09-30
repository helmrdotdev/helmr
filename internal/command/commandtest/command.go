// Package commandtest places Computer Commands over a Run test database: a
// Command with its start claim, bound to a Run lease's Instance.
package commandtest

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// Command is a Command bound to an Instance at a writer generation.
type Command struct {
	ID               uuid.UUID
	ClaimID          uuid.UUID
	InstanceID       uuid.UUID
	WriterGeneration int64
}

// Bound inserts a Command in status, with an accepted start claim, bound to
// the Instance and writer generation of the Run lease. A running or stopping
// Command has started.
func Bound(t *testing.T, f runtest.Fixture, leaseID uuid.UUID, status string) Command {
	t.Helper()
	command := Command{ID: uuid.NewV7(), ClaimID: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at)
 VALUES($1,$2,'computer.command.start',$3,$3,now())`, command.ClaimID, f.EnvironmentID, dbtest.Hash(command.ClaimID.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,computer_instance_id,writer_generation,status,started_at)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text,computer_instance_id,writer_generation,$4::text,
 CASE WHEN $4::text IN ('running','stopping') THEN now() END FROM run_leases WHERE id=$1`, leaseID, command.ID, command.ClaimID, status)
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,writer_generation FROM computer_commands WHERE id=$1`, command.ID).Scan(&command.InstanceID, &command.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	return command
}

// Pending inserts a pending Command, with an accepted start claim, on the
// Run lease's Computer, and returns it with its revision.
func Pending(t *testing.T, f runtest.Fixture, leaseID uuid.UUID) (uuid.UUID, int64) {
	t.Helper()
	id, claim := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at)
 VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,status)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text,'pending' FROM run_leases WHERE id=$1`, leaseID, id, claim)
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM computer_commands WHERE id=$1`, id).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return id, revision
}
