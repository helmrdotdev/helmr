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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type ExecutionLeaseRecoveryRequest struct {
	RunID         uuid.UUID
	WorkspaceID   uuid.UUID
	AttemptNumber int32
	RunLeaseID    uuid.UUID
}

// LockExecutionLeaseRecovery locks the complete ownership subtree affected by
// losing a shared Computer. Run ancestry and Computer assignment are immutable;
// the canonical graph locker revalidates the discovered subtree under locks.
func LockExecutionLeaseRecovery(ctx context.Context, tx pgx.Tx, request OwnedFinalizationRequest) (OwnedFinalization, error) {
	rows, err := db.New(tx).ListCancellationLineage(ctx, db.ListCancellationLineageParams{
		TargetID: pgvalue.UUID(request.RunID), MaxDepth: maxCancellationGraphSize,
	})
	if err != nil {
		return OwnedFinalization{}, err
	}
	if len(rows) == 0 || len(rows) > maxCancellationGraphSize {
		return OwnedFinalization{}, cancellationAuthority("execution recovery lineage is incomplete", nil)
	}
	for _, row := range rows {
		if row.Cycle {
			return OwnedFinalization{}, cancellationAuthority("execution recovery lineage contains a cycle", nil)
		}
	}
	computer := rows[len(rows)-1].WorkspaceID
	for i := len(rows) - 1; i >= 0 && rows[i].WorkspaceID == computer; i-- {
		request.RunID = uuid.UUID(rows[i].ID.Bytes)
	}
	return LockOwnedFinalization(ctx, tx, request)
}

// executionTarget narrows an already locked graph for a pre-start transition
// that does not invalidate the shared Computer or its ancestors.
func (g OwnedFinalization) executionTarget(runID uuid.UUID) (OwnedFinalization, error) {
	_, found := g.locked[runID]
	if !found {
		return OwnedFinalization{}, cancellationAuthority("execution recovery target was not locked", nil)
	}
	selected := map[uuid.UUID]bool{runID: true}
	descendants := make([]cancellationRun, 0, len(g.descendants))
	for _, r := range g.descendants {
		if r.id != runID && (!r.parentRunID.Valid || !selected[uuid.UUID(r.parentRunID.Bytes)]) {
			continue
		}
		selected[r.id] = true
		descendants = append(descendants, r)
	}
	g.currentRun = runID
	g.descendants = descendants
	return g, nil
}

type executionLeaseLoss struct {
	at     time.Time
	kind   string
	reason string
	state  db.RunLeaseStatus
}

