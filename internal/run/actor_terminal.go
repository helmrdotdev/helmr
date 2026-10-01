package run

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// An Actor completion terminates the locked finalizing execution of an
// Actor's Run in its caller's transaction: CompleteActorAttempt records the
// lease and attempt outcome, FinishActorRun the Run's, and
// AppendActorTerminalEvent the Run's terminal event. The Session owner
// settles the Session between them.

// ActorAttemptTerminal is the terminal outcome of an Actor execution's lease
// and attempt. Status is the Run's terminal status: succeeded, cancelled
// (an interruption) or failed. Reason and Error describe a failed Run.
// InputSequence is the Session's committed input sequence at termination.
type ActorAttemptTerminal struct {
	Status        db.RunStatus
	Reason        string
	Error         []byte
	InputSequence int64
	Fingerprint   string
	CompletedAt   pgtype.Timestamptz
}

// CompleteActorAttempt completes the execution's lease and then its attempt:
// a succeeded Run completes them as completed, a cancelled Run cancels them
// as session_interrupted, and a failed Run fails them with its reason and
// error. A lease or attempt that is no longer the execution's is
// pgx.ErrNoRows.
func CompleteActorAttempt(ctx context.Context, tx pgx.Tx, execution Execution, terminal ActorAttemptTerminal) error {
	q := db.New(tx)
	runRow, attempt, lease, computerRow := execution.run, execution.attempt, execution.lease, execution.Computer()
	leaseStatus := db.RunLeaseStatusFailed
	outcome := pgvalue.Text("failed")
	reason := "actor_failed"
	var terminalError []byte
	if terminal.Status == db.RunStatusSucceeded {
		leaseStatus = db.RunLeaseStatusCompleted
		outcome = pgvalue.Text("succeeded")
		reason = "completed"
	} else if terminal.Status == db.RunStatusCancelled {
		leaseStatus = db.RunLeaseStatusCancelled
		outcome = pgvalue.Text("cancelled")
		reason = "session_interrupted"
	} else {
		reason = terminal.Reason
		terminalError = terminal.Error
	}
	if _, err := q.CompleteTaskRunLease(ctx, db.CompleteTaskRunLeaseParams{
		Status: leaseStatus, CompletedAt: terminal.CompletedAt, ReasonCode: pgvalue.Text(reason), Error: terminalError,
		TerminalRequestFingerprint: pgvalue.Text(terminal.Fingerprint), ID: lease.ID,
		RunID: runRow.ID, ComputerID: computerRow.ID, AttemptNumber: attempt.Number,
		LeaseSequence: lease.LeaseSequence,
	}); err != nil {
		return err
	}
	if _, err := q.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalSessionInputSequence: pgtype.Int8{Int64: terminal.InputSequence, Valid: true}, TerminalOutcome: outcome,
		ReasonCode: pgvalue.Text(reason), Error: terminalError, CompletedAt: terminal.CompletedAt,
		RunID: runRow.ID, Number: attempt.Number, ComputerID: computerRow.ID,
	}); err != nil {
		return err
	}
	return nil
}

// FinishActorRun records the Actor Run's terminal status and failure. A Run
// that is no longer the execution's is pgx.ErrNoRows.
func FinishActorRun(ctx context.Context, tx pgx.Tx, execution Execution, status db.RunStatus, failure json.RawMessage, completedAt pgtype.Timestamptz) error {
	runRow, attempt, lease, computerRow := execution.run, execution.attempt, execution.lease, execution.Computer()
	_, err := db.New(tx).FinishActorRun(ctx, db.FinishActorRunParams{
		Status: status, Failure: failure, CompletedAt: completedAt,
		ID: runRow.ID, ComputerID: computerRow.ID, SessionID: execution.session.ID,
		AttemptNumber: attempt.Number, RunLeaseID: lease.ID,
	})
	return err
}

// AppendActorTerminalEvent appends the Actor Run's terminal event for its
// status, run.completed, run.cancelled or run.failed, with the reason when
// there is one.
func AppendActorTerminalEvent(ctx context.Context, tx pgx.Tx, execution Execution, status db.RunStatus, reason string) error {
	kind := "run.completed"
	if status == db.RunStatusFailed {
		kind = "run.failed"
	} else if status == db.RunStatusCancelled {
		kind = "run.cancelled"
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason})
	if err != nil {
		return err
	}
	if _, err := db.New(tx).AppendRunEvent(ctx, db.AppendRunEventParams{OrgID: execution.run.OrgID, RunID: execution.run.ID, Kind: kind, Payload: payload}); err != nil {
		return fmt.Errorf("append actor terminal event: %w", err)
	}
	return nil
}
