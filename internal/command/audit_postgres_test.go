package command

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAuditCommandCompletionReceiptSurvivesAuditRemoval(t *testing.T) {
	f := commandFixture(t)
	start := f.start(t, "audit-completion")
	id := uuid.UUID(start.Command.ID.Bytes)
	report := f.report(id, "exited")
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_command_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_command_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_command_audit()`)
	if err := Complete(t.Context(), policy, f.Pool, f.host(), report); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	if f.get(t, id).TerminalAt.Valid {
		t.Fatal("terminal outcome escaped audit rollback")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_command_audit ON audit_events`)
	if err := Complete(t.Context(), policy, f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	var count int
	var valid bool
	err := f.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(actor_kind='service' AND actor_service='worker-host' AND credential_id=$2 AND credential_version=1 AND runtime_generation=1 AND causality->>'initiator_id'=$3) FROM audit_events WHERE target_id=$1 AND action='command.completed'`, id, f.Worker, f.User.String()).Scan(&count, &valid)
	if err != nil || count != 1 || !valid {
		t.Fatalf("worker attribution: %d %v %v", count, valid, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM audit_events WHERE target_id=$1 AND action='command.completed'`, id)
	if err := Complete(t.Context(), policy, f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE target_id=$1 AND action='command.completed'`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("replay recreated removed audit event: %d %v", count, err)
	}
}

func TestAuditCommandLossRollbackAndReplay(t *testing.T) {
	f := commandFixture(t)
	start := f.start(t, "audit-lost")
	id := uuid.UUID(start.Command.ID.Bytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	candidate := RecoveryCandidate{EnvironmentID: f.Environment, ComputerID: f.Computer, CommandID: id, ExpectedRevision: f.get(t, id).Revision}
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_loss_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_loss_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_loss_audit()`)
	recover := func() error {
		return Recover(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, candidate)
	}
	if err := recover(); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	if f.get(t, id).TerminalAt.Valid {
		t.Fatal("loss escaped rollback")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_loss_audit ON audit_events`)
	if err := recover(); err != nil {
		t.Fatal(err)
	}
	var count int
	err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE target_id=$1 AND action='command.failed' AND outcome='unknown' AND actor_service='command-recovery' AND details->>'reason'='worker_lost'`, id).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("loss fact: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM audit_events WHERE target_id=$1`, id)
	candidate.ExpectedRevision = f.get(t, id).Revision
	if err := recover(); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE target_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("recreated loss: %d %v", count, err)
	}
}
