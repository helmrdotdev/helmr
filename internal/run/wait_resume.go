package run

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// acknowledgeWaitResume releases one restored waiter in the caller's
// transaction.
func acknowledgeWaitResume(ctx context.Context, tx pgx.Tx, fence ExecutionFence, waitID, checkpointID pgtype.UUID) (db.RunWait, error) {
	a, err := lockExecution(ctx, tx, fence, executionResume, executionTarget{})
	if err != nil {
		return db.RunWait{}, err
	}
	if a.lease.Status != db.RunLeaseStatusRunning || !a.attempt.EntrypointEnteredAt.Valid || a.lease.FinalizationOperationID.Valid {
		return db.RunWait{}, pgx.ErrNoRows
	}
	wait, err := lockRestoredWait(ctx, tx, a, waitID, checkpointID)
	if err != nil {
		return db.RunWait{}, err
	}
	q := db.New(tx)
	if wait.SuspensionStatus != db.RunWaitStatusResuming {
		return wait, nil
	}
	wait, err = q.AcknowledgeRunWaitResume(ctx, db.AcknowledgeRunWaitResumeParams{WaitID: waitID, EnvironmentID: a.run.EnvironmentID, RunLeaseID: a.lease.ID, LeaseSequence: a.lease.LeaseSequence})
	if err != nil {
		return db.RunWait{}, err
	}
	stopped, err := q.RunWaitSessionStopped(ctx, waitID)
	if err != nil {
		return db.RunWait{}, err
	}
	if stopped && wait.SuspensionStatus == db.RunWaitStatusHot && wait.ConditionStatus == db.WaitStatusPending {
		return q.FailHotRunWait(ctx, db.FailHotRunWaitParams{ID: wait.ID, RunID: wait.RunID, AttemptNumber: wait.AttemptNumber, CurrentRunLeaseID: wait.CurrentRunLeaseID, ExpectedRunRevision: wait.ExpectedRunRevision, ReasonCode: pgvalue.Text("session_stopped"), ConditionError: []byte(`{"code":"session_stopped","retryable":false}`)})
	}
	return wait, nil
}

func lockRestoredWait(ctx context.Context, tx pgx.Tx, a Execution, waitID, checkpointID pgtype.UUID) (db.RunWait, error) {
	q := db.New(tx)
	var err error
	// Checkpoints retain captured membership after the wait clears its suspension
	// pointer, allowing an exact acknowledgement to be retried after a lost reply.
	var locked pgtype.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM run_waits WHERE id=$1 AND run_id=$2 AND attempt_number=$3 FOR UPDATE`, waitID, a.run.ID, a.attempt.Number).Scan(&locked); err != nil {
		return db.RunWait{}, err
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_checkpoint_runs m
 JOIN computer_checkpoints c ON c.id=m.checkpoint_id
 JOIN computer_instances i ON i.id=c.resume_computer_instance_id
 JOIN run_leases l ON l.id=$4 AND l.computer_instance_id=i.id
 JOIN run_waits w ON w.id=m.run_wait_id
 WHERE m.checkpoint_id=$1 AND m.run_wait_id=$2 AND m.run_id=$3
 AND m.attempt_number=l.attempt_number AND c.status='ready' AND c.resume_committed_at IS NOT NULL
 AND i.id=$5 AND i.source_checkpoint_id=c.id AND i.writer_generation=l.writer_generation
 AND i.writer_expires_at>clock_timestamp() AND l.expires_at>clock_timestamp()
 AND w.current_run_lease_id=l.id AND w.suspension_status IN ('resuming','hot','released')
 AND ((w.suspension_status='resuming' AND w.suspend_checkpoint_id=c.id AND w.prior_run_lease_id=m.source_run_lease_id)
 OR (w.suspension_status IN ('hot','released') AND w.suspend_checkpoint_id IS NULL AND w.prior_run_lease_id IS NULL)))`, checkpointID, waitID, a.run.ID, a.lease.ID, a.Instance().ID).Scan(&valid)
	if err != nil {
		return db.RunWait{}, err
	}
	if !valid {
		return db.RunWait{}, pgx.ErrNoRows
	}
	current, err := q.RunWaitTurnCurrent(ctx, waitID)
	if err != nil {
		return db.RunWait{}, err
	}
	if !current {
		stopped, err := q.RunWaitSessionStopped(ctx, waitID)
		if err != nil {
			return db.RunWait{}, err
		}
		if !stopped {
			return db.RunWait{}, pgx.ErrNoRows
		}
	}
	wait, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: waitID, RunID: a.run.ID, AttemptNumber: a.attempt.Number})
	if err != nil {
		return db.RunWait{}, err
	}
	return wait, nil
}

// ClaimRestoredExecution attaches to an already-activated captured Program.
// It never admits a new Program or starts another attempt.
func ClaimRestoredExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (Execution, db.RunWait, error) {
	a, err := lockExecution(ctx, tx, fence, executionResume, executionTarget{})
	if err != nil {
		return Execution{}, db.RunWait{}, err
	}
	if (a.Instance().AdmissionState != "open" && a.Instance().AdmissionState != "draining") || a.lease.Status != db.RunLeaseStatusRunning || !a.attempt.EntrypointEnteredAt.Valid || a.lease.FinalizationOperationID.Valid {
		return Execution{}, db.RunWait{}, pgx.ErrNoRows
	}
	var waitID pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT m.run_wait_id FROM computer_checkpoint_runs m JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=$1 AND m.run_id=$2 AND m.attempt_number=$3 AND w.current_run_lease_id=$4 AND w.suspension_status='resuming'`, a.Instance().SourceCheckpointID, a.run.ID, a.attempt.Number, a.lease.ID).Scan(&waitID)
	if err != nil {
		return Execution{}, db.RunWait{}, err
	}
	wait, err := lockRestoredWait(ctx, tx, a, waitID, a.Instance().SourceCheckpointID)
	if err != nil {
		return Execution{}, db.RunWait{}, err
	}
	if wait.SuspensionStatus != db.RunWaitStatusResuming {
		return Execution{}, db.RunWait{}, pgx.ErrNoRows
	}
	return a, wait, nil
}
