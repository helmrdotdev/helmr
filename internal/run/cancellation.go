package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrCancellationNotFound  = errors.New("run not found")
	ErrCancellationConflict  = errors.New("run has another terminal outcome")
	ErrCancellationAuthority = errors.New("run cancellation authority is inconsistent")
)

const maxCancellationGraphSize = 1000

type CancellationDB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type CancellationRequest struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
}

type CancellationResult struct {
	RunID         uuid.UUID
	Changed       bool
	CancelledRuns int
}

type Canceler struct {
	db CancellationDB
}

type cancellationRun struct {
	id                      uuid.UUID
	parentRunID             pgtype.UUID
	parentOwnsLifecycle     pgtype.Bool
	environmentID           uuid.UUID
	workspaceID             uuid.UUID
	actorID                 pgtype.UUID
	status                  db.RunStatus
	currentAttemptNumber    int32
	currentRunLeaseID       pgtype.UUID
	stateVersion            int64
	runtimePreparationCount int32
	depth                   int
}

type cancellationWait struct {
	id                       uuid.UUID
	runID                    uuid.UUID
	workspaceID              uuid.UUID
	childRunID               pgtype.UUID
	conditionState           db.WaitState
	suspensionState          db.RunWaitState
	expectedRunStateVersion  int64
	attemptNumber            int32
	currentRunLeaseID        pgtype.UUID
	priorRunLeaseID          pgtype.UUID
	suspendCheckpointID      pgtype.UUID
	handoffRuntimeInstanceID pgtype.UUID
	handoffWorkspaceMountID  pgtype.UUID
	baseWorkspaceVersionID   pgtype.UUID
	childWriterGeneration    pgtype.Int8
}

type terminalChildWaitResolution struct {
	conditionState           db.WaitState
	result                   json.RawMessage
	reasonCode               *string
	conditionError           json.RawMessage
	resumeWorkspaceVersionID pgtype.UUID
}

type termination struct {
	reasonCode        string
	errorCode         string
	errorMessage      string
	runStatus         db.RunStatus
	runLeaseState     db.RunLeaseState
	attemptOutcome    string
	waitCondition     db.WaitState
	waitSuspension    db.RunWaitState
	eventKind         string
	eventMessage      string
	actorFailureCode  string
	actorCancellation bool
	discardRuntime    bool
}

var cancelledTermination = termination{
	reasonCode:        "run_cancelled",
	errorCode:         "run_cancelled",
	errorMessage:      "Run was cancelled",
	runStatus:         db.RunStatusCancelled,
	runLeaseState:     db.RunLeaseStateCancelled,
	attemptOutcome:    "cancelled",
	waitCondition:     db.WaitStateCancelled,
	waitSuspension:    db.RunWaitStateCancelled,
	eventKind:         "run.cancelled",
	eventMessage:      "Run cancelled",
	actorCancellation: true,
}

var secretRevokedTermination = termination{
	reasonCode:       "secret_revoked",
	errorCode:        "secret_revoked",
	errorMessage:     "A Workspace Secret used by this Run was revoked",
	runStatus:        db.RunStatusFailed,
	runLeaseState:    db.RunLeaseStateFailed,
	attemptOutcome:   "failed",
	waitCondition:    db.WaitStateFailed,
	waitSuspension:   db.RunWaitStateFailed,
	eventKind:        "run.failed",
	eventMessage:     "Run failed",
	actorFailureCode: "run_failed",
}

var runtimePreparationTermination = termination{
	reasonCode:       "runtime_preparation_failed",
	errorCode:        "runtime_preparation_failed",
	errorMessage:     "Run runtime preparation failed",
	runStatus:        db.RunStatusSystemFailed,
	runLeaseState:    db.RunLeaseStateFailed,
	attemptOutcome:   "failed",
	waitCondition:    db.WaitStateFailed,
	waitSuspension:   db.RunWaitStateFailed,
	eventKind:        "run.system_failed",
	eventMessage:     "Run runtime preparation failed",
	actorFailureCode: "platform_failure",
}

func NewCanceler(database CancellationDB) (*Canceler, error) {
	if database == nil {
		return nil, errors.New("run cancellation database is required")
	}
	return &Canceler{db: database}, nil
}

type OwnedFinalizationRequest struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
}

type OwnedFinalization struct {
	tx           pgx.Tx
	currentRun   uuid.UUID
	descendants  []cancellationRun
	locked       map[uuid.UUID]cancellationRun
	waitsByChild map[uuid.UUID]cancellationWait
}

// LockOwnedFinalization acquires the global cancellation
// lock order before ordinary Run authority is locked. It lets terminal
// finalization re-lock its exact authority without later reaching from a
// Workspace lock back to an unlocked descendant Run.
func LockOwnedFinalization(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedFinalizationRequest,
) (OwnedFinalization, error) {
	return lockOwnedFinalization(ctx, tx, request, nil)
}

// LockOwnedFinalizationWithRuntimeFence acquires the owned Run graph in the
// same order as placement, invoking beforeRuntime after Run, Workspace,
// attempt, Wait, and checkpoint authority is locked but before Runtime and
// lease authority. Callers that must fence provider supply before consuming a
// Runtime use the hook to preserve Run -> Worker Group -> Pool -> Worker ->
// Runtime ordering.
func LockOwnedFinalizationWithRuntimeFence(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedFinalizationRequest,
	beforeRuntime func() error,
) (OwnedFinalization, error) {
	if beforeRuntime == nil {
		return OwnedFinalization{}, errors.New("owned run finalization Runtime fence is required")
	}
	return lockOwnedFinalization(ctx, tx, request, beforeRuntime)
}

