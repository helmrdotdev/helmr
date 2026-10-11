package db_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestOwnershipSurvivingScopePathsRejectAndRollback(t *testing.T) {
	f := agenttest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	turn := schemaTurn(t, f, tx)
	for _, set := range []string{"environment_id", "session_id", "computer_id"} {
		rejectSchemaRow(t, tx, "23503", "UPDATE turns SET "+set+"=$3 WHERE environment_id=$1 AND id=$2", f.Environment, turn, uuid.NewV7())
	}
	rejectSchemaRow(t, tx, "23503", `UPDATE session_processes SET computer_lease_epoch=2 WHERE environment_id=$1 AND session_id=$2`, f.Environment, f.Session)
	rejectSchemaRow(t, tx, "23503", `UPDATE computer_leases SET worker_host_id=$3 WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer, uuid.NewV7())
	command := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_commands(environment_id,id,computer_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,ARRAY['true'],'{}','',1000,'user','fixture')`, f.Environment, command, f.Computer)
	for _, set := range []string{"environment_id", "computer_id"} {
		rejectSchemaRow(t, tx, "23503", "UPDATE computer_commands SET "+set+"=$3 WHERE environment_id=$1 AND id=$2", f.Environment, command, uuid.NewV7())
	}
	rejectSchemaRow(t, tx, "23503", `UPDATE computer_commands SET computer_lease_epoch=2 WHERE environment_id=$1 AND id=$2`, f.Environment, command)
	var count int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1 AND id=$2`, f.Environment, turn).Scan(&count); err != nil || count != 1 {
		t.Fatalf("valid owner lost after rollback: %d %v", count, err)
	}
}
