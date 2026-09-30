package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type commandCompletion struct {
	org, command, instance  pgtype.UUID
	status, reason, failure string
	exit                    pgtype.Int4
	detail                  []byte
	exited, releaseSafe     bool
}

func parseCommandCompletion(r workerapi.ComputerCommandCompleteRequest) (commandCompletion, error) {
	var p commandCompletion
	org, e1 := ids.Parse(r.OrgID)
	command, e2 := ids.Parse(r.CommandID)
	instance, e3 := ids.Parse(r.ComputerInstanceID)
	if e1 != nil || e2 != nil || e3 != nil || r.WriterGeneration <= 0 {
		return p, errors.New("canonical command, organization and instance IDs and a positive writer generation are required")
	}
	p.org, p.command, p.instance = pgvalue.UUID(org), pgvalue.UUID(command), pgvalue.UUID(instance)
	p.reason = r.Outcome
	p.releaseSafe = true
	if r.Outcome == "exited" {
		if r.ExitCode == nil || len(r.Error) != 0 {
			return p, errors.New("exited command requires exit_code and no error")
		}
		p.status = "exited"
		p.reason = "computer_command_completed"
		p.exit = pgtype.Int4{Int32: *r.ExitCode, Valid: true}
		p.exited = true
		return p, nil
	}
	if r.ExitCode != nil {
		return p, errors.New("failed command must not include exit_code")
	}
	p.status, p.failure = "failed", "guest_failure"
	switch r.Outcome {
	case "computer_command_cancelled":
		p.status, p.failure, p.exited = "cancelled", "", true
	case "computer_command_timed_out":
		p.status, p.failure, p.exited = "timed_out", "", true
	case "computer_command_signaled":
		p.exited = true
	case "computer_command_scope_termination_failed":
		p.failure, p.releaseSafe = "scope_termination_failed", false
	case "computer_command_result_uncertain":
		p.status, p.releaseSafe = "lost", false
	case "computer_command_failed", "computer_command_secret_delivery_failed", "computer_command_launch_failed", "computer_command_output_capture_failed":
	default:
		return p, errors.New("unsupported command outcome")
	}
	detail := map[string]json.RawMessage{}
	if len(r.Error) > 0 {
		if err := json.Unmarshal(r.Error, &detail); err != nil || detail == nil {
			return p, errors.New("command error must be a JSON object")
		}
	}
	if len(detail) == 0 {
		detail["code"], _ = json.Marshal(r.Outcome)
	}
	p.detail, _ = json.Marshal(detail)
	return p, nil
}

// Completion records one member's result after output acknowledgement. It never
// saves, closes, or releases the Computer. Uncertain process scopes stay attached.
func completeCommand(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, r workerapi.ComputerCommandCompleteRequest) error {
	return applyCommandCompletion(ctx, tx, worker, r, false)
}

func reconcileCommand(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, r workerapi.ComputerCommandCompleteRequest) error {
	return applyCommandCompletion(ctx, tx, worker, r, true)
}

