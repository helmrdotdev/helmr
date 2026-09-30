package controlplane

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
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
			id, claim := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,computer_instance_id,writer_generation,status,started_at)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text,computer_instance_id,writer_generation,'running',now() FROM run_leases WHERE id=$1`, member.LeaseID, id, claim)
			request := workerapi.ComputerCommandCompleteRequest{OrgID: f.OrgID.String(), CommandID: id.String(), Outcome: outcome}
			var originalHead string
			if err := f.Pool.QueryRow(t.Context(), `SELECT i.id::text,i.writer_generation,c.head_disk_version_id::text FROM computer_commands command JOIN computer_instances i ON i.id=command.computer_instance_id JOIN computers c ON c.id=i.computer_id WHERE command.id=$1`, id).Scan(&request.ComputerInstanceID, &request.WriterGeneration, &originalHead); err != nil {
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
			execute := func(r workerapi.ComputerCommandCompleteRequest) error {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback(t.Context())
				if err = completeCommand(t.Context(), tx, worker, r); err != nil {
					return err
				}
				return tx.Commit(t.Context())
			}
			if outcome == "computer_command_cancelled" {
				if err := execute(request); !errors.Is(err, pgx.ErrNoRows) {
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
				server := &Server{db: db.New(f.Pool), tx: f.Pool}
				var receipt api.CommandCancelReceipt
				for range 2 {
					err := server.inTx(t.Context(), func(work *txWork) error {
						got, err := cancelCommandInTx(t.Context(), work, db.GetCommandParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(id)})
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
			}
			stale := request
			stale.WriterGeneration++
			if err := execute(stale); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale generation: %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
			if err := execute(request); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("expired completion: %v", err)
			}
			var terminal bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT terminal_at IS NOT NULL FROM computer_commands WHERE id=$1`, id).Scan(&terminal); err != nil || terminal {
				t.Fatalf("expired completion persisted: %v %v", terminal, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, request.ComputerInstanceID)
			log := workerapi.CommandLogAppendRequest{OrgID: request.OrgID, CommandID: request.CommandID, ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration, Stream: workerapi.LogStreamStdout, ObservedAt: time.Now(), Content: []byte("last")}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err = appendCommandLog(t.Context(), tx, worker, log); err != nil {
				tx.Rollback(t.Context())
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = execute(request); err != nil {
				t.Fatal(err)
			}
			var unchanged, reconciled bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='ready' AND i.mount_state='mounted' AND i.admission_state='open' AND i.reclaimed_at IS NULL AND c.head_disk_version_id=$3 AND r.status='running' AND l.status='running',command.process_reconciled_at IS NOT NULL FROM computer_commands command JOIN computer_instances i ON i.id=command.computer_instance_id JOIN computers c ON c.id=i.computer_id JOIN run_leases l ON l.id=$2 JOIN runs r ON r.id=l.run_id WHERE command.id=$1`, id, member.LeaseID, originalHead).Scan(&unchanged, &reconciled); err != nil {
				t.Fatal(err)
			}
			wantReconciled := outcome == "exited" || outcome == "computer_command_cancelled" || outcome == "computer_command_timed_out"
			if !unchanged || reconciled {
				t.Fatalf("shared physical state preserved=%v reconciled=%v", unchanged, reconciled)
			}

			reconcile := func() error {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback(t.Context())
				if err = reconcileCommand(t.Context(), tx, worker, request); err != nil {
					return err
				}
				return tx.Commit(t.Context())
			}
			if wantReconciled {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
				if err := reconcile(); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("expired reconciliation: %v", err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, request.ComputerInstanceID)
				for range 2 {
					if err := reconcile(); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NOT NULL FROM computer_commands WHERE id=$1`, id).Scan(&reconciled); err != nil || !reconciled {
					t.Fatalf("Guest acknowledgement not reconciled: %v %v", reconciled, err)
				}
			} else if err := reconcile(); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("uncertain scope reconciled: %v", err)
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			log.ObservedSeq++
			err = appendCommandLog(t.Context(), tx, worker, log)
			tx.Rollback(t.Context())
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("terminal output admitted: %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
			if err = execute(request); err != nil {
				t.Fatalf("historical replay: %v", err)
			}
			changed := request
			changed.Outcome = "computer_command_launch_failed"
			changed.ExitCode = nil
			if err = execute(changed); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("conflicting replay: %v", err)
			}
		})
	}
}
