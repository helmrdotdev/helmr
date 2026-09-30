package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func commandRef(f runtest.Fixture, commandID uuid.UUID) Ref {
	return Ref{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, CommandID: commandID}
}

// Cancel stores its receipt on the idempotency claim, and a replay rejects a
// stored receipt that differs from its claim.
func TestCancelStoresAndValidatesReceipt(t *testing.T) {
	f, computerID := computerFixture(t)
	created, err := Create(t.Context(), f.Pool, createRequest(f, computerID, "cancel-receipt"))
	if err != nil {
		t.Fatal(err)
	}
	commandID := uuid.UUID(created.ID.Bytes)
	receipt, err := Cancel(t.Context(), f.Pool, commandRef(f, commandID))
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := f.Pool.QueryRow(t.Context(), `SELECT receipt::text FROM idempotency_claims WHERE id=$1`, receipt.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := `{"id": "` + receipt.ID + `", "status": "accepted", "target_id": "` + commandID.String() + `"}`; stored != want {
		t.Fatalf("stored receipt = %s, want %s", stored, want)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE idempotency_claims SET receipt=jsonb_set(receipt,'{status}','"rejected"') WHERE id=$1`, receipt.ID)
	if _, err := Cancel(t.Context(), f.Pool, commandRef(f, commandID)); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("altered receipt = %v", err)
	}
}

// Cancellation replays its receipt after the Command's result was pruned,
// and a pruned Command stays readable in its scope for later scope checks.
func TestCancelReplaysAfterResultPruning(t *testing.T) {
	f, computerID := computerFixture(t)
	created, err := Create(t.Context(), f.Pool, createRequest(f, computerID, "cancel-pruned"))
	if err != nil {
		t.Fatal(err)
	}
	ref := commandRef(f, uuid.UUID(created.ID.Bytes))
	first, err := Cancel(t.Context(), f.Pool, ref)
	if err != nil || first.Status != "accepted" || first.TargetID != ref.CommandID.String() {
		t.Fatalf("cancel = %+v, %v", first, err)
	}
	cancelled, err := Get(t.Context(), db.New(f.Pool), ref)
	if err != nil || cancelled.Status != db.ComputerCommandStatusCancelled || !cancelled.TerminalAt.Valid {
		t.Fatalf("cancelled = %+v, %v", cancelled, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET result_expires_at=now()-interval '1 day' WHERE id=$1`, ref.CommandID)
	if pruned, err := db.New(f.Pool).PruneExpiredComputerCommandResults(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}
	read, err := Get(t.Context(), db.New(f.Pool), ref)
	if err != nil || !read.ResultPrunedAt.Valid || read.Argv != nil {
		t.Fatalf("pruned read = %+v, %v", read, err)
	}
	replayed, err := Cancel(t.Context(), f.Pool, ref)
	if err != nil || replayed != first {
		t.Fatalf("replay = %+v, %v; want %+v", replayed, err, first)
	}
	after, err := Get(t.Context(), db.New(f.Pool), ref)
	if err != nil || after.Revision != read.Revision {
		t.Fatalf("replay changed the Command: revision %d -> %d, %v", read.Revision, after.Revision, err)
	}
}

// Get and Cancel find a Command only in its organization, project and
// Environment.
func TestCommandReadsIsolateEveryScopeCoordinate(t *testing.T) {
	f, computerID := computerFixture(t)
	created, err := Create(t.Context(), f.Pool, createRequest(f, computerID, "isolated"))
	if err != nil {
		t.Fatal(err)
	}
	ref := commandRef(f, uuid.UUID(created.ID.Bytes))
	for name, mutate := range map[string]func(*Ref){
		"organization": func(r *Ref) { r.OrgID = uuid.NewV7() },
		"project":      func(r *Ref) { r.ProjectID = uuid.NewV7() },
		"environment":  func(r *Ref) { r.EnvironmentID = uuid.NewV7() },
		"command":      func(r *Ref) { r.CommandID = uuid.NewV7() },
	} {
		t.Run(name, func(t *testing.T) {
			other := ref
			mutate(&other)
			if _, err := Get(t.Context(), db.New(f.Pool), other); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get = %v", err)
			}
			if _, err := Cancel(t.Context(), f.Pool, other); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cancel = %v", err)
			}
		})
	}
	if got, err := Get(t.Context(), db.New(f.Pool), ref); err != nil || got.ID != created.ID || got.Status != db.ComputerCommandStatusPending {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

// Public cancellation, replayed from its receipt, is what lets the worker
// host report a cancelled result or an exit that raced the cancellation.
func TestCancelAdmitsCancelledCompletion(t *testing.T) {
	for _, outcome := range []string{"computer_command_cancelled", "exited_after_cancel"} {
		t.Run(outcome, func(t *testing.T) {
			f := runtest.New(t)
			member := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, member.RunID)
			bound := commandtest.Bound(t, f, member.LeaseID, "running")
			report := CompletionReport{OrgID: f.OrgID, CommandID: bound.ID, InstanceID: bound.InstanceID, WriterGeneration: bound.WriterGeneration, Outcome: outcome}
			if outcome == "exited_after_cancel" {
				code := int32(17)
				report.Outcome, report.ExitCode = "exited", &code
			}
			worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			if report.Outcome == "computer_command_cancelled" {
				if err := Complete(t.Context(), f.Pool, worker, report); !errors.Is(err, ErrChanged) {
					t.Fatalf("unsolicited cancellation accepted: %v", err)
				}
			}
			first, err := Cancel(t.Context(), f.Pool, commandRef(f, bound.ID))
			if err != nil {
				t.Fatal(err)
			}
			var status string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, bound.ID).Scan(&status); err != nil || status != "stopping" {
				t.Fatalf("cancelled bound Command status = %s, %v", status, err)
			}
			if replayed, err := Cancel(t.Context(), f.Pool, commandRef(f, bound.ID)); err != nil || replayed != first {
				t.Fatalf("cancel replay = %+v, %v; want %+v", replayed, err, first)
			}
			if err := Complete(t.Context(), f.Pool, worker, report); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, bound.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if want := map[string]string{"computer_command_cancelled": "cancelled", "exited": "exited"}[report.Outcome]; status != want {
				t.Fatalf("status = %s, want %s", status, want)
			}
		})
	}
}
