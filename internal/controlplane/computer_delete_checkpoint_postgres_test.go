package controlplane

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestComputerDeleteRetiresParkedCheckpoint(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, true)
	cp := computertest.Complete(t, f, ref, manifest, objects)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='cancelled',terminal_at=now(),failure='{"code":"cancelled","message":"Cancelled","details":{}}',current_run_lease_id=NULL,active_started_at=NULL WHERE computer_id=$1`, cp.ComputerID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"session_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, ref.InstanceID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = computer.Delete(t.Context(), f.Pool, computer.Deletion{Scope: computer.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID}, ComputerID: pgvalue.MustUUIDValue(cp.ComputerID), IdempotencyKey: "delete-parked"}); err != nil {
		t.Fatal(err)
	}
	var retired bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='invalid' AND invalidation_reason_code='computer_deleted' AND computer_payload_required IS NULL FROM computer_checkpoints WHERE id=$1`, cp.ID).Scan(&retired); err != nil || !retired {
		t.Fatalf("checkpoint not retired: %v %v", retired, err)
	}
	if _, err = db.New(f.Pool).FinalizeDeletingComputers(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	row, err := db.New(f.Pool).GetComputer(t.Context(), db.GetComputerParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: cp.ComputerID})
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "deleted" || row.Residency != "cold" {
		t.Fatalf("deleted computer remains parked: %+v", row)
	}
}

func TestComputerDeleteRetiresConsumedCheckpoint(t *testing.T) {
	f, authority, fence := dispatchtest.Restore(t, true)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := authority.CommitRestore(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err = tx.QueryRow(t.Context(), `SELECT writer_generation FROM computer_instances WHERE id=$1`, fence.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err = computer.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, generation, nil); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='cancelled',terminal_at=now(),failure='{"code":"cancelled","message":"Cancelled","details":{}}',current_run_lease_id=NULL,active_started_at=NULL WHERE computer_id=$1`, cp.ComputerID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = computer.Delete(t.Context(), f.Pool, computer.Deletion{Scope: computer.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID}, ComputerID: pgvalue.MustUUIDValue(cp.ComputerID), IdempotencyKey: "delete-consumed"}); err != nil {
		t.Fatal(err)
	}
	var retainedReceipt bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='invalid' AND computer_payload_required IS NULL AND resume_computer_instance_id=$2 AND resume_committed_at=$3 FROM computer_checkpoints WHERE id=$1`, cp.ID, cp.ResumeComputerInstanceID, cp.ResumeCommittedAt).Scan(&retainedReceipt); err != nil || !retainedReceipt {
		t.Fatalf("consumed payload or receipt wrong: %v %v", retainedReceipt, err)
	}
}