func lockOwnedFinalization(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedFinalizationRequest,
	beforeRuntime func() error,
) (OwnedFinalization, error) {
	if tx == nil || request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil || request.RunID == uuid.Nil {
		return OwnedFinalization{}, errors.New("owned run finalization graph authority is required")
	}
	scope := CancellationRequest{
		OrgID: request.OrgID, ProjectID: request.ProjectID,
		EnvironmentID: request.EnvironmentID,
	}
	lineage, err := cancellationLineage(ctx, tx, request.RunID)
	if err != nil {
		return OwnedFinalization{}, err
	}
	descendantIDs, err := discoverOwnedCancellationRuns(
		ctx, tx, scope, request.RunID,
	)
	if err != nil {
		return OwnedFinalization{}, err
	}
	lockOrder := append(slices.Clone(lineage), descendantIDs[1:]...)
	if len(lockOrder) > maxCancellationGraphSize {
		return OwnedFinalization{}, cancellationAuthority(
			"owned run finalization graph exceeds the transaction bound",
			nil,
		)
	}
	if err := lockCancellationActors(ctx, tx, scope, lockOrder); err != nil {
		return OwnedFinalization{}, err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lockOrder))
	for _, id := range lockOrder {
		run, err := lockCancellationRun(ctx, tx, scope, id)
		if err != nil {
			return OwnedFinalization{}, cancellationAuthority(
				"lock owned run finalization graph",
				err,
			)
		}
		locked[id] = run
	}
	reloaded, err := discoverOwnedCancellationRuns(
		ctx, tx, scope, request.RunID,
	)
	if err != nil {
		return OwnedFinalization{}, err
	}
	if !slices.Equal(descendantIDs, reloaded) {
		return OwnedFinalization{}, cancellationAuthority(
			"owned run finalization graph changed during lock acquisition",
			nil,
		)
	}
	descendants := make([]cancellationRun, 0, len(descendantIDs))
	for depth, id := range descendantIDs {
		run, found := locked[id]
		if !found {
			return OwnedFinalization{}, cancellationAuthority(
				"owned run finalization descendant was not locked",
				nil,
			)
		}
		run.depth = depth
		descendants = append(descendants, run)
	}
	waitsByChild, err := lockCancellationResources(
		ctx, tx, lockOrder, descendants, beforeRuntime,
	)
	if err != nil {
		return OwnedFinalization{}, err
	}
	return OwnedFinalization{
		tx: tx, currentRun: request.RunID, descendants: descendants,
		locked: locked, waitsByChild: waitsByChild,
	}, nil
}

// CancelDescendants terminalizes all still-active owned descendants without
// resolving the current Run's boundary Wait. The current Run is terminalized
// by the caller in the same transaction.
func (g OwnedFinalization) CancelDescendants(ctx context.Context) (int, error) {
	if g.tx == nil || g.currentRun == uuid.Nil || len(g.descendants) == 0 ||
		g.descendants[0].id != g.currentRun {
		return 0, errors.New("owned run finalization graph is invalid")
	}
	runs := slices.Clone(g.descendants[1:])
	slices.SortFunc(runs, func(left, right cancellationRun) int {
		if left.depth != right.depth {
			return right.depth - left.depth
		}
		return slices.Compare(left.id[:], right.id[:])
	})
	cancelled := 0
	for _, run := range runs {
		if runStatusTerminal(run.status) {
			continue
		}
		if err := cancelLockedRun(
			ctx, g.tx, run, pgtype.UUID{}, pgtype.UUID{},
		); err != nil {
			return 0, err
		}
		cancelled++
	}
	return cancelled, nil
}

// FailCurrentForSecretRevocation terminalizes the graph root with an explicit
// Secret revocation error after cancelling its owned descendants. The caller
// must lock and validate the Workspace's complete Secret set before acquiring
// this graph.
func (g OwnedFinalization) FailCurrentForSecretRevocation(
	ctx context.Context,
) (int, error) {
	cancelled, err := g.CancelDescendants(ctx)
	if err != nil {
		return 0, err
	}
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return cancelled, nil
	}
	if err := terminateLockedRun(
		ctx,
		g.tx,
		target,
		pgtype.UUID{},
		pgtype.UUID{},
		secretRevokedTermination,
	); err != nil {
		return 0, err
	}
	if target.parentRunID.Valid && target.parentOwnsLifecycle.Valid &&
		target.parentOwnsLifecycle.Bool {
		parentID := uuid.UUID(target.parentRunID.Bytes)
		parent, found := g.locked[parentID]
		if found && !runStatusTerminal(parent.status) {
			if parent.workspaceID == target.workspaceID {
				return 0, cancellationAuthority(
					"secret-revoked run retained an active same-workspace parent",
					nil,
				)
			}
			wait, found := g.waitsByChild[target.id]
			if !found || wait.handoffRuntimeInstanceID.Valid ||
				wait.handoffWorkspaceMountID.Valid {
				return 0, cancellationAuthority(
					"secret-revoked child wait boundary is inconsistent",
					nil,
				)
			}
			result, err := marshalChildFailureResult(
				target.id,
				"secret_revoked",
				"A Workspace Secret used by the child Run was revoked",
			)
			if err != nil {
				return 0, err
			}
			if err := resolveDifferentWorkspaceChildWait(
				ctx,
				g.tx,
				parent,
				wait,
				result,
			); err != nil {
				return 0, err
			}
		}
	}
	return cancelled + 1, nil
}

