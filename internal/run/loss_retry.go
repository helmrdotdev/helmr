package run

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/retry"
)

type lostTaskRetry struct {
	entered bool
	at      time.Time
}

// A retry preserves the logical invocation, not the old process or its private disk.
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
		current, err = q.StopLostRunActiveInterval(ctx, db.StopLostRunActiveIntervalParams{LossAt: pgvalue.Timestamptz(at), RunID: current.ID, WorkspaceID: current.WorkspaceID, ExpectedRevision: current.Revision, AttemptNumber: current.CurrentAttemptNumber, RunLeaseID: current.CurrentRunLeaseID})
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
	plans := map[uuid.UUID]*lostTaskRetry{root.id: plan}
	terminal := map[uuid.UUID]bool{}
	for _, child := range g.descendants[1:] {
		if runStatusTerminal(child.status) {
			continue
		}
		if child.parentRunID.Valid && terminal[uuid.UUID(child.parentRunID.Bytes)] {
			terminal[child.id] = true
			continue
		}
		// A separate Computer remains owned and running across the parent's retry.
		if child.workspaceID != root.workspaceID {
			continue
		}
		p, err := g.taskLossRetry(ctx, child, loss.at)
		if err != nil {
			return false, err
		}
		if p == nil {
			terminal[child.id] = true
		} else {
			plans[child.id] = p
		}
	}
	term := termination{reasonCode: loss.reason, errorCode: loss.reason, errorMessage: message, runStatus: db.RunStatusSystemFailed, runLeaseStatus: loss.state, attemptOutcome: "failed", waitCondition: db.WaitStatusFailed, waitSuspension: db.RunWaitStatusFailed, eventKind: "run.system_failed", eventMessage: message}
	for i := len(g.descendants) - 1; i >= 0; i-- {
		r := g.descendants[i]
		if runStatusTerminal(r.status) {
			continue
		}
		if terminal[r.id] {
			if r.workspaceID == root.workspaceID {
				if err := terminateLockedRun(ctx, g.tx, r, term); err != nil {
					return false, err
				}
			} else if err := cancelLockedRun(ctx, g.tx, r); err != nil {
				return false, err
			}
		} else if p := plans[r.id]; p != nil {
			if err := g.scheduleLostTaskRetry(ctx, r, p, term); err != nil {
				return false, err
			}
		}
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
	// Pin the committed source while waiting for physical cleanup. A shared child
	// will replace this unentered source with its new parent's handoff checkpoint.
	if p.entered {
		if _, err = g.tx.Exec(ctx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,workspace_id,base_workspace_version_id)
SELECT r.id,$2,'task',r.workspace_id,c.head_version_id FROM runs r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, r.id, next); err != nil {
			return err
		}
		if _, err = g.tx.Exec(ctx, `INSERT INTO secret_resolutions(id,workspace_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation)
SELECT gen_random_uuid(),workspace_id,run_id,$2,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation FROM secret_resolutions WHERE run_id=$1 AND attempt_number=$3`, r.id, next, r.currentAttemptNumber); err != nil {
			return err
		}
	}
	result, err := g.tx.Exec(ctx, `UPDATE runs r SET status='retry_delayed',current_attempt_number=$2,current_run_lease_id=NULL,retry_at=$3,active_started_at=NULL,base_workspace_version_id=a.base_workspace_version_id,revision=r.revision+1,updated_at=now()
FROM run_attempts a WHERE r.id=$1 AND r.revision=$4 AND a.run_id=r.id AND a.number=$2 AND a.terminal_at IS NULL`, r.id, next, p.at, r.revision)
	if err != nil || result.RowsAffected() != 1 {
		return cancellationAuthority("schedule lost Task retry", err)
	}
	return nil
}
