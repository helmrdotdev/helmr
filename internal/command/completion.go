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
type OutputBoundary struct {
	ThroughSequence  int64
	Complete, Gapped bool
}
type CompletionReport struct {
	Stdout, Stderr   OutputBoundary
	OutputFenced     bool
	EnvironmentID    uuid.UUID
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
	stdout, stderr                 OutputBoundary
	outputFenced                   bool
	environment, command, instance pgtype.UUID
	status, reason, failure        string
	exit                           pgtype.Int4
	detail                         []byte
	exited, releaseSafe            bool
}

// Validate reports an ErrInvalidCompletion unless the report describes one
// unambiguous result.
func (r CompletionReport) Validate() error {
	_, err := r.parse()
	return err
}

func (r CompletionReport) parse() (completion, error) {
	var p completion
	if r.Stdout.ThroughSequence <= 0 || r.Stderr.ThroughSequence <= 0 {
		return p, completionError("command requires final output boundaries")
	}
	p.stdout, p.stderr, p.outputFenced = r.Stdout, r.Stderr, r.OutputFenced
	if !validID(r.EnvironmentID) || !validID(r.CommandID) || !validID(r.InstanceID) || r.WriterGeneration <= 0 {
		return p, completionError("canonical command, environment and instance IDs and a positive writer generation are required")
	}
	p.environment, p.command, p.instance = pgvalue.UUID(r.EnvironmentID), pgvalue.UUID(r.CommandID), pgvalue.UUID(r.InstanceID)
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
// Environments and Instances carry.
func validID(id uuid.UUID) bool {
	_, err := ids.Parse(id.String())
	return err == nil
}

// Complete records one Command result independently of diagnostic admission. It
// never saves, closes or releases the Computer, and an uncertain process
// scope stays attached to its Instance. Replaying the recorded result is
// accepted even after the Instance's writer expired or was reclaimed.
func Complete(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, report CompletionReport) error {
	return settle(ctx, txb, worker, report, false)
}

// Reconcile records that the worker host released the process scope of a
// terminal Command whose result and settled or fenced output permit release.
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
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{EnvironmentID: p.environment, CommandID: p.command})
	if err != nil {
		return err
	}
	if err = lockHost(ctx, tx, worker); err != nil {
		return err
	}
	l, err := lockCommandLease(ctx, tx, target, uuid.UUID(p.command.Bytes))
	if err != nil {
		return err
	}
	if !l.matches(worker, uuid.UUID(p.instance.Bytes), writerGeneration) {
		return pgx.ErrNoRows
	}
	c, err := lockCommand(ctx, tx, l.EnvironmentID, l.ComputerID, uuid.UUID(p.command.Bytes))
	if err != nil {
		return err
	}
	if !c.ComputerLeaseEpoch.Valid || c.ComputerLeaseEpoch.Int64 != l.Epoch {
		return pgx.ErrNoRows
	}
	if reconcile && !c.TerminalAt.Valid {
		return pgx.ErrNoRows
	}
	if c.TerminalAt.Valid {
		if !c.StdoutFinalThrough.Valid || c.StdoutFinalThrough.Int64 != p.stdout.ThroughSequence || c.StdoutFinalComplete.Bool != p.stdout.Complete || c.StdoutFinalGapped.Bool != p.stdout.Gapped || !c.StderrFinalThrough.Valid || c.StderrFinalThrough.Int64 != p.stderr.ThroughSequence || c.StderrFinalComplete.Bool != p.stderr.Complete || c.StderrFinalGapped.Bool != p.stderr.Gapped {
			return pgx.ErrNoRows
		}
		if c.ResultPrunedAt.Valid || c.Status != p.status || c.ExitCode != p.exit || c.TerminalReasonCode.String != p.reason || c.FailureReason.String != p.failure {
			return pgx.ErrNoRows
		}
		var same bool
		if err = tx.QueryRow(ctx, `SELECT $1::jsonb IS NOT DISTINCT FROM $2::jsonb`, c.Error, p.detail).Scan(&same); err != nil {
			return err
		}
		if !same {
			return pgx.ErrNoRows
		}
		if p.outputFenced && !c.OutputFenced {
			if err = producerStillAuthorized(ctx, tx, worker, l, false); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE computer_commands SET output_fenced=true WHERE environment_id=$1 AND id=$2`, l.EnvironmentID, p.command); err != nil {
				return err
			}
			if err = producerStillAuthorized(ctx, tx, worker, l, false); err != nil {
				return err
			}
			c.OutputFenced = true
		}
		if !reconcile || c.ProcessReconciledAt.Valid {
			return nil
		}
		if !c.OutputFenced && !commandOutputSettled(c) {
			return pgx.ErrNoRows
		}
	}
	if !c.TerminalAt.Valid && p.status == "cancelled" && (c.Status != "stopping" || !c.CancelRequestedAt.Valid) {
		return pgx.ErrNoRows
	}
	if !c.TerminalAt.Valid && c.Status != "running" && c.Status != "stopping" {
		return pgx.ErrNoRows
	}
	if err = producerStillAuthorized(ctx, tx, worker, l, false); err != nil {
		return err
	}
	if !c.TerminalAt.Valid {
		if c.StdoutAcceptedThrough > p.stdout.ThroughSequence || c.StderrAcceptedThrough > p.stderr.ThroughSequence || (c.StdoutEnded && (c.StdoutAcceptedThrough != p.stdout.ThroughSequence || c.StdoutEndComplete != p.stdout.Complete)) || (c.StderrEnded && (c.StderrAcceptedThrough != p.stderr.ThroughSequence || c.StderrEndComplete != p.stderr.Complete)) || (c.StdoutGapped && !p.stdout.Gapped) || (c.StderrGapped && !p.stderr.Gapped) {
			return pgx.ErrNoRows
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_commands SET status=$3,exit_code=$4,failure_reason=NULLIF($5,''),error=$6,
 process_exited_at=CASE WHEN $7 THEN clock_timestamp() END,terminal_at=clock_timestamp(),terminal_reason_code=$8,
 stdout_final_through=$9,stdout_final_complete=$10,stdout_final_gapped=$11,stderr_final_through=$12,stderr_final_complete=$13,stderr_final_gapped=$14,output_fenced=$15,
 result_expires_at=clock_timestamp()+interval '30 days',revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2`, l.EnvironmentID, p.command, p.status, p.exit, p.failure, p.detail, p.exited, p.reason, p.stdout.ThroughSequence, p.stdout.Complete, p.stdout.Gapped, p.stderr.ThroughSequence, p.stderr.Complete, p.stderr.Gapped, p.outputFenced); err != nil {
			return err
		}
	}
	if reconcile {
		if _, err = tx.Exec(ctx, `UPDATE computer_commands SET process_reconciled_at=clock_timestamp(),revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2 AND process_reconciled_at IS NULL`, l.EnvironmentID, p.command); err != nil {
			return err
		}
	}
	return producerStillAuthorized(ctx, tx, worker, l, false)
}