func applyCommandCompletion(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, r workerapi.ComputerCommandCompleteRequest, reconcile bool) error {
	p, err := parseCommandCompletion(r)
	if err != nil {
		return err
	}
	if reconcile && !p.releaseSafe {
		return pgx.ErrNoRows
	}
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: p.org, CommandID: p.command})
	if err != nil {
		return err
	}
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(worker.GroupID))
	if err != nil {
		return err
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: pgvalue.UUID(worker.HostID), WorkerGroupID: pgvalue.UUID(worker.GroupID)})
	if err != nil {
		return err
	}
	if err = worker.CheckLockedClaims(host, group); err != nil {
		return err
	}
	computer, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return err
	}
	i, err := q.LockComputerCommandInstance(ctx, db.LockComputerCommandInstanceParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: p.command})
	if err != nil {
		return err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: p.command})
	if err != nil {
		return err
	}
	if i.ID != p.instance || i.WorkerHostID != pgvalue.UUID(worker.HostID) || i.WorkerGroupID != pgvalue.UUID(worker.GroupID) || i.WorkerEpoch != worker.Epoch || i.WriterGeneration != r.WriterGeneration || command.ComputerInstanceID != i.ID || !command.WriterGeneration.Valid || command.WriterGeneration.Int64 != i.WriterGeneration {
		return pgx.ErrNoRows
	}
	if reconcile && !command.TerminalAt.Valid {
		return pgx.ErrNoRows
	}
	if command.TerminalAt.Valid {
		// Historical replay grants no new mutation, even after writer expiry/reclaim.
		if command.ResultPrunedAt.Valid || command.Status != p.status || command.ExitCode != p.exit || command.TerminalReasonCode.String != p.reason || command.FailureReason.String != p.failure {
			return pgx.ErrNoRows
		}
		var same bool
		if err = tx.QueryRow(ctx, `SELECT $1::jsonb IS NOT DISTINCT FROM $2::jsonb`, command.Error, p.detail).Scan(&same); err != nil {
			return err
		}
		if !same {
			return pgx.ErrNoRows
		}
		if !reconcile || command.ProcessReconciledAt.Valid {
			return nil
		}
	}
	if !command.TerminalAt.Valid && p.status == "cancelled" && (command.Status != "stopping" || !command.CancelRequestedAt.Valid) {
		return pgx.ErrNoRows
	}
	if !command.TerminalAt.Valid && command.Status != "running" && command.Status != "stopping" {
		return pgx.ErrNoRows
	}
	if i.ReclaimedAt.Valid || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.WriterGeneration != computer.WriterGeneration || (i.AdmissionState != "open" && i.AdmissionState != "draining") {
		return pgx.ErrNoRows
	}
	if !command.TerminalAt.Valid {
		now, err := q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			return err
		}
		exited := pgtype.Timestamptz{}
		if p.exited {
			exited = now
		}
		_, err = q.CompleteComputerCommand(ctx, db.CompleteComputerCommandParams{CommandID: p.command, ComputerInstanceID: i.ID, WriterGeneration: command.WriterGeneration, Status: p.status, ExitCode: p.exit, FailureReason: pgvalue.Text(p.failure), Error: p.detail, ProcessExitedAt: exited, ReasonCode: pgvalue.Text(p.reason)})
		if err != nil {
			return err
		}
	}
	if reconcile {
		if _, err = q.ReconcileComputerCommand(ctx, db.ReconcileComputerCommandParams{CommandID: p.command, ComputerInstanceID: i.ID, WriterGeneration: command.WriterGeneration}); err != nil {
			return err
		}
	}
	if _, err := q.TouchRunComputerActivity(ctx, db.TouchRunComputerActivityParams{
		ID: computer.ID, EnvironmentID: computer.EnvironmentID,
		OrgID: target.OrgID, ProjectID: target.ProjectID, WriterGeneration: i.WriterGeneration,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{WorkerHostID: i.WorkerHostID, WorkerGroupID: i.WorkerGroupID, WorkerEpoch: worker.Epoch, ExpiresAt: i.WriterExpiresAt})
	if err != nil {
		return err
	}
	if !authorized.Valid || !authorized.Bool {
		return pgx.ErrNoRows
	}
	return nil
}

func commandRelease(command db.ComputerCommand, org string, fingerprint string) (*workerapi.ComputerCommandRelease, error) {
	if !command.TerminalAt.Valid || command.ProcessReconciledAt.Valid || command.Status == "lost" || command.FailureReason.String == "scope_termination_failed" {
		return nil, nil
	}
	switch command.TerminalReasonCode.String {
	case "computer_command_cancelled", "computer_command_completed", "computer_command_timed_out", "computer_command_signaled", "computer_command_failed", "computer_command_secret_delivery_failed", "computer_command_launch_failed", "computer_command_output_capture_failed":
	default:
		return nil, nil
	}
	completion := workerapi.ComputerCommandCompleteRequest{OrgID: org, CommandID: pgvalue.UUIDString(command.ID), ComputerInstanceID: pgvalue.UUIDString(command.ComputerInstanceID), WriterGeneration: command.WriterGeneration.Int64, Outcome: command.TerminalReasonCode.String, Error: command.Error}
	if command.Status == "exited" {
		completion.Outcome = "exited"
		code := command.ExitCode.Int32
		completion.ExitCode = &code
	}
	if _, err := parseCommandCompletion(completion); err != nil {
		return nil, err
	}
	return &workerapi.ComputerCommandRelease{ComputerID: pgvalue.UUIDString(command.ComputerID), RequestFingerprint: fingerprint, Completion: completion}, nil
}
