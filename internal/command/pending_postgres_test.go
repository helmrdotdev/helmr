package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func failPendingInTx(t *testing.T, f runtest.Fixture, pending Pending, failure Failure) error {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = FailPending(t.Context(), tx, pending, failure); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func TestFailPendingRecordsFailureOnce(t *testing.T) {
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	id, revision := commandtest.Pending(t, f, lease.LeaseID)
	failure := Failure{Code: "computer_command_assignment_timed_out", Detail: []byte(`{"code":"computer_command_assignment_timed_out"}`)}
	for name, pending := range map[string]Pending{
		"foreign organization": {OrgID: uuid.NewV7(), CommandID: id, ExpectedRevision: revision},
		"unknown command":      {OrgID: f.OrgID, CommandID: uuid.NewV7(), ExpectedRevision: revision},
		"stale revision":       {OrgID: f.OrgID, CommandID: id, ExpectedRevision: revision + 1},
	} {
		if err := failPendingInTx(t, f, pending, failure); !errors.Is(err, ErrChanged) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	pending := Pending{OrgID: f.OrgID, CommandID: id, ExpectedRevision: revision}
	if err := failPendingInTx(t, f, pending, failure); err != nil {
		t.Fatal(err)
	}
	var status, reason, detail, failed string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,terminal_reason_code,error::text,failure_reason FROM computer_commands WHERE id=$1`, id).Scan(&status, &reason, &detail, &failed); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != failure.Code || detail != `{"code": "computer_command_assignment_timed_out"}` || failed != "dispatch_failed" {
		t.Fatalf("failed Command status=%s reason=%s error=%s failure=%s", status, reason, detail, failed)
	}
	pending.ExpectedRevision++
	if err := failPendingInTx(t, f, pending, failure); !errors.Is(err, ErrChanged) {
		t.Fatalf("terminal Command failed again: %v", err)
	}
}

// A candidate bound after discovery locks its new Instance, as every other
// Command operation does, before its stale revision rejects it.
func TestFailPendingLocksInstanceBoundAfterDiscovery(t *testing.T) {
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	id, revision := commandtest.Pending(t, f, lease.LeaseID)
	var computerID, instanceID uuid.UUID
	var generation int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id,computer_instance_id,writer_generation FROM run_leases WHERE id=$1`, lease.LeaseID).Scan(&computerID, &instanceID, &generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.New(f.Pool).BindComputerCommandInstance(t.Context(), db.BindComputerCommandInstanceParams{
		EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(computerID), CommandID: pgvalue.UUID(id),
		ComputerInstanceID: pgvalue.UUID(instanceID), WriterGeneration: generation,
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = FailPending(t.Context(), tx, Pending{OrgID: f.OrgID, CommandID: id, ExpectedRevision: revision}, Failure{Code: "computer_preparation_exhausted", Detail: []byte(`{}`)}); !errors.Is(err, ErrChanged) {
		t.Fatalf("bound candidate: %v", err)
	}
	var pgErr *pgconn.PgError
	if _, err = f.Pool.Exec(t.Context(), `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE NOWAIT`, instanceID); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("bound Instance lock = %v, want lock_not_available", err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, id).Scan(&status); err != nil || status != "starting" {
		t.Fatalf("bound Command status=%s err=%v", status, err)
	}
	if _, err = f.Pool.Exec(t.Context(), `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE NOWAIT`, instanceID); err != nil {
		t.Fatalf("Instance lock after rollback: %v", err)
	}
}