// release is the completion a worker host replays to release the process
// scope of a terminal Command it no longer runs, or nil when the scope needs
// recovery evidence instead.
func release(command db.ComputerCommand, environment uuid.UUID, lease Lease) (*CompletionReport, error) {
	if (!command.OutputFenced && !commandOutputSettled(command)) || !command.TerminalAt.Valid || command.ProcessReconciledAt.Valid || command.Status == "lost" || command.FailureReason.String == "scope_termination_failed" {
		return nil, nil
	}
	switch command.TerminalReasonCode.String {
	case "computer_command_cancelled", "computer_command_completed", "computer_command_timed_out", "computer_command_signaled", "computer_command_failed", "computer_command_secret_delivery_failed", "computer_command_launch_failed", "computer_command_output_capture_failed":
	default:
		return nil, nil
	}
	// A null identity reads as the nil UUID, which Validate rejects.
	report := CompletionReport{Stdout: OutputBoundary{command.StdoutFinalThrough.Int64, command.StdoutFinalComplete.Bool, command.StdoutFinalGapped.Bool}, Stderr: OutputBoundary{command.StderrFinalThrough.Int64, command.StderrFinalComplete.Bool, command.StderrFinalGapped.Bool}, OutputFenced: command.OutputFenced, EnvironmentID: environment, CommandID: uuid.UUID(command.ID.Bytes), InstanceID: lease.InstanceID, WriterGeneration: lease.Epoch, Outcome: command.TerminalReasonCode.String, Error: command.Error}
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

func commandOutputSettled(c db.ComputerCommand) bool {
	return c.StdoutFinalThrough.Valid && c.StderrFinalThrough.Valid && c.StdoutEnded && c.StderrEnded && c.StdoutAcceptedThrough == c.StdoutFinalThrough.Int64 && c.StderrAcceptedThrough == c.StderrFinalThrough.Int64
}