// ChargeRuntimePreparationFailure records one infrastructure delivery failure.
// Exhaustion terminalizes the exact Run and its owned graph without consuming
// the user execution RetryPolicy.
func (g OwnedFinalization) ChargeRuntimePreparationFailure(
	ctx context.Context,
) (bool, error) {
	if g.tx == nil || g.currentRun == uuid.Nil || len(g.descendants) == 0 ||
		g.descendants[0].id != g.currentRun {
		return false, errors.New("runtime preparation failure authority is invalid")
	}
	target := g.descendants[0]
	if target.status != db.RunStatusQueued || target.currentRunLeaseID.Valid {
		return false, cancellationAuthority("runtime preparation target is not queued", nil)
	}
	if target.runtimePreparationCount < 0 || target.runtimePreparationCount > 7 {
		return false, cancellationAuthority("runtime preparation count is invalid", nil)
	}
	queries := db.New(g.tx)
	if target.runtimePreparationCount < 7 {
		if _, err := queries.ChargeRunRuntimePreparationFailure(
			ctx,
			db.ChargeRunRuntimePreparationFailureParams{
				ID:            pgvalue.UUID(target.id),
				AttemptNumber: target.currentAttemptNumber,
				ExpectedCount: target.runtimePreparationCount,
			},
		); err != nil {
			return false, cancellationAuthority("charge runtime preparation failure", err)
		}
		return false, nil
	}
	if _, err := queries.ExhaustRunRuntimePreparation(
		ctx,
		db.ExhaustRunRuntimePreparationParams{
			ID:            pgvalue.UUID(target.id),
			AttemptNumber: target.currentAttemptNumber,
		},
	); err != nil {
		return false, cancellationAuthority("exhaust runtime preparation", err)
	}
	if err := g.failCurrentForRuntimePreparation(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (g OwnedFinalization) failCurrentForRuntimePreparation(ctx context.Context) error {
	if _, err := g.CancelDescendants(ctx); err != nil {
		return err
	}
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return nil
	}
	if err := terminateLockedRun(
		ctx,
		g.tx,
		target,
		pgtype.UUID{},
		pgtype.UUID{},
		runtimePreparationTermination,
	); err != nil {
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
		return cancellationAuthority("runtime preparation child wait is missing", nil)
	}
	result, err := marshalChildFailureResult(
		target.id,
		"runtime_preparation_failed",
		"Child Run runtime preparation failed",
	)
	if err != nil {
		return err
	}
	if parent.workspaceID != target.workspaceID {
		return resolveDifferentWorkspaceChildWait(ctx, g.tx, parent, wait, result)
	}
	if !wait.baseWorkspaceVersionID.Valid || !wait.handoffRuntimeInstanceID.Valid ||
		!wait.handoffWorkspaceMountID.Valid {
		return cancellationAuthority("runtime preparation handoff wait is inconsistent", nil)
	}
	reasonCode := "runtime_preparation_failed"
	return resolveTerminalChildWait(
		ctx,
		g.tx,
		parent,
		wait,
		terminalChildWaitResolution{
			conditionState:           db.WaitStateFailed,
			reasonCode:               &reasonCode,
			conditionError:           json.RawMessage(`{"code":"runtime_preparation_failed","message":"Child Run runtime preparation failed","retryable":false}`),
			resumeWorkspaceVersionID: wait.baseWorkspaceVersionID,
		},
	)
}

func (c *Canceler) Cancel(
	ctx context.Context,
	request CancellationRequest,
) (CancellationResult, error) {
	if request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil ||
		request.RunID == uuid.Nil {
		return CancellationResult{}, errors.New("run cancellation scope and ID are required")
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return CancellationResult{}, fmt.Errorf("begin run cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	targetID, err := findCancellationTarget(ctx, tx, request)
	if errors.Is(err, pgx.ErrNoRows) {
		return CancellationResult{}, ErrCancellationNotFound
	}
	if err != nil {
		return CancellationResult{}, cancellationAuthority("resolve target run", err)
	}
	lineage, err := cancellationLineage(ctx, tx, targetID)
	if err != nil {
		return CancellationResult{}, err
	}
	descendants, err := discoverOwnedCancellationRuns(ctx, tx, request, targetID)
	if err != nil {
		return CancellationResult{}, err
	}
	lockOrder := append(slices.Clone(lineage), descendants[1:]...)
	if len(lockOrder) > maxCancellationGraphSize {
		return CancellationResult{}, cancellationAuthority(
			"run cancellation graph exceeds the transaction bound",
			nil,
		)
	}
	if err := lockCancellationActors(ctx, tx, request, lockOrder); err != nil {
		return CancellationResult{}, err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lockOrder))
	for _, id := range lockOrder {
		run, err := lockCancellationRun(ctx, tx, request, id)
		if err != nil {
			return CancellationResult{}, cancellationAuthority("lock run graph", err)
		}
		locked[id] = run
	}
	target, ok := locked[targetID]
	if !ok {
		return CancellationResult{}, cancellationAuthority("target run was not locked", nil)
	}
	result := CancellationResult{RunID: target.id}
	if runStatusTerminal(target.status) {
		if target.status != db.RunStatusCancelled {
			return CancellationResult{}, ErrCancellationConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return CancellationResult{}, fmt.Errorf("commit run cancellation replay: %w", err)
		}
		return result, nil
	}

	reloaded, err := discoverOwnedCancellationRuns(ctx, tx, request, targetID)
	if err != nil {
		return CancellationResult{}, err
	}
	if !slices.Equal(descendants, reloaded) {
		return CancellationResult{}, cancellationAuthority(
			"run cancellation graph changed during lock acquisition",
			nil,
		)
	}
	cancelled := make(map[uuid.UUID]struct{}, len(descendants))
	runs := make([]cancellationRun, 0, len(descendants))
	for depth, id := range descendants {
		run, found := locked[id]
		if !found {
			return CancellationResult{}, cancellationAuthority(
				"parent-owned run was not locked",
				nil,
			)
		}
		run.depth = depth
		cancelled[id] = struct{}{}
		runs = append(runs, run)
	}
	waitsByChild, err := lockCancellationResources(ctx, tx, lockOrder, runs, nil)
	if err != nil {
		return CancellationResult{}, err
	}
	var preservedRuntimeID, preservedMountID pgtype.UUID
	var boundaryParent cancellationRun
	var boundaryWait cancellationWait
	resolveBoundaryParent := false
	if target.parentRunID.Valid && target.parentOwnsLifecycle.Valid &&
		target.parentOwnsLifecycle.Bool {
		parentID := uuid.UUID(target.parentRunID.Bytes)
		if _, parentCancelled := cancelled[parentID]; !parentCancelled {
			var found bool
			boundaryParent, found = locked[parentID]
			if !found {
				return CancellationResult{}, cancellationAuthority(
					"parent-owned run parent was not locked",
					nil,
				)
			}
			boundaryWait, found = waitsByChild[target.id]
			if !found {
				return CancellationResult{}, cancellationAuthority(
					"parent-owned run wait was not locked",
					nil,
				)
			}
			preservedRuntimeID, preservedMountID, err = retainedCancellationHandoff(
				boundaryParent,
				target,
				boundaryWait,
			)
			if err != nil {
				return CancellationResult{}, err
			}
			resolveBoundaryParent = true
		}
	}
	slices.SortFunc(runs, func(left, right cancellationRun) int {
		if left.depth != right.depth {
			return right.depth - left.depth
		}
		return slices.Compare(left.id[:], right.id[:])
	})
	for _, run := range runs {
		if err := cancelLockedRun(
			ctx,
			tx,
			run,
			preservedRuntimeID,
			preservedMountID,
		); err != nil {
			return CancellationResult{}, err
		}
		if resolveBoundaryParent && run.id == target.id {
			if err := resolveCancelledChildWait(
				ctx,
				tx,
				boundaryParent,
				run,
				boundaryWait,
			); err != nil {
				return CancellationResult{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CancellationResult{}, fmt.Errorf("commit run cancellation: %w", err)
	}
	result.Changed = true
	result.CancelledRuns = len(runs)
	return result, nil
}

func findCancellationTarget(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
) (uuid.UUID, error) {
	id, err := db.New(tx).FindCancellationTarget(ctx, db.FindCancellationTargetParams{
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		ID:            pgvalue.UUID(request.RunID),
	})
	return uuid.UUID(id.Bytes), err
}

func cancellationLineage(
	ctx context.Context,
	tx pgx.Tx,
	targetID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).ListCancellationLineage(ctx, db.ListCancellationLineageParams{
		TargetID: pgvalue.UUID(targetID),
		MaxDepth: maxCancellationGraphSize,
	})
	if err != nil {
		return nil, cancellationAuthority("load run lineage", err)
	}
	var ids []uuid.UUID
	for _, row := range rows {
		if row.Cycle {
			return nil, cancellationAuthority("run lineage contains a cycle", nil)
		}
		ids = append(ids, uuid.UUID(row.ID.Bytes))
		if len(ids) > maxCancellationGraphSize {
			return nil, cancellationAuthority("run lineage exceeds the transaction bound", nil)
		}
	}
	if len(ids) == 0 || ids[len(ids)-1] != targetID {
		return nil, cancellationAuthority("run lineage is incomplete", nil)
	}
	return ids, nil
}

func lockCancellationActors(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
	lineage []uuid.UUID,
) error {
	_, err := db.New(tx).LockCancellationActors(ctx, db.LockCancellationActorsParams{
		RunIDs:        pgUUIDs(lineage),
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
	})
	if err != nil {
		return cancellationAuthority("lock run lineage actors", err)
	}
	return nil
}

func lockCancellationRun(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
	id uuid.UUID,
) (cancellationRun, error) {
	row, err := db.New(tx).LockCancellationRun(ctx, db.LockCancellationRunParams{
		ID:            pgvalue.UUID(id),
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
	})
	if err != nil {
		return cancellationRun{}, err
	}
	return cancellationRun{
		id:                      uuid.UUID(row.ID.Bytes),
		parentRunID:             row.ParentRunID,
		parentOwnsLifecycle:     row.ParentOwnsLifecycle,
		environmentID:           uuid.UUID(row.EnvironmentID.Bytes),
		workspaceID:             uuid.UUID(row.WorkspaceID.Bytes),
		actorID:                 row.SessionID,
		status:                  row.Status,
		currentAttemptNumber:    row.CurrentAttemptNumber,
		currentRunLeaseID:       row.CurrentRunLeaseID,
		stateVersion:            row.StateVersion,
		runtimePreparationCount: row.RuntimePreparationCount,
	}, nil
}

func discoverOwnedCancellationRuns(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
	targetID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).ListOwnedCancellationRuns(ctx, db.ListOwnedCancellationRunsParams{
		TargetID:      pgvalue.UUID(targetID),
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		MaxDepth:      maxCancellationGraphSize + 1,
		LimitCount:    maxCancellationGraphSize + 1,
	})
	if err != nil {
		return nil, cancellationAuthority("discover parent-owned Runs", err)
	}
	var ids []uuid.UUID
	for _, row := range rows {
		if row.Cycle {
			return nil, cancellationAuthority("parent-owned run graph contains a cycle", nil)
		}
		ids = append(ids, uuid.UUID(row.ID.Bytes))
	}
	if len(ids) == 0 || ids[0] != targetID {
		return nil, cancellationAuthority("parent-owned run graph is incomplete", nil)
	}
	if len(ids) > maxCancellationGraphSize {
		return nil, cancellationAuthority("run cancellation graph exceeds the transaction bound", nil)
	}
	return ids, nil
}

func lockCancellationResources(
	ctx context.Context,
	tx pgx.Tx,
	lockOrder []uuid.UUID,
	cancelRuns []cancellationRun,
	beforeRuntime func() error,
) (map[uuid.UUID]cancellationWait, error) {
	cancelIDs := make([]uuid.UUID, 0, len(cancelRuns))
	for _, run := range cancelRuns {
		cancelIDs = append(cancelIDs, run.id)
	}
	if err := lockCancellationWorkspaces(ctx, tx, lockOrder); err != nil {
		return nil, err
	}
	if err := lockCancellationAttempts(ctx, tx, lockOrder); err != nil {
		return nil, err
	}
	waitsByChild, err := lockCancellationWaits(ctx, tx, lockOrder, cancelIDs)
	if err != nil {
		return nil, err
	}
	if err := lockCancellationCheckpoints(ctx, tx, lockOrder); err != nil {
		return nil, err
	}
	if beforeRuntime != nil {
		if err := beforeRuntime(); err != nil {
			return nil, err
		}
	}
	runtimeIDs, err := lockCancellationRuntimes(ctx, tx, lockOrder, cancelIDs)
	if err != nil {
		return nil, err
	}
	runLeaseIDs, err := lockCancellationRunLeases(ctx, tx, cancelIDs)
	if err != nil {
		return nil, err
	}
	if err := lockCancellationMounts(ctx, tx, runtimeIDs); err != nil {
		return nil, err
	}
	if err := lockCancellationWorkspaceLeases(ctx, tx, runLeaseIDs); err != nil {
		return nil, err
	}
	return waitsByChild, nil
}

func lockCancellationWorkspaces(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) error {
	_, err := db.New(tx).LockCancellationWorkspaces(ctx, pgUUIDs(runIDs))
	if err != nil {
		return cancellationAuthority("lock cancellation workspaces", err)
	}
	return nil
}

func lockCancellationAttempts(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) error {
	_, err := db.New(tx).LockCancellationAttempts(ctx, pgUUIDs(runIDs))
	if err != nil {
		return cancellationAuthority("lock cancellation attempts", err)
	}
	return nil
}

func lockCancellationRuntimes(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
	cancelIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).LockCancellationRuntimes(ctx, db.LockCancellationRuntimesParams{
		RunIDs:    pgUUIDs(runIDs),
		CancelIDs: pgUUIDs(cancelIDs),
	})
	if err != nil {
		return nil, cancellationAuthority("lock cancellation Runtimes", err)
	}
	return cancellationIDs(rows), nil
}

func lockCancellationRunLeases(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).LockCancellationRunLeases(ctx, pgUUIDs(runIDs))
	if err != nil {
		return nil, cancellationAuthority("lock cancellation run leases", err)
	}
	return cancellationIDs(rows), nil
}

func lockCancellationMounts(
	ctx context.Context,
	tx pgx.Tx,
	runtimeIDs []uuid.UUID,
) error {
	if len(runtimeIDs) == 0 {
		return nil
	}
	_, err := db.New(tx).LockCancellationMounts(ctx, pgUUIDs(runtimeIDs))
	if err != nil {
		return cancellationAuthority("lock cancellation Mounts", err)
	}
	return nil
}

func lockCancellationWorkspaceLeases(
	ctx context.Context,
	tx pgx.Tx,
	runLeaseIDs []uuid.UUID,
) error {
	if len(runLeaseIDs) == 0 {
		return nil
	}
	_, err := db.New(tx).LockCancellationWorkspaceLeases(ctx, pgUUIDs(runLeaseIDs))
	if err != nil {
		return cancellationAuthority("lock cancellation workspace leases", err)
	}
	return nil
}

func lockCancellationWaits(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
	cancelIDs []uuid.UUID,
) (map[uuid.UUID]cancellationWait, error) {
	rows, err := db.New(tx).LockCancellationWaits(ctx, db.LockCancellationWaitsParams{
		RunIDs:    pgUUIDs(runIDs),
		CancelIDs: pgUUIDs(cancelIDs),
	})
	if err != nil {
		return nil, cancellationAuthority("lock cancellation waits", err)
	}
	waitsByChild := make(map[uuid.UUID]cancellationWait)
	for _, row := range rows {
		wait := cancellationWait{
			id:                       uuid.UUID(row.ID.Bytes),
			runID:                    uuid.UUID(row.RunID.Bytes),
			workspaceID:              uuid.UUID(row.WorkspaceID.Bytes),
			childRunID:               row.ChildRunID,
			conditionState:           row.ConditionState,
			suspensionState:          row.SuspensionState,
			expectedRunStateVersion:  row.ExpectedRunStateVersion,
			attemptNumber:            row.AttemptNumber,
			currentRunLeaseID:        row.CurrentRunLeaseID,
			priorRunLeaseID:          row.PriorRunLeaseID,
			suspendCheckpointID:      row.SuspendCheckpointID,
			handoffRuntimeInstanceID: row.HandoffRuntimeInstanceID,
			handoffWorkspaceMountID:  row.HandoffWorkspaceMountID,
			baseWorkspaceVersionID:   row.BaseWorkspaceVersionID,
			childWriterGeneration:    row.ChildWriterGeneration,
		}
		if wait.childRunID.Valid {
			childID := uuid.UUID(wait.childRunID.Bytes)
			if _, duplicate := waitsByChild[childID]; duplicate {
				return nil, cancellationAuthority(
					"multiple active parent waits name one child run",
					nil,
				)
			}
			waitsByChild[childID] = wait
		}
	}
	return waitsByChild, nil
}

func lockCancellationCheckpoints(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) error {
	_, err := db.New(tx).LockCancellationCheckpoints(ctx, pgUUIDs(runIDs))
	if err != nil {
		return cancellationAuthority("lock cancellation Checkpoints", err)
	}
	return nil
}

func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	values := make([]pgtype.UUID, len(ids))
	for index, id := range ids {
		values[index] = pgvalue.UUID(id)
	}
	return values
}

