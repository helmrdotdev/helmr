package command

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CompletionReport is a worker host's report of one Command result on the
// Instance incarnation and writer generation it acts for. Outcome is
// "exited" with an ExitCode, or a computer_command_* reason with an optional
// JSON object Error.
type CompletionReport struct {
	OrgID            uuid.UUID
	CommandID        uuid.UUID
	InstanceID       uuid.UUID
	WriterGeneration int64
	Outcome          string
	ExitCode         *int32
	Error            []byte
}

// completionError is an ErrInvalidCompletion that keeps its specific message.
type completionError string

func (e completionError) Error() string        { return string(e) }
func (e completionError) Is(target error) bool { return target == ErrInvalidCompletion }

type completion struct {
	org, command, instance  pgtype.UUID
	status, reason, failure string
	exit                    pgtype.Int4
	detail                  []byte
	exited, releaseSafe     bool
}

// Validate reports an ErrInvalidCompletion unless the report describes one
// unambiguous result.
func (r CompletionReport) Validate() error {
	_, err := r.parse()
	return err
}

func (r CompletionReport) parse() (completion, error) {
	var p completion
	if !validID(r.OrgID) || !validID(r.CommandID) || !validID(r.InstanceID) || r.WriterGeneration <= 0 {
		return p, completionError("canonical command, organization and instance IDs and a positive writer generation are required")
	}
	p.org, p.command, p.instance = pgvalue.UUID(r.OrgID), pgvalue.UUID(r.CommandID), pgvalue.UUID(r.InstanceID)
	p.reason = r.Outcome
	p.releaseSafe = true
	if r.Outcome == "exited" {
		if r.ExitCode == nil || len(r.Error) != 0 {
			return p, completionError("exited command requires exit_code and no error")
		}
		p.status = "exited"
		p.reason = "computer_command_completed"
		p.exit = pgtype.Int4{Int32: *r.ExitCode, Valid: true}
		p.exited = true
		return p, nil
	}
	if r.ExitCode != nil {
		return p, completionError("failed command must not include exit_code")
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
		return p, completionError("unsupported command outcome")
	}
	detail := map[string]json.RawMessage{}
	if len(r.Error) > 0 {
		if err := json.Unmarshal(r.Error, &detail); err != nil || detail == nil {
			return p, completionError("command error must be a JSON object")
		}
	}
	if len(detail) == 0 {
		detail["code"], _ = json.Marshal(r.Outcome)
	}
	p.detail, _ = json.Marshal(detail)
	return p, nil
}

// validID reports whether id is a UUIDv7, the only identity form Commands,
// Organizations and Instances carry.
func validID(id uuid.UUID) bool {
	_, err := ids.Parse(id.String())
	return err == nil
}

// Complete records one Command result after its output was acknowledged. It
// never saves, closes or releases the Computer, and an uncertain process
// scope stays attached to its Instance. Replaying the recorded result is
// accepted even after the Instance's writer expired or was reclaimed.
func Complete(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, report CompletionReport) error {
	return settle(ctx, txb, worker, report, false)
}

// Reconcile records that the worker host released the process scope of a
// terminal Command whose result is safe to release, completing it first when
// it is not yet terminal.
func Reconcile(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, report CompletionReport) error {
	return settle(ctx, txb, worker, report, true)
}

func settle(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, report CompletionReport, reconcile bool) error {
	p, err := report.parse()
	if err != nil {
		return err
	}
	return changed(db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		return applyCompletion(ctx, tx, worker, p, report.WriterGeneration, reconcile)
	}))
}

func applyCompletion(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, p completion, writerGeneration int64, reconcile bool) error {
	if reconcile && !p.releaseSafe {
		return pgx.ErrNoRows
	}
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: p.org, CommandID: p.command})
	if err != nil {
		return err
	}
	if _, err = workergroup.LockHost(ctx, q, worker); err != nil {
		return err
	}
	locked, err := lockCommandInstance(ctx, tx, target, p.command)
	if err != nil {
		return err
	}
	if !locked.Bound() {
		return pgx.ErrNoRows
	}
	i := locked.Instance()
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: p.command})
	if err != nil {
		return err
	}
	if !locked.On(host(worker), pgvalue.MustUUIDValue(p.instance), writerGeneration) || command.ComputerInstanceID != i.ID || !command.WriterGeneration.Valid || command.WriterGeneration.Int64 != i.WriterGeneration {
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
	// Liveness checks no Computer status: a member may finish on a Computer
	// whose deletion or recovery began.
	if !locked.Serving() || (i.AdmissionState != "open" && i.AdmissionState != "draining") {
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
	if err = locked.TouchActivity(ctx, pgvalue.MustUUIDValue(target.OrgID), pgvalue.MustUUIDValue(target.ProjectID)); err != nil {
		return err
	}
	return producerStillAuthorized(ctx, q, worker, i)
}

// release is the completion a worker host replays to release the process
// scope of a terminal Command it no longer runs, or nil when the scope needs
// recovery evidence instead.
func release(command db.ComputerCommand, org uuid.UUID) (*CompletionReport, error) {
	if !command.TerminalAt.Valid || command.ProcessReconciledAt.Valid || command.Status == "lost" || command.FailureReason.String == "scope_termination_failed" {
		return nil, nil
	}
	switch command.TerminalReasonCode.String {
	case "computer_command_cancelled", "computer_command_completed", "computer_command_timed_out", "computer_command_signaled", "computer_command_failed", "computer_command_secret_delivery_failed", "computer_command_launch_failed", "computer_command_output_capture_failed":
	default:
		return nil, nil
	}
	// A null identity reads as the nil UUID, which Validate rejects.
	report := CompletionReport{OrgID: org, CommandID: uuid.UUID(command.ID.Bytes), InstanceID: uuid.UUID(command.ComputerInstanceID.Bytes), WriterGeneration: command.WriterGeneration.Int64, Outcome: command.TerminalReasonCode.String, Error: command.Error}
	if command.Status == "exited" {
		report.Outcome = "exited"
		code := command.ExitCode.Int32
		report.ExitCode = &code
	}
	if err := report.Validate(); err != nil {
		return nil, err
	}
	return &report, nil
}
