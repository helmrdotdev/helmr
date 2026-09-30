package computer

import (
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDeleteWithoutMembersTombstonesAndReplays(t *testing.T) {
	for _, recoveryRequired := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery_required=%t", recoveryRequired), func(t *testing.T) {
			f := newFixture(t)
			computerID := f.insertComputer(t, "delete-me")
			if recoveryRequired {
				dbtest.MustExec(t, t.Context(), f.Pool, `
UPDATE computers
   SET status = 'recovery_required', desired_state = 'stopped', dirty_state = 'dirty_state_lost',
       recovery_id=$2, recovery_disk_version_id=head_disk_version_id, recovery_reason='worker_lost', recovery_started_at=now()
 WHERE id = $1`, computerID, uuid.NewV7())
			}
			var originalKey string
			if err := f.Pool.QueryRow(t.Context(), `SELECT key FROM computers WHERE id=$1`, computerID).Scan(&originalKey); err != nil || originalKey == "" {
				t.Fatalf("computer key before deletion = %q, %v", originalKey, err)
			}
			deletion := Deletion{Scope: f.scope, ComputerID: computerID, IdempotencyKey: "delete-without-members"}
			deleted, err := Delete(t.Context(), f.Pool, deletion)
			if err != nil {
				t.Fatal(err)
			}
			if deleted.Replayed || deleted.ComputerID != computerID {
				t.Fatalf("delete result = %+v", deleted)
			}
			var state db.ComputerStatus
			var desiredState, dirtyState string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status, desired_state, dirty_state FROM computers WHERE id = $1`, computerID).Scan(&state, &desiredState, &dirtyState); err != nil {
				t.Fatal(err)
			}
			if state != db.ComputerStatusDeleting || desiredState != "deleted" || dirtyState != "clean" {
				t.Fatalf("computer state = %s/%s/%s, want deleting/deleted/clean", state, desiredState, dirtyState)
			}
			finalized, err := db.New(f.Pool).FinalizeDeletingComputers(t.Context(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(finalized) != 1 || pgvalue.MustUUIDValue(finalized[0]) != computerID {
				t.Fatalf("finalized computers = %+v, want %s", finalized, computerID)
			}
			var revision int64
			var computerSpecID *uuid.UUID
			var key, sandboxDeclaredID *string
			var headVersionID *uuid.UUID
			var deletedAt time.Time
			if err := f.Pool.QueryRow(t.Context(), `
SELECT status, revision, computer_spec_id, key, sandbox_declared_id, head_disk_version_id, deleted_at
  FROM computers WHERE id = $1`, computerID).Scan(&state, &revision, &computerSpecID, &key, &sandboxDeclaredID, &headVersionID, &deletedAt); err != nil {
				t.Fatal(err)
			}
			if computerSpecID == nil || *computerSpecID == uuid.Nil() {
				t.Fatal("computer tombstone lost its spec")
			}
			if state != db.ComputerStatusDeleted || revision != 3 || key != nil || sandboxDeclaredID == nil || *sandboxDeclaredID != declaredID || headVersionID != nil || deletedAt.IsZero() {
				t.Fatalf("computer tombstone = %s rev=%d key=%v sandbox=%v head=%v deleted=%s", state, revision, key, sandboxDeclaredID, headVersionID, deletedAt)
			}
			replayed, err := Delete(t.Context(), f.Pool, deletion)
			if err != nil || !replayed.Replayed || replayed.ComputerID != computerID {
				t.Fatalf("delete replay = %+v, %v", replayed, err)
			}
			if _, err := Delete(t.Context(), f.Pool, Deletion{Scope: f.scope, ComputerID: computerID, IdempotencyKey: "delete-after-tombstone"}); err != nil {
				t.Fatalf("fresh delete after tombstone error = %v", err)
			}
			snapshot, err := Read(t.Context(), db.New(f.Pool), f.scope, computerID)
			if err != nil || snapshot.Status != StatusDeleted || snapshot.SandboxID != declaredID {
				t.Fatalf("tombstone snapshot=%+v %v", snapshot, err)
			}
			replacementKey := originalKey
			replacement, err := f.creator().Create(t.Context(), f.Pool, Request{Scope: f.scope, DeclaredID: declaredID, Key: &replacementKey, IdempotencyKey: "recreate-after-delete"})
			if err != nil {
				t.Fatal(err)
			}
			if replacement.ComputerID == computerID || replacement.Snapshot.Key == nil || *replacement.Snapshot.Key != replacementKey {
				t.Fatalf("replacement computer = %+v", replacement)
			}
		})
	}
}

func TestDeleteRejectsMembersAndAbsentComputers(t *testing.T) {
	f := newFixture(t)
	work := f.AddRunLease(t, "running", time.Now())
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	busy := Deletion{Scope: f.scope, ComputerID: computerID, IdempotencyKey: "busy"}
	if _, err := Delete(t.Context(), f.Pool, busy); !errors.Is(err, ErrBusy) {
		t.Fatalf("member-held delete = %v", err)
	}
	// A rejected deletion rolls back its claim, so a later attempt runs again.
	if _, err := Delete(t.Context(), f.Pool, busy); !errors.Is(err, ErrBusy) {
		t.Fatalf("retried member-held delete = %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM idempotency_claims WHERE operation='computer.delete'`); n != 0 {
		t.Fatalf("rejected deletions left %d claims", n)
	}
	if _, err := Delete(t.Context(), f.Pool, Deletion{Scope: f.scope, ComputerID: uuid.NewV7()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent delete = %v", err)
	}
	other := f.scope
	other.ProjectID = uuid.NewV7()
	if _, err := Delete(t.Context(), f.Pool, Deletion{Scope: other, ComputerID: computerID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope delete = %v", err)
	}
}

func TestDeleteRequestsCloseOfUnreclaimedInstance(t *testing.T) {
	f := newFixture(t)
	work := f.AddRunLease(t, "running", time.Now())
	var computerID, instanceID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id, computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID, &instanceID); err != nil {
		t.Fatal(err)
	}
	// The Instance outlives its settled member: deletion requests its close.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancelled',terminal_at=now(),failure='{"code":"cancelled","message":"Cancelled","details":{}}',current_run_lease_id=NULL,active_started_at=NULL WHERE id=$1`, work.RunID)
	if _, err := Delete(t.Context(), f.Pool, Deletion{Scope: f.scope, ComputerID: computerID}); err != nil {
		t.Fatal(err)
	}
	var desiredState, reason string
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_state, desired_reason FROM computer_instances WHERE id=$1`, instanceID).Scan(&desiredState, &reason); err != nil {
		t.Fatal(err)
	}
	if desiredState != "closed" || reason != "computer_deleted" {
		t.Fatalf("instance desired = %s/%s", desiredState, reason)
	}
}
