package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/retry"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type ExecutionLeaseRecoveryRequest struct {
	RunID                 uuid.UUID
	WorkspaceID           uuid.UUID
	AttemptNumber         int32
	RunLeaseID            uuid.UUID
	RetryResolutions      []secret.Resolution
	RetrySecretsAvailable bool
}

type executionLeaseLoss struct {
	at     time.Time
	kind   string
	reason string
	state  db.RunLeaseState
}

// RecoverExecutionLeaseLoss applies the state-specific recovery transition after
// LockOwnedFinalization has acquired the canonical Run graph and physical
// authority order. Exact replay or a race won by claim/start/renewal is a
// no-op.
func (g OwnedFinalization) RecoverExecutionLeaseLoss(
	ctx context.Context,
	request ExecutionLeaseRecoveryRequest,
) (bool, error) {
	if g.tx == nil || g.currentRun != request.RunID || len(g.descendants) == 0 ||
		request.RunID == uuid.Nil() || request.WorkspaceID == uuid.Nil() ||
		request.AttemptNumber <= 0 || request.RunLeaseID == uuid.Nil() {
		return false, errors.New("Run execution lease recovery authority is invalid")
	}
	target := g.descendants[0]
	if target.id != request.RunID || target.workspaceID != request.WorkspaceID ||
		target.currentAttemptNumber != request.AttemptNumber ||
		!target.currentRunLeaseID.Valid ||
		uuid.UUID(target.currentRunLeaseID.Bytes) != request.RunLeaseID {
		return false, nil
	}
	q := db.New(g.tx)
	authority, err := q.GetRunExecutionLeaseLossAuthority(
		ctx,
		db.GetRunExecutionLeaseLossAuthorityParams{
			RunID:         pgvalue.UUID(request.RunID),
			WorkspaceID:   pgvalue.UUID(request.WorkspaceID),
			AttemptNumber: request.AttemptNumber,
			RunLeaseID:    pgvalue.UUID(request.RunLeaseID),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, cancellationAuthority("load Run execution lease loss authority", err)
	}
	loss, ok, err := decideExecutionLeaseLoss(authority)
	if err != nil || !ok {
		return false, err
	}
	if authority.RunLeaseState == string(db.RunLeaseStateAssigned) ||
		authority.RunLeaseState == string(db.RunLeaseStateStarting) {
		cleared, err := recoverExecutionPrestartLease(ctx, q, authority, loss)
		if err != nil {
			return false, err
		}
		if loss.kind == "physical_failure" {
			if err := g.recordClearedExecutionPrestart(cleared); err != nil {
				return false, err
			}
			if _, err := g.ChargeRuntimePreparationFailure(ctx); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	leaseState := db.RunLeaseState(authority.RunLeaseState)
	switch leaseState {
	case db.RunLeaseStateRunning, db.RunLeaseStateCheckpointing:
		if _, err := q.StopLostRunActiveInterval(ctx, db.StopLostRunActiveIntervalParams{
			LossAt: pgvalue.Timestamptz(loss.at), RunID: authority.RunID,
			WorkspaceID: authority.WorkspaceID, ExpectedStateVersion: authority.StateVersion,
			AttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return false, cancellationAuthority("stop lost Run active interval", err)
		}
	case db.RunLeaseStateFinalizing:
		// Finalization starts only after the active interval is durably stopped.
		// Preserve its immutable finalization receipt on the terminal Lease.
	default:
		return false, nil
	}
	if loss.kind == "active_deadline" {
		if err := g.failCurrentForLeaseLoss(
			ctx,
			loss,
			"Run maximum active duration was exceeded",
			db.RunStatusExpired,
			"run_expired",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	policy, err := retry.Parse(authority.RetryPolicy)
	if err != nil {
		loss.reason = "retry_policy_invalid"
		loss.state = db.RunLeaseStateLost
		if err := g.failCurrentForLeaseLoss(
			ctx, loss, "Run retry policy was invalid", db.RunStatusSystemFailed, "platform_failure",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	delay, shouldRetry, err := retry.Delay(policy, authority.CurrentAttemptNumber, nil)
	if err != nil {
		return false, cancellationAuthority("apply Run execution retry policy", err)
	}
	if shouldRetry && !request.RetrySecretsAvailable {
		loss.reason = "secret_retry_unavailable"
		loss.state = db.RunLeaseStateLost
		if err := g.failCurrentForLeaseLoss(
			ctx, loss, "Run retry Secret authority was unavailable", db.RunStatusSystemFailed, "platform_failure",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	if !shouldRetry {
		if err := g.failCurrentForLeaseLoss(
			ctx, loss, executionLeaseLossMessage(loss.reason), db.RunStatusSystemFailed, "platform_failure",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	if len(g.descendants) != 1 {
		return false, cancellationAuthority("Run execution retry retained an owned descendant", nil)
	}
	if err := g.retryCurrentAfterLeaseLoss(
		ctx,
		authority,
		loss,
		loss.at.Add(delay),
		request.RetryResolutions,
	); err != nil {
		return false, err
	}
	return true, nil
}

func decideExecutionLeaseLoss(
	authority db.GetRunExecutionLeaseLossAuthorityRow,
) (executionLeaseLoss, bool, error) {
	if !authority.ObservedAt.Valid || !authority.RunLeaseExpiresAt.Valid ||
		!authority.StartDeadlineAt.Valid {
		return executionLeaseLoss{}, false, errors.New("Run execution lease loss timestamps are incomplete")
	}
	candidates := make([]executionLeaseLoss, 0, 9)
	add := func(at pgtype.Timestamptz, kind, reason string, state db.RunLeaseState) {
		if at.Valid {
			candidates = append(candidates, executionLeaseLoss{at: at.Time, kind: kind, reason: reason, state: state})
		}
	}
	add(authority.RunLeaseExpiresAt, "lease", "lease_expired", db.RunLeaseStateExpired)
	switch db.RunLeaseState(authority.RunLeaseState) {
	case db.RunLeaseStateRunning, db.RunLeaseStateCheckpointing:
		if !authority.ActiveStartedAt.Valid || authority.MaxActiveDurationMs < authority.ActiveElapsedMs {
			return executionLeaseLoss{}, false, errors.New("active Run execution Lease budget is invalid")
		}
		hardDeadline := authority.ActiveStartedAt.Time.Add(
			time.Duration(authority.MaxActiveDurationMs-authority.ActiveElapsedMs) * time.Millisecond,
		)
		candidates = append(candidates, executionLeaseLoss{
			at: hardDeadline, kind: "active_deadline", reason: "max_active_duration_exceeded",
			state: db.RunLeaseStateExpired,
		})
	case db.RunLeaseStateAssigned, db.RunLeaseStateStarting:
		add(authority.StartDeadlineAt, "start_deadline", "lease_expired", db.RunLeaseStateExpired)
	case db.RunLeaseStateFinalizing:
		// Finalizing has neither a start nor active deadline. Its renewable Lease
		// expiry and physical authority are the only recovery boundaries.
	default:
		return executionLeaseLoss{}, false, nil
	}
	add(authority.WorkerLostAt, "physical_loss", "worker_lost", db.RunLeaseStateLost)
	add(authority.WorkerTerminationReadyAt, "physical_loss", "worker_lost", db.RunLeaseStateLost)
	if authority.WorkerCurrentEpoch.Valid && authority.WorkerCurrentEpoch.Int64 != authority.WorkerEpoch {
		epochAt := authority.WorkerEpochStartedAt
		if !epochAt.Valid {
			epochAt = authority.WorkerUpdatedAt
		}
		add(epochAt, "physical_loss", "worker_lost", db.RunLeaseStateLost)
	}
	add(authority.RuntimeLostAt, "physical_loss", "worker_lost", db.RunLeaseStateLost)
	add(authority.MountLostAt, "physical_loss", "worker_lost", db.RunLeaseStateLost)
	add(authority.RuntimeFailedAt, "physical_failure", "runtime_failed", db.RunLeaseStateLost)
	add(authority.MountFailedAt, "physical_failure", "runtime_failed", db.RunLeaseStateLost)
	if len(candidates) == 0 {
		return executionLeaseLoss{}, false, nil
	}
	chosen := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.at.Before(chosen.at) ||
			(candidate.at.Equal(chosen.at) && executionLossPriority(candidate.kind) < executionLossPriority(chosen.kind)) {
			chosen = candidate
		}
	}
	if chosen.at.After(authority.ObservedAt.Time) {
		return executionLeaseLoss{}, false, nil
	}
	return chosen, true, nil
}

func executionLossPriority(kind string) int {
	switch kind {
	case "active_deadline":
		return 0
	case "physical_failure":
		return 1
	case "physical_loss":
		return 2
	default:
		return 3
	}
}

func recoverExecutionPrestartLease(
	ctx context.Context,
	q *db.Queries,
	authority db.GetRunExecutionLeaseLossAuthorityRow,
	loss executionLeaseLoss,
) (db.Run, error) {
	errorPayload, err := leaseLossError(loss.reason, executionLeaseLossMessage(loss.reason), true)
	if err != nil {
		return db.Run{}, err
	}
	if err := terminalizeExecutionLeasePhysicalAuthority(ctx, q, authority, loss, errorPayload); err != nil {
		return db.Run{}, err
	}
	cleared, err := q.ClearFreshPrestartRunLease(ctx, db.ClearFreshPrestartRunLeaseParams{
		RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
		ExpectedStateVersion: authority.StateVersion, AttemptNumber: authority.CurrentAttemptNumber,
		RunLeaseID: authority.RunLeaseID,
	})
	if err != nil {
		return db.Run{}, cancellationAuthority("clear Run execution pre-start Lease", err)
	}
	return cleared, nil
}

func (g OwnedFinalization) recordClearedExecutionPrestart(cleared db.Run) error {
	if len(g.descendants) == 0 || !cleared.ID.Valid ||
		uuid.UUID(cleared.ID.Bytes) != g.currentRun ||
		cleared.CurrentRunLeaseID.Valid || cleared.Status != db.RunStatusQueued {
		return cancellationAuthority("cleared Run execution pre-start state is invalid", nil)
	}
	updated := g.descendants[0]
	updated.currentRunLeaseID = cleared.CurrentRunLeaseID
	updated.stateVersion = cleared.StateVersion
	updated.status = cleared.Status
	updated.runtimePreparationCount = cleared.RuntimePreparationCount
	g.descendants[0] = updated
	g.locked[updated.id] = updated
	return nil
}

func (g OwnedFinalization) retryCurrentAfterLeaseLoss(
	ctx context.Context,
	authority db.GetRunExecutionLeaseLossAuthorityRow,
	loss executionLeaseLoss,
	retryAt time.Time,
	resolutions []secret.Resolution,
) error {
	q := db.New(g.tx)
	errorPayload, err := leaseLossError(loss.reason, executionLeaseLossMessage(loss.reason), true)
	if err != nil {
		return err
	}
	if authority.RunStatus == string(db.RunStatusWaiting) {
		if err := terminalizeExecutionRetrySuspensions(ctx, q, authority, loss, errorPayload); err != nil {
			return err
		}
	}
	if err := terminalizeExecutionLeasePhysicalAuthority(ctx, q, authority, loss, errorPayload); err != nil {
		return err
	}
	affected, err := q.TerminalizeRunAttempt(ctx, db.TerminalizeRunAttemptParams{
		Outcome: "failed", ReasonCode: loss.reason, ErrorPayload: errorPayload,
		RunID: authority.RunID, AttemptNumber: authority.CurrentAttemptNumber,
	})
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize lost Run Attempt", err)
	}
	nextAttempt := authority.CurrentAttemptNumber + 1
	switch authority.EntrypointKind {
	case "task":
		if authority.RunStatus == string(db.RunStatusWaiting) {
			if _, err := q.CreateCheckpointFailureRetryAttempt(ctx, db.CreateCheckpointFailureRetryAttemptParams{
				Number: nextAttempt, RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
				PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
			}); err != nil {
				return cancellationAuthority("create lost checkpointing Task retry Attempt", err)
			}
		} else if authority.RunStatus == string(db.RunStatusRunning) {
			if _, err := q.CreateTaskRetryAttempt(ctx, db.CreateTaskRetryAttemptParams{
				Number: nextAttempt, RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
				PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
			}); err != nil {
				return cancellationAuthority("create lost Task retry Attempt", err)
			}
		} else {
			return cancellationAuthority("lost Task retry Run state is unsupported", nil)
		}
	case "actor":
		if !authority.ActorRunGeneration.Valid || !authority.SessionID.Valid {
			return cancellationAuthority("lost Actor retry authority is incomplete", nil)
		}
		if authority.RunStatus == string(db.RunStatusWaiting) {
			if _, err := q.CreateActorCheckpointFailureRetryAttempt(ctx, db.CreateActorCheckpointFailureRetryAttemptParams{
				Number: nextAttempt, ExpectedRunGeneration: authority.ActorRunGeneration.Int64,
				RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
				PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
			}); err != nil {
				return cancellationAuthority("create lost checkpointing Actor retry Attempt", err)
			}
		} else if authority.RunStatus == string(db.RunStatusRunning) {
			if _, err := q.CreateActorRetryAttempt(ctx, db.CreateActorRetryAttemptParams{
				Number: nextAttempt, ExpectedRunGeneration: authority.ActorRunGeneration.Int64,
				RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
				PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
			}); err != nil {
				return cancellationAuthority("create lost Actor retry Attempt", err)
			}
		} else {
			return cancellationAuthority("lost Actor retry Run state is unsupported", nil)
		}
	default:
		return cancellationAuthority("lost Run entrypoint kind is unsupported", nil)
	}
	if err := secret.CreateAttemptResolutions(
		ctx, q, authority.WorkspaceID, authority.RunID, nextAttempt, resolutions,
	); err != nil {
		return cancellationAuthority("record lost Run retry Secret resolutions", err)
	}
	lostAt := pgvalue.Timestamptz(loss.at)
	retryTimestamp := pgvalue.Timestamptz(retryAt)
	if authority.EntrypointKind == "task" {
		if authority.RunStatus == string(db.RunStatusWaiting) {
			if _, err := q.DelayCheckpointFailureRetry(ctx, db.DelayCheckpointFailureRetryParams{
				NextAttemptNumber: nextAttempt, FailedAt: lostAt, RetryAt: retryTimestamp,
				ID: authority.RunID, WorkspaceID: authority.WorkspaceID,
				PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
			}); err != nil {
				return cancellationAuthority("delay lost checkpointing Task retry", err)
			}
		} else if _, err := q.DelayTaskRunRetry(ctx, db.DelayTaskRunRetryParams{
			NextAttemptNumber: nextAttempt, CompletedAt: lostAt, RetryAt: retryTimestamp,
			ID: authority.RunID, WorkspaceID: authority.WorkspaceID,
			PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return cancellationAuthority("delay lost Task retry", err)
		}
	} else {
		if authority.RunStatus == string(db.RunStatusWaiting) {
			if _, err := q.DelayActorCheckpointFailureRetry(ctx, db.DelayActorCheckpointFailureRetryParams{
				NextAttemptNumber: nextAttempt, RetryAt: retryTimestamp, FailedAt: lostAt,
				ID: authority.RunID, WorkspaceID: authority.WorkspaceID, SessionID: authority.SessionID,
				PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
			}); err != nil {
				return cancellationAuthority("delay lost checkpointing Actor retry", err)
			}
		} else if _, err := q.DelayActorRunRetry(ctx, db.DelayActorRunRetryParams{
			NextAttemptNumber: nextAttempt, RetryAt: retryTimestamp, CompletedAt: lostAt,
			ID: authority.RunID, WorkspaceID: authority.WorkspaceID, SessionID: authority.SessionID,
			PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return cancellationAuthority("delay lost Actor retry", err)
		}
	}
	return nil
}

func terminalizeExecutionRetrySuspensions(
	ctx context.Context,
	q *db.Queries,
	authority db.GetRunExecutionLeaseLossAuthorityRow,
	loss executionLeaseLoss,
	errorPayload json.RawMessage,
) error {
	if err := q.TerminalizeRunSuspensions(ctx, db.TerminalizeRunSuspensionsParams{
		ConditionState: "failed", ErrorPayload: errorPayload, ReasonCode: loss.reason,
		SuspensionState: "failed", RunID: authority.RunID,
	}); err != nil {
		return cancellationAuthority("terminalize lost checkpoint suspension", err)
	}
	if err := q.InvalidateRunCheckpoints(ctx, db.InvalidateRunCheckpointsParams{
		ReasonCode: loss.reason, RunID: authority.RunID,
	}); err != nil {
		return cancellationAuthority("invalidate lost checkpoint", err)
	}
	return nil
}

func terminalizeExecutionLeasePhysicalAuthority(
	ctx context.Context,
	q *db.Queries,
	authority db.GetRunExecutionLeaseLossAuthorityRow,
	loss executionLeaseLoss,
	errorPayload json.RawMessage,
) error {
	if err := terminalizeExecutionLeaseFences(ctx, q, authority, loss, errorPayload); err != nil {
		return err
	}
	if err := q.CloseRunRuntimes(ctx, db.CloseRunRuntimesParams{
		ReasonCode: loss.reason, RunLeaseID: authority.RunLeaseID,
		RunID: authority.RunID,
	}); err != nil {
		return cancellationAuthority("request lost Run runtime cleanup", err)
	}
	return nil
}

func terminalizeExecutionLeaseFences(
	ctx context.Context,
	q *db.Queries,
	authority db.GetRunExecutionLeaseLossAuthorityRow,
	loss executionLeaseLoss,
	errorPayload json.RawMessage,
) error {
	affected, err := q.FenceRunWorkspaceLease(ctx, db.FenceRunWorkspaceLeaseParams{
		ReasonCode: loss.reason, ErrorPayload: errorPayload, RunLeaseID: authority.RunLeaseID,
	})
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize lost Run Workspace lease", err)
	}
	affected, err = q.TerminalizeRunLease(ctx, db.TerminalizeRunLeaseParams{
		State: string(loss.state), ReasonCode: loss.reason, ErrorPayload: errorPayload,
		ID: authority.RunLeaseID, RunID: authority.RunID,
	})
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize lost Run lease", err)
	}
	return nil
}

func (g OwnedFinalization) failCurrentForLeaseLoss(
	ctx context.Context,
	loss executionLeaseLoss,
	message string,
	status db.RunStatus,
	actorFailureCode string,
) error {
	if _, err := g.CancelDescendants(ctx); err != nil {
		return err
	}
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return nil
	}
	term := termination{
		reasonCode: loss.reason, errorCode: loss.reason, errorMessage: message,
		runStatus: status, runLeaseState: loss.state, attemptOutcome: "failed",
		waitCondition: db.WaitStateFailed, waitSuspension: db.RunWaitStateFailed,
		eventKind: "run.system_failed", eventMessage: message,
		actorFailureCode: actorFailureCode,
	}
	if status == db.RunStatusExpired {
		term.eventKind = "run.expired"
	}
	if err := terminateLockedRun(ctx, g.tx, target, term); err != nil {
		return err
	}
	if !target.parentRunID.Valid || !target.parentOwnsLifecycle.Valid ||
		!target.parentOwnsLifecycle.Bool {
		return nil
	}
	parentID := uuid.UUID(target.parentRunID.Bytes)
	parent, found := g.locked[parentID]
	if !found || runStatusTerminal(parent.status) {
		return nil
	}
	wait, found := g.waitsByChild[target.id]
	if !found {
		return cancellationAuthority("lost child wait is missing", nil)
	}
	if parent.workspaceID != target.workspaceID {
		result, err := marshalChildFailureResult(target.id, loss.reason, message)
		if err != nil {
			return err
		}
		return resolveDifferentWorkspaceChildWait(ctx, g.tx, parent, wait, result)
	}
	if !wait.baseWorkspaceVersionID.Valid {
		return cancellationAuthority("lost same-Workspace child wait is inconsistent", nil)
	}
	errorPayload, err := leaseLossError(loss.reason, message, false)
	if err != nil {
		return err
	}
	reason := loss.reason
	return resolveTerminalChildWait(ctx, g.tx, parent, wait, terminalChildWaitResolution{
		conditionState: db.WaitStateFailed, reasonCode: &reason,
		conditionError: errorPayload, resumeWorkspaceVersionID: wait.baseWorkspaceVersionID,
	})
}

func leaseLossError(code, message string, retryable bool) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"code": code, "message": message, "retryable": retryable,
	})
}

func executionLeaseLossMessage(reason string) string {
	switch reason {
	case "worker_lost":
		return "Run Worker was lost"
	case "runtime_failed":
		return "Run runtime failed"
	case "lease_expired":
		return "Run execution lease expired"
	case "max_active_duration_exceeded":
		return "Run maximum active duration was exceeded"
	default:
		return fmt.Sprintf("Run execution failed (%s)", reason)
	}
}
