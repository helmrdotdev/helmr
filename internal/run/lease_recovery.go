package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/retry"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type FreshLeaseRecoveryRequest struct {
	RunID                 uuid.UUID
	WorkspaceID           uuid.UUID
	AttemptNumber         int32
	RunLeaseID            uuid.UUID
	RetryResolutions      []secret.Resolution
	RetrySecretsAvailable bool
}

type freshLeaseLoss struct {
	at     time.Time
	kind   string
	reason string
	state  db.RunLeaseState
}

// RecoverFreshLeaseLoss applies the state-specific recovery transition after
// LockOwnedFinalization has acquired the canonical Run graph and physical
// authority order. Exact replay or a race won by claim/start/renewal is a
// no-op.
func (g OwnedFinalization) RecoverFreshLeaseLoss(
	ctx context.Context,
	request FreshLeaseRecoveryRequest,
) (bool, error) {
	if g.tx == nil || g.currentRun != request.RunID || len(g.descendants) == 0 ||
		request.RunID == uuid.Nil || request.WorkspaceID == uuid.Nil ||
		request.AttemptNumber <= 0 || request.RunLeaseID == uuid.Nil {
		return false, errors.New("fresh Run lease recovery authority is invalid")
	}
	target := g.descendants[0]
	if target.id != request.RunID || target.workspaceID != request.WorkspaceID ||
		target.currentAttemptNumber != request.AttemptNumber ||
		!target.currentRunLeaseID.Valid ||
		uuid.UUID(target.currentRunLeaseID.Bytes) != request.RunLeaseID {
		return false, nil
	}
	q := db.New(g.tx)
	authority, err := q.GetFreshRunLeaseLossAuthority(
		ctx,
		db.GetFreshRunLeaseLossAuthorityParams{
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
		return false, cancellationAuthority("load fresh Run lease loss authority", err)
	}
	loss, ok, err := decideFreshLeaseLoss(authority)
	if err != nil || !ok {
		return false, err
	}
	_, sameWorkspaceChild, err := g.sameWorkspaceChildRecoveryBoundary(target)
	if err != nil {
		return false, err
	}
	if sameWorkspaceChild && authority.RunLeaseState == string(db.RunLeaseStateAssigned) &&
		freshAssignedHandoffRetainable(authority) {
		if err := recoverFreshAssignedHandoff(ctx, q, authority, loss); err != nil {
			return false, err
		}
		return true, nil
	}
	if authority.RunLeaseState == string(db.RunLeaseStateAssigned) ||
		authority.RunLeaseState == string(db.RunLeaseStateStarting) {
		if sameWorkspaceChild {
			loss.reason = "same_workspace_handoff_runtime_lost"
			loss.state = db.RunLeaseStateLost
			if err := g.failCurrentForLeaseLoss(
				ctx,
				loss,
				"Same-Workspace child handoff runtime was lost",
				db.RunStatusSystemFailed,
				"platform_failure",
			); err != nil {
				return false, err
			}
			return true, nil
		}
		if err := recoverFreshPrestartLease(ctx, q, authority, loss); err != nil {
			return false, err
		}
		return true, nil
	}
	if authority.RunLeaseState != string(db.RunLeaseStateRunning) {
		return false, nil
	}
	if _, err := q.StopLostRunActiveInterval(ctx, db.StopLostRunActiveIntervalParams{
		LossAt: pgvalue.Timestamptz(loss.at), RunID: authority.RunID,
		WorkspaceID: authority.WorkspaceID, ExpectedStateVersion: authority.StateVersion,
		AttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
	}); err != nil {
		return false, cancellationAuthority("stop lost Run active interval", err)
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
	if sameWorkspaceChild {
		loss.reason = "same_workspace_handoff_runtime_lost"
		loss.state = db.RunLeaseStateLost
		if err := g.failCurrentForLeaseLoss(
			ctx,
			loss,
			"Same-Workspace child handoff runtime was lost",
			db.RunStatusSystemFailed,
			"platform_failure",
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
		return false, cancellationAuthority("apply fresh Run retry policy", err)
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
			ctx, loss, freshLeaseLossMessage(loss.reason), db.RunStatusSystemFailed, "platform_failure",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	if len(g.descendants) != 1 {
		return false, cancellationAuthority("running retry retained an owned descendant", nil)
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

func (g OwnedFinalization) sameWorkspaceChildRecoveryBoundary(
	target cancellationRun,
) (cancellationWait, bool, error) {
	if !target.parentRunID.Valid || !target.parentOwnsLifecycle.Valid ||
		!target.parentOwnsLifecycle.Bool {
		return cancellationWait{}, false, nil
	}
	parent, found := g.locked[uuid.UUID(target.parentRunID.Bytes)]
	if !found {
		return cancellationWait{}, false, cancellationAuthority(
			"fresh child recovery parent is unavailable", nil,
		)
	}
	if parent.workspaceID != target.workspaceID {
		return cancellationWait{}, false, nil
	}
	wait, found := g.waitsByChild[target.id]
	if !found || !wait.childWriterGeneration.Valid ||
		!wait.handoffRuntimeInstanceID.Valid || !wait.handoffWorkspaceMountID.Valid {
		return cancellationWait{}, false, cancellationAuthority(
			"fresh same-Workspace child recovery boundary is unavailable", nil,
		)
	}
	return wait, true, nil
}

func freshAssignedHandoffRetainable(authority db.GetFreshRunLeaseLossAuthorityRow) bool {
	return authority.RunLeaseState == string(db.RunLeaseStateAssigned) &&
		authority.WorkerState == "active" &&
		authority.WorkerCurrentEpoch.Valid &&
		authority.WorkerCurrentEpoch.Int64 == authority.WorkerEpoch &&
		!authority.WorkerLostAt.Valid && !authority.WorkerTerminationReadyAt.Valid &&
		authority.RuntimeDesiredState == "ready" &&
		authority.RuntimeObservedState == "ready" &&
		!authority.RuntimeLostAt.Valid && !authority.RuntimeFailedAt.Valid &&
		authority.WorkspaceLeaseState == "active" &&
		authority.MountState == "mounted" &&
		!authority.MountLostAt.Valid && !authority.MountFailedAt.Valid
}

func recoverFreshAssignedHandoff(
	ctx context.Context,
	q *db.Queries,
	authority db.GetFreshRunLeaseLossAuthorityRow,
	loss freshLeaseLoss,
) error {
	errorPayload, err := leaseLossError(loss.reason, freshLeaseLossMessage(loss.reason), true)
	if err != nil {
		return err
	}
	if err := terminalizeFreshLeaseFences(ctx, q, authority, loss, errorPayload); err != nil {
		return err
	}
	if _, err := q.ClearFreshPrestartRunLease(ctx, db.ClearFreshPrestartRunLeaseParams{
		RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
		ExpectedStateVersion: authority.StateVersion, AttemptNumber: authority.CurrentAttemptNumber,
		RunLeaseID: authority.RunLeaseID,
	}); err != nil {
		return cancellationAuthority("clear retained handoff child Run lease", err)
	}
	return nil
}

func decideFreshLeaseLoss(
	authority db.GetFreshRunLeaseLossAuthorityRow,
) (freshLeaseLoss, bool, error) {
	if !authority.ObservedAt.Valid || !authority.RunLeaseExpiresAt.Valid ||
		!authority.StartDeadlineAt.Valid {
		return freshLeaseLoss{}, false, errors.New("fresh Run lease loss timestamps are incomplete")
	}
	candidates := make([]freshLeaseLoss, 0, 9)
	add := func(at pgtype.Timestamptz, kind, reason string, state db.RunLeaseState) {
		if at.Valid {
			candidates = append(candidates, freshLeaseLoss{at: at.Time, kind: kind, reason: reason, state: state})
		}
	}
	add(authority.RunLeaseExpiresAt, "lease", "lease_expired", db.RunLeaseStateExpired)
	if authority.RunLeaseState == string(db.RunLeaseStateRunning) {
		if !authority.ActiveStartedAt.Valid || authority.MaxActiveDurationMs < authority.ActiveElapsedMs {
			return freshLeaseLoss{}, false, errors.New("fresh running Lease active budget is invalid")
		}
		hardDeadline := authority.ActiveStartedAt.Time.Add(
			time.Duration(authority.MaxActiveDurationMs-authority.ActiveElapsedMs) * time.Millisecond,
		)
		candidates = append(candidates, freshLeaseLoss{
			at: hardDeadline, kind: "active_deadline", reason: "max_active_duration_exceeded",
			state: db.RunLeaseStateExpired,
		})
	} else {
		add(authority.StartDeadlineAt, "start_deadline", "lease_expired", db.RunLeaseStateExpired)
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
		return freshLeaseLoss{}, false, nil
	}
	chosen := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.at.Before(chosen.at) ||
			(candidate.at.Equal(chosen.at) && freshLossPriority(candidate.kind) < freshLossPriority(chosen.kind)) {
			chosen = candidate
		}
	}
	if chosen.at.After(authority.ObservedAt.Time) {
		return freshLeaseLoss{}, false, nil
	}
	return chosen, true, nil
}

func freshLossPriority(kind string) int {
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

func recoverFreshPrestartLease(
	ctx context.Context,
	q *db.Queries,
	authority db.GetFreshRunLeaseLossAuthorityRow,
	loss freshLeaseLoss,
) error {
	errorPayload, err := leaseLossError(loss.reason, freshLeaseLossMessage(loss.reason), true)
	if err != nil {
		return err
	}
	if err := terminalizeFreshLeasePhysicalAuthority(ctx, q, authority, loss, errorPayload); err != nil {
		return err
	}
	if _, err := q.ClearFreshPrestartRunLease(ctx, db.ClearFreshPrestartRunLeaseParams{
		RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
		ExpectedStateVersion: authority.StateVersion, AttemptNumber: authority.CurrentAttemptNumber,
		RunLeaseID: authority.RunLeaseID,
	}); err != nil {
		return cancellationAuthority("clear fresh pre-start Run lease", err)
	}
	return nil
}

func (g OwnedFinalization) retryCurrentAfterLeaseLoss(
	ctx context.Context,
	authority db.GetFreshRunLeaseLossAuthorityRow,
	loss freshLeaseLoss,
	retryAt time.Time,
	resolutions []secret.Resolution,
) error {
	q := db.New(g.tx)
	errorPayload, err := leaseLossError(loss.reason, freshLeaseLossMessage(loss.reason), true)
	if err != nil {
		return err
	}
	if err := terminalizeFreshLeasePhysicalAuthority(ctx, q, authority, loss, errorPayload); err != nil {
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
		if _, err := q.CreateTaskRetryAttempt(ctx, db.CreateTaskRetryAttemptParams{
			Number: nextAttempt, RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
			PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return cancellationAuthority("create lost Task retry Attempt", err)
		}
	case "actor":
		if !authority.ActorRunGeneration.Valid || !authority.SessionID.Valid {
			return cancellationAuthority("lost Actor retry authority is incomplete", nil)
		}
		if _, err := q.CreateActorRetryAttempt(ctx, db.CreateActorRetryAttemptParams{
			Number: nextAttempt, ExpectedRunGeneration: authority.ActorRunGeneration.Int64,
			RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
			PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return cancellationAuthority("create lost Actor retry Attempt", err)
		}
	default:
		return cancellationAuthority("lost Run entrypoint kind is unsupported", nil)
	}
	if err := secret.CreateAttemptResolutions(
		ctx, q, authority.WorkspaceID, authority.RunID, nextAttempt, resolutions,
	); err != nil {
		return cancellationAuthority("record lost Run retry Secret resolutions", err)
	}
	completedAt := authority.ObservedAt
	retryTimestamp := pgvalue.Timestamptz(retryAt)
	if authority.EntrypointKind == "task" {
		if _, err := q.DelayTaskRunRetry(ctx, db.DelayTaskRunRetryParams{
			NextAttemptNumber: nextAttempt, CompletedAt: completedAt, RetryAt: retryTimestamp,
			ID: authority.RunID, WorkspaceID: authority.WorkspaceID,
			PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return cancellationAuthority("delay lost Task retry", err)
		}
	} else {
		if _, err := q.DelayActorRunRetry(ctx, db.DelayActorRunRetryParams{
			NextAttemptNumber: nextAttempt, RetryAt: retryTimestamp, CompletedAt: completedAt,
			ID: authority.RunID, WorkspaceID: authority.WorkspaceID, SessionID: authority.SessionID,
			PreviousAttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return cancellationAuthority("delay lost Actor retry", err)
		}
	}
	return nil
}

func terminalizeFreshLeasePhysicalAuthority(
	ctx context.Context,
	q *db.Queries,
	authority db.GetFreshRunLeaseLossAuthorityRow,
	loss freshLeaseLoss,
	errorPayload json.RawMessage,
) error {
	if err := terminalizeFreshLeaseFences(ctx, q, authority, loss, errorPayload); err != nil {
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

func terminalizeFreshLeaseFences(
	ctx context.Context,
	q *db.Queries,
	authority db.GetFreshRunLeaseLossAuthorityRow,
	loss freshLeaseLoss,
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
	loss freshLeaseLoss,
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
	if err := terminateLockedRun(ctx, g.tx, target, pgtype.UUID{}, pgtype.UUID{}, term); err != nil {
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
	if !wait.baseWorkspaceVersionID.Valid || !wait.handoffRuntimeInstanceID.Valid ||
		!wait.handoffWorkspaceMountID.Valid {
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

func freshLeaseLossMessage(reason string) string {
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