func cancellationIDs(values []pgtype.UUID) []uuid.UUID {
	ids := make([]uuid.UUID, len(values))
	for index, value := range values {
		ids[index] = uuid.UUID(value.Bytes)
	}
	return ids
}

func retainedCancellationHandoff(
	parent cancellationRun,
	child cancellationRun,
	wait cancellationWait,
) (pgtype.UUID, pgtype.UUID, error) {
	if wait.runID != parent.id ||
		!wait.childRunID.Valid ||
		uuid.UUID(wait.childRunID.Bytes) != child.id {
		return pgtype.UUID{}, pgtype.UUID{}, cancellationAuthority(
			"cancelled child wait relation does not match",
			nil,
		)
	}
	if wait.workspaceID != parent.workspaceID {
		return pgtype.UUID{}, pgtype.UUID{}, cancellationAuthority(
			"cancelled child wait workspace does not match parent",
			nil,
		)
	}
	if !wait.handoffRuntimeInstanceID.Valid && !wait.handoffWorkspaceMountID.Valid {
		return pgtype.UUID{}, pgtype.UUID{}, nil
	}
	if parent.workspaceID != child.workspaceID ||
		!wait.handoffRuntimeInstanceID.Valid ||
		!wait.handoffWorkspaceMountID.Valid {
		return pgtype.UUID{}, pgtype.UUID{}, cancellationAuthority(
			"cancelled child handoff reservation is inconsistent",
			nil,
		)
	}
	return wait.handoffRuntimeInstanceID, wait.handoffWorkspaceMountID, nil
}