// RecoverExecutionLeaseLoss applies the state-specific recovery transition after
// LockOwnedFinalization has acquired the canonical Run graph and physical
// authority order. Exact replay or a race won by claim/start/renewal is a
// no-op.
func (g OwnedFinalization) RecoverExecutionLeaseLoss(
	ctx context.Context,
	request ExecutionLeaseRecoveryRequest,
) (bool, error) {
	if g.tx == nil || len(g.descendants) == 0 ||
		request.RunID == uuid.Nil() || request.WorkspaceID == uuid.Nil() ||
		request.AttemptNumber <= 0 || request.RunLeaseID == uuid.Nil() {
		return false, errors.New("Run execution lease recovery authority is invalid")
	}
	target, found := g.locked[request.RunID]
	if !found || target.id != request.RunID || target.workspaceID != request.WorkspaceID ||
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
	if (authority.RunLeaseStatus == string(db.RunLeaseStatusAssigned) ||
		authority.RunLeaseStatus == string(db.RunLeaseStatusStarting)) &&
		!(target.actorID.Valid && (authority.ActorDispatchHoldID.Valid || authority.HasResumeWait)) {
		targetGraph, err := g.executionTarget(request.RunID)
		if err != nil {
			return false, err
		}
		cleared, err := recoverExecutionPrestartLease(ctx, q, authority, loss)
		if err != nil {
			return false, err
		}
		if loss.kind == "physical_failure" {
			if err := targetGraph.recordClearedExecutionPrestart(cleared); err != nil {
				return false, err
			}
			if _, err := targetGraph.ChargeRuntimePreparationFailure(ctx); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	leaseStatus := db.RunLeaseStatus(authority.RunLeaseStatus)
	switch leaseStatus {
	case db.RunLeaseStatusRunning, db.RunLeaseStatusCheckpointing:
		if _, err := q.StopLostRunActiveInterval(ctx, db.StopLostRunActiveIntervalParams{
			LossAt: pgvalue.Timestamptz(loss.at), RunID: authority.RunID,
			WorkspaceID: authority.WorkspaceID, ExpectedRevision: authority.Revision,
			AttemptNumber: authority.CurrentAttemptNumber, RunLeaseID: authority.RunLeaseID,
		}); err != nil {
			return false, cancellationAuthority("stop lost Run active interval", err)
		}
	case db.RunLeaseStatusAssigned, db.RunLeaseStatusStarting:
		// A held or unresumable Actor cannot retry a restored process.
	case db.RunLeaseStatusFinalizing:
		// Finalization starts only after the active interval is durably stopped.
		// Preserve its immutable finalization receipt on the terminal Lease.
	default:
		return false, nil
	}
	// Retain the committed source and require physical exclusion before cold retry.
	if leaseStatus == db.RunLeaseStatusRunning || leaseStatus == db.RunLeaseStatusCheckpointing || leaseStatus == db.RunLeaseStatusFinalizing {
		affected, err := q.RequireLostRunComputerRecovery(ctx, db.RequireLostRunComputerRecoveryParams{
			WorkspaceID: authority.WorkspaceID, RunLeaseID: authority.RunLeaseID,
			RecoveryID: pgvalue.UUID(uuid.NewV7()), RecoveryReason: pgvalue.Text(loss.reason),
		})
		if err != nil || affected != 1 {
			return false, cancellationAuthority("require lost Computer recovery", err)
		}
	}
	status := db.RunStatusSystemFailed
	message := executionLeaseLossMessage(loss.reason)
	if loss.kind == "active_deadline" {
		status = db.RunStatusExpired
		message = "Run maximum active duration was exceeded"
	}
	if status != db.RunStatusExpired {
		retried, err := g.retryLostTaskTree(ctx, loss, message)
		if err != nil || retried {
			return retried, err
		}
	}
	if err := g.failCurrentForLeaseLoss(ctx, request.RunID, loss, message, status); err != nil {
		return false, err
	}
	return true, nil
}

// FailCheckpointExecution abandons an entered Computer whose suspension could
// not be saved. The caller validates the Worker receipt and records its replay
// identity in the same transaction after LockExecutionLeaseRecovery.
func (g OwnedFinalization) FailCheckpointExecution(ctx context.Context, request ExecutionLeaseRecoveryRequest, message string) error {
	target, found := g.locked[request.RunID]
	if g.tx == nil || !found || target.workspaceID != request.WorkspaceID ||
		target.currentAttemptNumber != request.AttemptNumber || target.currentRunLeaseID != pgvalue.UUID(request.RunLeaseID) {
		return cancellationAuthority("checkpoint failure target is not locked", nil)
	}
	q := db.New(g.tx)
	a, err := q.GetRunExecutionLeaseLossAuthority(ctx, db.GetRunExecutionLeaseLossAuthorityParams{
		RunID: pgvalue.UUID(request.RunID), WorkspaceID: pgvalue.UUID(request.WorkspaceID),
		AttemptNumber: request.AttemptNumber, RunLeaseID: pgvalue.UUID(request.RunLeaseID),
	})
	if err != nil {
		return err
	}
	if !a.ObservedAt.Valid || !a.ActiveStartedAt.Valid {
		return cancellationAuthority("checkpoint failure active timestamps are missing", nil)
	}
	affected, err := q.RequireLostRunComputerRecovery(ctx, db.RequireLostRunComputerRecoveryParams{WorkspaceID: a.WorkspaceID, RunLeaseID: a.RunLeaseID, RecoveryID: pgvalue.UUID(uuid.NewV7()), RecoveryReason: pgvalue.Text("checkpoint_failed")})
	if err != nil || affected != 1 {
		return cancellationAuthority("require unsaved Computer recovery", err)
	}
	loss := executionLeaseLoss{at: a.ObservedAt.Time, reason: "checkpoint_failed", state: db.RunLeaseStatusFailed}
	status := db.RunStatusSystemFailed
	if !a.ActiveStartedAt.Time.Add(time.Duration(a.MaxActiveDurationMs-a.ActiveElapsedMs) * time.Millisecond).After(a.ObservedAt.Time) {
		loss.reason = "max_active_duration_exceeded"
		status = db.RunStatusExpired
	}
	if status != db.RunStatusExpired {
		retried, err := g.retryLostTaskTree(ctx, loss, message)
		if err != nil || retried {
			return err
		}
	}
	return g.failCurrentForLeaseLoss(ctx, request.RunID, loss, message, status)
}

func decideExecutionLeaseLoss(
	authority db.GetRunExecutionLeaseLossAuthorityRow,
) (executionLeaseLoss, bool, error) {
	if !authority.ObservedAt.Valid || !authority.RunLeaseExpiresAt.Valid ||
		!authority.StartDeadlineAt.Valid {
		return executionLeaseLoss{}, false, errors.New("Run execution lease loss timestamps are incomplete")
	}
	candidates := make([]executionLeaseLoss, 0, 9)
	add := func(at pgtype.Timestamptz, kind, reason string, state db.RunLeaseStatus) {
		if at.Valid {
			candidates = append(candidates, executionLeaseLoss{at: at.Time, kind: kind, reason: reason, state: state})
		}
	}
	add(authority.RunLeaseExpiresAt, "lease", "lease_expired", db.RunLeaseStatusExpired)
	switch db.RunLeaseStatus(authority.RunLeaseStatus) {
	case db.RunLeaseStatusRunning, db.RunLeaseStatusCheckpointing:
		if !authority.ActiveStartedAt.Valid || authority.MaxActiveDurationMs < authority.ActiveElapsedMs {
			return executionLeaseLoss{}, false, errors.New("active Run execution Lease budget is invalid")
		}
		hardDeadline := authority.ActiveStartedAt.Time.Add(
			time.Duration(authority.MaxActiveDurationMs-authority.ActiveElapsedMs) * time.Millisecond,
		)
		candidates = append(candidates, executionLeaseLoss{
			at: hardDeadline, kind: "active_deadline", reason: "max_active_duration_exceeded",
			state: db.RunLeaseStatusExpired,
		})
	case db.RunLeaseStatusAssigned, db.RunLeaseStatusStarting:
		add(authority.StartDeadlineAt, "start_deadline", "lease_expired", db.RunLeaseStatusExpired)
	case db.RunLeaseStatusFinalizing:
		// Finalizing has neither a start nor active deadline. Its renewable Lease
		// expiry and physical authority are the only recovery boundaries.
	default:
		return executionLeaseLoss{}, false, nil
	}
	add(authority.WorkerLostAt, "physical_loss", "worker_lost", db.RunLeaseStatusLost)
	add(authority.WorkerTerminationReadyAt, "physical_loss", "worker_lost", db.RunLeaseStatusLost)
	if authority.WorkerCurrentEpoch.Valid && authority.WorkerCurrentEpoch.Int64 != authority.WorkerEpoch {
		epochAt := authority.WorkerEpochStartedAt
		if !epochAt.Valid {
			epochAt = authority.WorkerUpdatedAt
		}
		add(epochAt, "physical_loss", "worker_lost", db.RunLeaseStatusLost)
	}
	add(authority.RuntimeLostAt, "physical_loss", "worker_lost", db.RunLeaseStatusLost)
	add(authority.MountLostAt, "physical_loss", "worker_lost", db.RunLeaseStatusLost)
	add(authority.RuntimeFailedAt, "physical_failure", "runtime_failed", db.RunLeaseStatusLost)
	add(authority.MountFailedAt, "physical_failure", "runtime_failed", db.RunLeaseStatusLost)
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
		ExpectedRevision: authority.Revision, AttemptNumber: authority.CurrentAttemptNumber,
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
	updated.revision = cleared.Revision
	updated.status = cleared.Status
	updated.runtimePreparationCount = cleared.RuntimePreparationCount
	g.descendants[0] = updated
	g.locked[updated.id] = updated
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
		Status: string(loss.state), ReasonCode: loss.reason, ErrorPayload: errorPayload,
		ID: authority.RunLeaseID, RunID: authority.RunID,
	})
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize lost Run lease", err)
	}
	return nil
}

