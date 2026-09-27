package run

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/retry"
)

type lostTaskRetry struct {
	entered bool
	at      time.Time
}

// A retry preserves the logical invocation; admission waits for the previous process scope to be reconciled.
func (g OwnedFinalization) taskLossRetry(ctx context.Context, r cancellationRun, at time.Time) (*lostTaskRetry, error) {
	if r.actorID.Valid || runStatusTerminal(r.status) || r.status == db.RunStatusCancelRequested {
		return nil, nil
	}
	q := db.New(g.tx)
	current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: pgvalue.UUID(r.environmentID), ID: pgvalue.UUID(r.id)})
	if err != nil {
		return nil, err
	}
	// Ancestors suspended on a child have no active interval. Charge an active
	// execution exactly through the evidenced loss time before checking its budget.
	if current.ActiveStartedAt.Valid {
		current, err = q.StopLostRunActiveInterval(ctx, db.StopLostRunActiveIntervalParams{LossAt: pgvalue.Timestamptz(at), RunID: current.ID, ComputerID: current.ComputerID, ExpectedRevision: current.Revision, AttemptNumber: current.CurrentAttemptNumber, RunLeaseID: current.CurrentRunLeaseID})
		if err != nil {
			return nil, err
		}
	}
	var revoked bool
	if err := g.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_resolutions r JOIN secrets s ON s.id=r.secret_id WHERE r.run_id=$1 AND r.attempt_number=$2 AND (s.status<>'active' OR s.revocation_generation<>r.revocation_generation))`, current.ID, current.CurrentAttemptNumber).Scan(&revoked); err != nil {
		return nil, err
	}
	if revoked {
		return nil, nil
	}
	if current.ActiveElapsedMs >= current.MaxActiveDurationMs {
		return nil, nil
	}
	var entered bool
	if err := g.tx.QueryRow(ctx, `SELECT entrypoint_entered_at IS NOT NULL OR EXISTS(SELECT 1 FROM run_leases l WHERE l.run_id=a.run_id AND l.attempt_number=a.number AND l.started_at IS NOT NULL) FROM run_attempts a WHERE run_id=$1 AND number=$2`, current.ID, current.CurrentAttemptNumber).Scan(&entered); err != nil {
		return nil, err
	}
	delay := time.Duration(0)
	if entered {
		policy, err := retry.Parse(current.RetryPolicy)
		if err != nil {
			return nil, nil
		} // Invalid pinned policy cannot authorize reexecution.
		var eligible bool
		delay, eligible, err = retry.Delay(policy, current.CurrentAttemptNumber, nil)
		if err != nil || !eligible {
			return nil, err
		}
	}
	// A pending child whose Attempt never entered keeps its existing budget.
	readyAt := at.Add(delay)
	if !entered && current.RetryAt.Valid && current.RetryAt.Time.After(readyAt) {
		readyAt = current.RetryAt.Time
	}
	return &lostTaskRetry{entered: entered, at: readyAt}, nil
}

func (g OwnedFinalization) retryLostTaskTree(ctx context.Context, loss executionLeaseLoss, message string) (bool, error) {
	root := g.descendants[0]
	plan, err := g.taskLossRetry(ctx, root, loss.at)
	if err != nil || plan == nil {
		return false, err
	}
	term := termination{reasonCode: loss.reason, errorCode: loss.reason, errorMessage: message,
		runStatus: db.RunStatusSystemFailed, runLeaseStatus: loss.state, attemptOutcome: "failed",
		waitCondition: db.WaitStatusFailed, waitSuspension: db.RunWaitStatusFailed,
		eventKind: "run.system_failed", eventMessage: message}
	if err := g.scheduleLostTaskRetry(ctx, root, plan, term); err != nil {
		return false, err
	}
	return true, nil
}

func (g OwnedFinalization) scheduleLostTaskRetry(ctx context.Context, r cancellationRun, p *lostTaskRetry, term termination) error {
	payload, err := leaseLossError(term.errorCode, term.errorMessage, true)
	if err != nil {
		return err
	}
	next := r.currentAttemptNumber
	if p.entered {
		if err := retireRunAttempt(ctx, g.tx, r, term, payload); err != nil {
			return err
		}
		next++
	} else if err := retireRunExecution(ctx, g.tx, r, term, payload); err != nil {
		return err
	}
	// The base records the durable frontier; a live shared Instance retains its disk.
	if p.entered {
		if _, err = g.tx.Exec(ctx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id)
SELECT r.id,$2,'task',r.computer_id,c.head_disk_version_id FROM runs r JOIN computers c ON c.id=r.computer_id WHERE r.id=$1`, r.id, next); err != nil {
			return err
		}
		if _, err = g.tx.Exec(ctx, `INSERT INTO secret_resolutions(id,computer_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation)
SELECT gen_random_uuid(),computer_id,run_id,$2,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation FROM secret_resolutions WHERE run_id=$1 AND attempt_number=$3`, r.id, next, r.currentAttemptNumber); err != nil {
			return err
		}
	}
	result, err := g.tx.Exec(ctx, `UPDATE runs r SET status='retry_delayed',current_attempt_number=$2,current_run_lease_id=NULL,retry_at=$3,active_started_at=NULL,base_computer_disk_version_id=a.base_computer_disk_version_id,revision=r.revision+1,updated_at=now()
FROM run_attempts a WHERE r.id=$1 AND r.revision=$4 AND a.run_id=r.id AND a.number=$2 AND a.terminal_at IS NULL`, r.id, next, p.at, r.revision)
	if err != nil || result.RowsAffected() != 1 {
		return cancellationAuthority("schedule lost Task retry", err)
	}
	return nil
}
