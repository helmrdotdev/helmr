package db_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Recover the same transaction after each rejected row. This catches SQL-NULL
// acceptance while proving that a real CHECK or foreign key rejects the write.
func rejectSchemaRow(t *testing.T, tx pgx.Tx, code, statement string, args ...any) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), tx, "SAVEPOINT invalid_row")
	_, err := tx.Exec(t.Context(), statement, args...)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != code || pgerr.ConstraintName == "" {
		t.Errorf("invalid row error=%v, want SQLSTATE %s with constraint name", err, code)
	}
	dbtest.MustExec(t, t.Context(), tx, "ROLLBACK TO SAVEPOINT invalid_row")
	dbtest.MustExec(t, t.Context(), tx, "RELEASE SAVEPOINT invalid_row")
}

func schemaTurn(t *testing.T, f agenttest.Fixture, tx pgx.Tx) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,request_digest,input) VALUES($1,$2,$3,$4,1,'user',$5,'enqueue',$3,decode(repeat('01',32),'hex'),'null')`, f.Environment, id, f.Session, f.Computer, f.User)
	return id
}

func TestSchemaTurnCompletionRequiresRecordedResultAndPublishedSave(t *testing.T) {
	f := agenttest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	turn := schemaTurn(t, f, tx)
	for _, set := range []string{"status='running'", "status='failed'", "terminal_at=clock_timestamp()", "status='completed',terminal_at=clock_timestamp()", "result_recorded_at=clock_timestamp()"} {
		rejectSchemaRow(t, tx, "23514", "UPDATE turns SET "+set+" WHERE environment_id=$1 AND id=$2", f.Environment, turn)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE turns SET status='running',started_at=clock_timestamp(),process_epoch=1 WHERE environment_id=$1 AND id=$2`, f.Environment, turn)
	rejectSchemaRow(t, tx, "23514", `UPDATE turns SET status='finalizing' WHERE environment_id=$1 AND id=$2`, f.Environment, turn)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE turns SET status='finalizing',processing_closed_at=clock_timestamp(),result_recorded_at=clock_timestamp(),result_digest=decode(repeat('02',32),'hex'),result='null',drain_evidence='drained' WHERE environment_id=$1 AND id=$2`, f.Environment, turn)
	save := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id) VALUES($1,$2,$3,1,1,$4)`, f.Environment, save, f.Computer, turn)
	rejectSchemaRow(t, tx, "23503", `UPDATE turns SET status='completed',terminal_at=clock_timestamp(),completion_save_id=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, turn, save)
	rejectSchemaRow(t, tx, "23514", `UPDATE computer_saves SET status='published',publication_evidence='published' WHERE environment_id=$1 AND id=$2`, f.Environment, save)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_saves SET status='published',flush_acknowledged_at=clock_timestamp(),captured_at=clock_timestamp(),captured_root_digest=decode(repeat('03',32),'hex'),capture_evidence='captured',publication_evidence='published',root_id=(SELECT initial_root_id FROM computers WHERE environment_id=$1 AND id=$3) WHERE environment_id=$1 AND id=$2`, f.Environment, save, f.Computer)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE turns SET status='completed',terminal_at=clock_timestamp(),completion_save_id=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, turn, save)
}

func TestSchemaPhysicalStopAndCheckpointRejectPartialEvidence(t *testing.T) {
	f := agenttest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	rejectSchemaRow(t, tx, "23514", `UPDATE session_processes SET status='stopped' WHERE environment_id=$1 AND session_id=$2`, f.Environment, f.Session)
	rejectSchemaRow(t, tx, "23514", `UPDATE computer_leases SET fenced_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	save, checkpoint := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,1)`, f.Environment, save, f.Computer)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest) VALUES($1,$2,$3,1,1,$4,'capturing',decode('01','hex'),clock_timestamp()+interval '1 minute',decode(repeat('02',32),'hex'))`, f.Environment, checkpoint, f.Computer, save)
	for _, set := range []string{"status='ready'", "manifest=decode('01','hex')", "status='lost'", "capture_request=NULL", "target_lease_epoch=2", "restore_control_version=2", "restore_identity=decode(repeat('00',32),'hex')"} {
		rejectSchemaRow(t, tx, "23514", "UPDATE computer_checkpoints SET "+set+" WHERE environment_id=$1 AND id=$2", f.Environment, checkpoint)
	}
	rejectSchemaRow(t, tx, "23503", `UPDATE computer_checkpoints SET target_lease_epoch=2,restore_control_version=2 WHERE environment_id=$1 AND id=$2`, f.Environment, checkpoint)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_checkpoints SET status='lost',terminal_evidence='host fenced',capture_request=NULL WHERE environment_id=$1 AND id=$2`, f.Environment, checkpoint)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.Environment, f.Session)
}
