package controlplane

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// Public cancellation, replayed from its receipt, is what lets the worker
// host report a cancelled result or an exit that raced the cancellation.
func TestCommandPublicCancellationAdmitsCancelledCompletion(t *testing.T) {
	for _, outcome := range []string{"computer_command_cancelled", "exited_after_cancel"} {
		t.Run(outcome, func(t *testing.T) {
			f := runtest.New(t)
			member := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, member.RunID)
			bound := commandtest.Bound(t, f, member.LeaseID, "running")
			report := command.CompletionReport{OrgID: f.OrgID, CommandID: bound.ID, InstanceID: bound.InstanceID, WriterGeneration: bound.WriterGeneration, Outcome: outcome}
			if outcome == "exited_after_cancel" {
				code := int32(17)
				report.Outcome, report.ExitCode = "exited", &code
			}
			worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			if report.Outcome == "computer_command_cancelled" {
				if err := command.Complete(t.Context(), f.Pool, worker, report); !errors.Is(err, command.ErrChanged) {
					t.Fatalf("unsolicited cancellation accepted: %v", err)
				}
			}
			server := &Server{db: db.New(f.Pool), tx: f.Pool}
			var receipt api.CommandCancelReceipt
			for range 2 {
				err := server.inTx(t.Context(), func(work *txWork) error {
					got, err := cancelCommandInTx(t.Context(), work, db.GetCommandParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(bound.ID)})
					if err == nil && receipt.ID != "" && got != receipt {
						t.Errorf("cancel replay changed receipt")
					}
					receipt = got
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := command.Complete(t.Context(), f.Pool, worker, report); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, bound.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if want := map[string]string{"computer_command_cancelled": "cancelled", "exited": "exited"}[report.Outcome]; status != want {
				t.Fatalf("status = %s, want %s", status, want)
			}
		})
	}
}