func cancelLockedRun(
	ctx context.Context,
	tx pgx.Tx,
	run cancellationRun,
	preservedRuntimeID pgtype.UUID,
	preservedMountID pgtype.UUID,
) error {
	return terminateLockedRun(
		ctx,
		tx,
		run,
		preservedRuntimeID,
		preservedMountID,
		cancelledTermination,
	)
}

func terminateLockedRun(
	ctx context.Context,
	tx pgx.Tx,
	run cancellationRun,
	preservedRuntimeID pgtype.UUID,
	preservedMountID pgtype.UUID,
	termination termination,
) error {
	if runStatusTerminal(run.status) {
		return nil
	}
	errorPayload, err := json.Marshal(map[string]any{
		"code":      termination.errorCode,
		"message":   termination.errorMessage,
		"retryable": false,
	})
	if err != nil {
		return err
	}
	failure, err := MarshalFailure(termination.errorCode, termination.errorMessage, nil)
	if err != nil {
		return err
	}
	queries := db.New(tx)
	if run.actorID.Valid {
		var affected int64
		if termination.actorCancellation {
			affected, err = queries.DetachActorFromCancelledRun(
				ctx,
				db.DetachActorFromCancelledRunParams{
					SessionID:   run.actorID,
					WorkspaceID: pgvalue.UUID(run.workspaceID),
					RunID:       pgvalue.UUID(run.id),
				},
			)
		} else {
			sessionFailure, marshalErr := MarshalFailure(
				termination.actorFailureCode,
				"Session failed because its Run terminated",
				map[string]any{"run_id": run.id.String()},
			)
			if marshalErr != nil {
				return marshalErr
			}
			affected, err = queries.FailActorForRunTermination(
				ctx,
				db.FailActorForRunTerminationParams{
					Failure:     sessionFailure,
					RunID:       pgvalue.UUID(run.id),
					SessionID:   run.actorID,
					WorkspaceID: pgvalue.UUID(run.workspaceID),
				},
			)
		}
		if err != nil || affected != 1 {
			return cancellationAuthority("terminalize owning actor", err)
		}
	}
	if err := queries.TerminalizeRunSuspensions(
		ctx,
		db.TerminalizeRunSuspensionsParams{
			ConditionState:  termination.waitCondition,
			ErrorPayload:    errorPayload,
			ReasonCode:      termination.reasonCode,
			SuspensionState: termination.waitSuspension,
			RunID:           pgvalue.UUID(run.id),
		},
	); err != nil {
		return cancellationAuthority("terminalize run suspension", err)
	}
	if err := queries.InvalidateRunCheckpoints(
		ctx,
		db.InvalidateRunCheckpointsParams{
			ReasonCode: termination.reasonCode,
			RunID:      pgvalue.UUID(run.id),
		},
	); err != nil {
		return cancellationAuthority("invalidate run checkpoints", err)
	}
	if run.currentRunLeaseID.Valid {
		affected, err := queries.FenceRunWorkspaceLease(
			ctx,
			db.FenceRunWorkspaceLeaseParams{
				ReasonCode:   termination.reasonCode,
				ErrorPayload: errorPayload,
				RunLeaseID:   run.currentRunLeaseID,
			},
		)
		if err != nil {
			return cancellationAuthority("fence run workspace lease", err)
		}
		if affected > 1 {
			return cancellationAuthority("multiple run workspace leases were active", nil)
		}
		affected, err = queries.TerminalizeRunLease(
			ctx,
			db.TerminalizeRunLeaseParams{
				State:        termination.runLeaseState,
				ReasonCode:   termination.reasonCode,
				ErrorPayload: errorPayload,
				ID:           run.currentRunLeaseID,
				RunID:        pgvalue.UUID(run.id),
			},
		)
		if err != nil || affected != 1 {
			return cancellationAuthority("terminalize current run lease", err)
		}
	}
	mountFinalizationKind, mountFinalizationReasonCode :=
		runtimeCleanupMountFinalization(termination.reasonCode, termination.discardRuntime)
	if err := queries.CloseRunRuntimes(
		ctx,
		db.CloseRunRuntimesParams{
			RetainedMountID:             preservedMountID,
			RunLeaseID:                  run.currentRunLeaseID,
			RunID:                       pgvalue.UUID(run.id),
			RetainedRuntimeID:           preservedRuntimeID,
			ReasonCode:                  termination.reasonCode,
			MountFinalizationKind:       mountFinalizationKind,
			MountFinalizationReasonCode: mountFinalizationReasonCode,
		},
	); err != nil {
		return cancellationAuthority("request terminal run runtime cleanup", err)
	}
	affected, err := queries.TerminalizeRunAttempt(
		ctx,
		db.TerminalizeRunAttemptParams{
			Outcome:       termination.attemptOutcome,
			ReasonCode:    termination.reasonCode,
			ErrorPayload:  errorPayload,
			RunID:         pgvalue.UUID(run.id),
			AttemptNumber: run.currentAttemptNumber,
		},
	)
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize current run attempt", err)
	}
	affected, err = queries.TerminalizeRun(
		ctx,
		db.TerminalizeRunParams{
			Status:               termination.runStatus,
			Failure:              failure,
			ID:                   pgvalue.UUID(run.id),
			ExpectedStateVersion: run.stateVersion,
		},
	)
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize run", err)
	}
	if !run.actorID.Valid {
		if err := queries.ReleaseTaskWorkspace(
			ctx,
			db.ReleaseTaskWorkspaceParams{
				WorkspaceID: pgvalue.UUID(run.workspaceID),
				RunID:       pgvalue.UUID(run.id),
			},
		); err != nil {
			return cancellationAuthority("release terminal task workspace", err)
		}
	} else if !termination.actorCancellation {
		if err := queries.ReleaseActorWorkspace(
			ctx,
			db.ReleaseActorWorkspaceParams{
				WorkspaceID: pgvalue.UUID(run.workspaceID),
				SessionID:   run.actorID,
			},
		); err != nil {
			return cancellationAuthority("release terminal actor workspace", err)
		}
	}
	if err := queries.RecordRunTerminalEvent(
		ctx,
		db.RecordRunTerminalEventParams{
			RunLeaseID: run.currentRunLeaseID,
			Kind:       termination.eventKind,
			Message:    termination.eventMessage,
			ReasonCode: termination.reasonCode,
			RunID:      pgvalue.UUID(run.id),
		},
	); err != nil {
		return cancellationAuthority("record run terminal event", err)
	}
	return nil
}

