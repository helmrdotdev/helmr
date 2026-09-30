package command

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestCommandCompletionPreservesSharedComputer(t *testing.T) {
	for _, outcome := range []string{"exited", "exited_after_cancel", "secret_revoked", "computer_command_cancelled", "computer_command_timed_out", "computer_command_scope_termination_failed", "computer_command_result_uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			secretRevoked := outcome == "secret_revoked"
			if secretRevoked {
				outcome = "computer_command_cancelled"
			}
			cancelBeforeCompletion := outcome == "exited_after_cancel"
			if cancelBeforeCompletion {
				outcome = "exited"
			}
			f := runtest.New(t)
			member := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, member.RunID)
			bound := commandtest.Bound(t, f, member.LeaseID, "running")
			id := bound.ID
			request := CompletionReport{OrgID: f.OrgID, CommandID: id, InstanceID: bound.InstanceID, WriterGeneration: bound.WriterGeneration, Outcome: outcome}
			var originalHead string
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id::text FROM computers c JOIN computer_commands command ON command.computer_id=c.id WHERE command.id=$1`, id).Scan(&originalHead); err != nil {
				t.Fatal(err)
			}
			if outcome != "exited" {
				request.Error = []byte(`{"detail":{"zz":1,"a":2},"value":1e2,"text":"\u0061"}`)
			}
			if outcome == "exited" {
				code := int32(17)
				request.ExitCode = &code
			}
			worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			execute := func(r CompletionReport) error {
				return Complete(t.Context(), f.Pool, worker, r)
			}
			if outcome == "computer_command_cancelled" {
				if err := execute(request); !errors.Is(err, ErrChanged) {
					t.Fatalf("unsolicited cancellation accepted: %v", err)
				}
			}
			if secretRevoked {
				q := db.New(f.Pool)
				before, err := q.GetCommand(t.Context(), db.GetCommandParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(id)})
				if err != nil {
					t.Fatal(err)
				}
				stopped, err := q.StopSecretRevokedComputerCommand(t.Context(), db.StopSecretRevokedComputerCommandParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(id), ExpectedRevision: before.Revision})
				if err != nil {
					t.Fatal(err)
				}
				if stopped.Status != "stopping" || stopped.TerminalAt.Valid || !stopped.CancelRequestedAt.Valid {
					t.Fatalf("revocation prematurely settled process: %+v", stopped)
				}
			} else if outcome == "computer_command_cancelled" || cancelBeforeCompletion {
				// Public cancellation's receipt and replay are covered with the
				// control plane; this is the transition it applies.
				requested, err := db.New(f.Pool).RequestComputerCommandCancellation(t.Context(), db.RequestComputerCommandCancellationParams{CommandID: pgvalue.UUID(id), EnvironmentID: pgvalue.UUID(f.EnvironmentID)})
				if err != nil {
					t.Fatal(err)
				}
				if requested.Status != "stopping" || !requested.CancelRequestedAt.Valid {
					t.Fatalf("cancellation request=%+v", requested)
				}
			}
			stale := request
			stale.WriterGeneration++
			if err := execute(stale); !errors.Is(err, ErrChanged) {
				t.Fatalf("stale generation: %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.InstanceID)
			if err := execute(request); !errors.Is(err, ErrChanged) {
				t.Fatalf("expired completion: %v", err)
			}
			var terminal bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT terminal_at IS NOT NULL FROM computer_commands WHERE id=$1`, id).Scan(&terminal); err != nil || terminal {
				t.Fatalf("expired completion persisted: %v %v", terminal, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, request.InstanceID)
			log := LogChunk{OrgID: request.OrgID, CommandID: request.CommandID, InstanceID: request.InstanceID, WriterGeneration: request.WriterGeneration, Stream: LogStdout, ObservedAt: time.Now(), Content: []byte("last")}
			if err := AppendLog(t.Context(), f.Pool, worker, log); err != nil {
				t.Fatal(err)
			}
			if err := execute(request); err != nil {
				t.Fatal(err)
			}
			var unchanged, reconciled bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='ready' AND i.mount_state='mounted' AND i.admission_state='open' AND i.reclaimed_at IS NULL AND c.head_disk_version_id=$3 AND r.status='running' AND l.status='running',command.process_reconciled_at IS NOT NULL FROM computer_commands command JOIN computer_instances i ON i.id=command.computer_instance_id JOIN computers c ON c.id=i.computer_id JOIN run_leases l ON l.id=$2 JOIN runs r ON r.id=l.run_id WHERE command.id=$1`, id, member.LeaseID, originalHead).Scan(&unchanged, &reconciled); err != nil {
				t.Fatal(err)
			}
			wantReconciled := outcome == "exited" || outcome == "computer_command_cancelled" || outcome == "computer_command_timed_out"
			if !unchanged || reconciled {
				t.Fatalf("shared physical state preserved=%v reconciled=%v", unchanged, reconciled)
			}

			reconcile := func() error {
				return Reconcile(t.Context(), f.Pool, worker, request)
			}
			if wantReconciled {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.InstanceID)
				if err := reconcile(); !errors.Is(err, ErrChanged) {
					t.Fatalf("expired reconciliation: %v", err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, request.InstanceID)
				for range 2 {
					if err := reconcile(); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NOT NULL FROM computer_commands WHERE id=$1`, id).Scan(&reconciled); err != nil || !reconciled {
					t.Fatalf("Guest acknowledgement not reconciled: %v %v", reconciled, err)
				}
			} else if err := reconcile(); !errors.Is(err, ErrChanged) {
				t.Fatalf("uncertain scope reconciled: %v", err)
			}
			log.ObservedSeq++
			if err := AppendLog(t.Context(), f.Pool, worker, log); !errors.Is(err, ErrChanged) {
				t.Fatalf("terminal output admitted: %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.InstanceID)
			if err := execute(request); err != nil {
				t.Fatalf("historical replay: %v", err)
			}
			conflicting := request
			conflicting.Outcome = "computer_command_launch_failed"
			conflicting.ExitCode = nil
			if err := execute(conflicting); !errors.Is(err, ErrChanged) {
				t.Fatalf("conflicting replay: %v", err)
			}
			// Replay stays accepted once the Instance was reclaimed.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances
 SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version+1,
 terminal_at=now(),terminal_reason_code='execution_lost',reclaimed_at=now(),mount_state='unmounted',unmounted_at=now(),admission_state='closed',
 reclaim_evidence='{"method":"host_reconciled"}' WHERE id=$1`, request.InstanceID)
			if err := execute(request); err != nil {
				t.Fatalf("replay after reclaim: %v", err)
			}
		})
	}
}