func (g OwnedFinalization) failCurrentForLeaseLoss(
	ctx context.Context,
	lostRunID uuid.UUID,
	loss executionLeaseLoss,
	message string,
	status db.RunStatus,
) error {
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return nil
	}
	term := termination{
		reasonCode: loss.reason, errorCode: loss.reason, errorMessage: message,
		runStatus: status, runLeaseStatus: loss.state, attemptOutcome: "failed",
		waitCondition: db.WaitStatusFailed, waitSuspension: db.RunWaitStatusFailed,
		eventKind: "run.system_failed", eventMessage: message,
	}
	if status == db.RunStatusExpired {
		term.eventKind = "run.expired"
	}
	lostTerm := term
	recoveryTerm := term
	recoveryTerm.reasonCode, recoveryTerm.errorCode = "computer_recovery_required", "computer_recovery_required"
	recoveryTerm.errorMessage, recoveryTerm.eventMessage = "Shared Computer execution was lost", "Shared Computer execution was lost"
	recoveryTerm.runStatus, recoveryTerm.runLeaseStatus = db.RunStatusSystemFailed, db.RunLeaseStatusLost
	recoveryTerm.eventKind = "run.system_failed"
	termFor := func(id uuid.UUID) termination {
		if id == lostRunID {
			return lostTerm
		}
		return recoveryTerm
	}
	// Descendants are discovered parent-first. Unwind them before their owners;
	// every scope sharing the lost Computer has the same recovery failure.
	for i := len(g.descendants) - 1; i > 0; i-- {
		child := g.descendants[i]
		if child.workspaceID == target.workspaceID {
			if err := terminateLockedRun(ctx, g.tx, child, termFor(child.id)); err != nil {
				return err
			}
		} else if err := cancelLockedRun(ctx, g.tx, child); err != nil {
			return err
		}
	}
	term = termFor(target.id)
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
		if parent.workspaceID != target.workspaceID {
			return nil
		}
		return cancellationAuthority("lost child wait is missing", nil)
	}
	if parent.workspaceID != target.workspaceID {
		result, err := marshalChildFailureResult(target.id, term.reasonCode, term.errorMessage)
		if err != nil {
			return err
		}
		return resolveDifferentWorkspaceChildWait(ctx, g.tx, parent, wait, result)
	}
	return cancellationAuthority("lost Computer owner was not included in recovery graph", nil)
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