func runtimeCleanupMountFinalization(reasonCode string, discardRuntime bool) (pgtype.Text, pgtype.Text) {
	if !discardRuntime {
		return pgtype.Text{}, pgtype.Text{}
	}
	return pgvalue.Text("discard"), pgvalue.Text(reasonCode)
}

func resolveCancelledChildWait(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	child cancellationRun,
	wait cancellationWait,
) error {
	if wait.conditionState != db.WaitStatePending ||
		parent.status != db.RunStatusWaiting ||
		parent.currentAttemptNumber != wait.attemptNumber ||
		parent.stateVersion != wait.expectedRunStateVersion {
		return cancellationAuthority("cancelled child wait fence does not match", nil)
	}
	if !wait.handoffRuntimeInstanceID.Valid {
		return resolveCancelledDifferentWorkspaceChildWait(ctx, tx, parent, child, wait)
	}
	if !wait.baseWorkspaceVersionID.Valid {
		return cancellationAuthority("cancelled handoff child has no base workspace version", nil)
	}
	reasonCode := "child_run_cancelled"
	return resolveTerminalChildWait(
		ctx,
		tx,
		parent,
		wait,
		terminalChildWaitResolution{
			conditionState:           db.WaitStateCancelled,
			reasonCode:               &reasonCode,
			conditionError:           json.RawMessage(`{"code":"child_run_cancelled","message":"Child Run was cancelled","retryable":false}`),
			resumeWorkspaceVersionID: wait.baseWorkspaceVersionID,
		},
	)
}

func resolveCancelledDifferentWorkspaceChildWait(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	child cancellationRun,
	wait cancellationWait,
) error {
	result, err := marshalChildFailureResult(
		child.id,
		"child_run_cancelled",
		"Child Run was cancelled",
	)
	if err != nil {
		return err
	}
	return resolveDifferentWorkspaceChildWait(ctx, tx, parent, wait, result)
}

func resolveDifferentWorkspaceChildWait(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	wait cancellationWait,
	result json.RawMessage,
) error {
	return resolveTerminalChildWait(
		ctx,
		tx,
		parent,
		wait,
		terminalChildWaitResolution{
			conditionState: db.WaitStateCompleted,
			result:         result,
		},
	)
}

func resolveTerminalChildWait(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	wait cancellationWait,
	resolution terminalChildWaitResolution,
) error {
	if resolution.resumeWorkspaceVersionID.Valid &&
		wait.suspensionState != db.RunWaitStateParked {
		return cancellationAuthority("terminal child workspace handoff is not parked", nil)
	}
	queries := db.New(tx)
	switch wait.suspensionState {
	case db.RunWaitStateHot:
		if !wait.currentRunLeaseID.Valid ||
			!parent.currentRunLeaseID.Valid ||
			wait.currentRunLeaseID != parent.currentRunLeaseID {
			return cancellationAuthority("hot terminal child wait lease does not match", nil)
		}
		if _, err := queries.ResolveHotTerminalChildWait(
			ctx,
			db.ResolveHotTerminalChildWaitParams{
				ConditionState:          string(resolution.conditionState),
				ConditionResult:         resolution.result,
				ConditionError:          resolution.conditionError,
				ReasonCode:              pgvalue.TextPtr(resolution.reasonCode),
				WaitID:                  pgvalue.UUID(wait.id),
				RunID:                   pgvalue.UUID(parent.id),
				ExpectedRunStateVersion: parent.stateVersion,
				AttemptNumber:           parent.currentAttemptNumber,
				CurrentRunLeaseID:       parent.currentRunLeaseID,
			},
		); err != nil {
			return cancellationAuthority("resolve hot terminal child wait", err)
		}
	case db.RunWaitStateCheckpointing:
		if _, err := queries.ResolveCheckpointingTerminalChildWait(
			ctx,
			db.ResolveCheckpointingTerminalChildWaitParams{
				ConditionState:  string(resolution.conditionState),
				ConditionResult: resolution.result,
				ConditionError:  resolution.conditionError,
				ReasonCode:      pgvalue.TextPtr(resolution.reasonCode),
				WaitID:          pgvalue.UUID(wait.id),
				RunID:           pgvalue.UUID(parent.id),
			},
		); err != nil {
			return cancellationAuthority("resolve checkpointing terminal child wait", err)
		}
	case db.RunWaitStateParked:
		if !wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			parent.currentRunLeaseID.Valid {
			return cancellationAuthority("parked terminal child wait fence does not match", nil)
		}
		_, err := queries.ResolveParkedTerminalChildWait(
			ctx,
			db.ResolveParkedTerminalChildWaitParams{
				ConditionState:             string(resolution.conditionState),
				ConditionResult:            resolution.result,
				ConditionError:             resolution.conditionError,
				ReasonCode:                 pgvalue.TextPtr(resolution.reasonCode),
				ResolvedWorkspaceVersionID: resolution.resumeWorkspaceVersionID,
				WaitID:                     pgvalue.UUID(wait.id),
				RunID:                      pgvalue.UUID(parent.id),
				ExpectedRunStateVersion:    parent.stateVersion,
				AttemptNumber:              parent.currentAttemptNumber,
			},
		)
		if err != nil {
			return cancellationAuthority("resolve parked terminal child wait", err)
		}
	default:
		return cancellationAuthority("terminal child wait suspension is ineligible", nil)
	}
	return nil
}

func runStatusTerminal(status db.RunStatus) bool {
	switch status {
	case db.RunStatusSucceeded, db.RunStatusFailed, db.RunStatusCancelled,
		db.RunStatusExpired, db.RunStatusSystemFailed:
		return true
	default:
		return false
	}
}

func cancellationAuthority(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrCancellationAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrCancellationAuthority, operation, cause)
}
