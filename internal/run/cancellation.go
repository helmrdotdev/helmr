package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

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
	RunPublicID   string
}

type CancellationResult struct {
	RunID         uuid.UUID
	RunPublicID   string
	Changed       bool
	CancelledRuns int
}

type Canceler struct {
	db CancellationDB
}

type cancellationRun struct {
	id                   uuid.UUID
	publicID             string
	parentRunID          pgtype.UUID
	parentOwnsLifecycle  pgtype.Bool
	environmentID        uuid.UUID
	workspaceID          uuid.UUID
	actorID              pgtype.UUID
	status               db.RunStatus
	currentAttemptNumber int32
	currentRunLeaseID    pgtype.UUID
	stateVersion         int64
	depth                int
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
	resumeRequestVersion     int64
	handoffRuntimeInstanceID pgtype.UUID
	handoffWorkspaceMountID  pgtype.UUID
	baseWorkspaceVersionID   pgtype.UUID
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
	if tx == nil || request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil || request.RunID == uuid.Nil {
		return OwnedFinalization{}, errors.New("owned Run finalization graph authority is required")
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
			"owned Run finalization graph exceeds the transaction bound",
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
				"lock owned Run finalization graph",
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
			"owned Run finalization graph changed during lock acquisition",
			nil,
		)
	}
	descendants := make([]cancellationRun, 0, len(descendantIDs))
	for depth, id := range descendantIDs {
		run, found := locked[id]
		if !found {
			return OwnedFinalization{}, cancellationAuthority(
				"owned Run finalization descendant was not locked",
				nil,
			)
		}
		run.depth = depth
		descendants = append(descendants, run)
	}
	waitsByChild, err := lockCancellationResources(
		ctx, tx, lockOrder, descendants,
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
		return 0, errors.New("owned Run finalization graph is invalid")
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
					"Secret-revoked Run retained an active same-Workspace parent",
					nil,
				)
			}
			wait, found := g.waitsByChild[target.id]
			if !found || wait.handoffRuntimeInstanceID.Valid ||
				wait.handoffWorkspaceMountID.Valid {
				return 0, cancellationAuthority(
					"Secret-revoked child Wait boundary is inconsistent",
					nil,
				)
			}
			result, err := json.Marshal(map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "secret_revoked",
					"message":   "A Workspace Secret used by the child Run was revoked",
					"retryable": false,
				},
				"run": map[string]any{"id": target.publicID},
			})
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

func (c *Canceler) Cancel(
	ctx context.Context,
	request CancellationRequest,
) (CancellationResult, error) {
	if request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil ||
		strings.TrimSpace(request.RunPublicID) == "" {
		return CancellationResult{}, errors.New("run cancellation scope and public ID are required")
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return CancellationResult{}, fmt.Errorf("begin Run cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	targetID, err := findCancellationTarget(ctx, tx, request)
	if errors.Is(err, pgx.ErrNoRows) {
		return CancellationResult{}, ErrCancellationNotFound
	}
	if err != nil {
		return CancellationResult{}, cancellationAuthority("resolve target Run", err)
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
			return CancellationResult{}, cancellationAuthority("lock Run graph", err)
		}
		locked[id] = run
	}
	target, ok := locked[targetID]
	if !ok {
		return CancellationResult{}, cancellationAuthority("target Run was not locked", nil)
	}
	result := CancellationResult{RunID: target.id, RunPublicID: target.publicID}
	if runStatusTerminal(target.status) {
		if target.status != db.RunStatusCancelled {
			return CancellationResult{}, ErrCancellationConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return CancellationResult{}, fmt.Errorf("commit Run cancellation replay: %w", err)
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
				"parent-owned Run was not locked",
				nil,
			)
		}
		run.depth = depth
		cancelled[id] = struct{}{}
		runs = append(runs, run)
	}
	waitsByChild, err := lockCancellationResources(ctx, tx, lockOrder, runs)
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
					"parent-owned Run parent was not locked",
					nil,
				)
			}
			boundaryWait, found = waitsByChild[target.id]
			if !found {
				return CancellationResult{}, cancellationAuthority(
					"parent-owned Run Wait was not locked",
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
		return CancellationResult{}, fmt.Errorf("commit Run cancellation: %w", err)
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
		PublicID:      request.RunPublicID,
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
		return nil, cancellationAuthority("load Run lineage", err)
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
		return cancellationAuthority("lock Run lineage Actors", err)
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
		id:                   uuid.UUID(row.ID.Bytes),
		publicID:             row.PublicID,
		parentRunID:          row.ParentRunID,
		parentOwnsLifecycle:  row.ParentOwnsLifecycle,
		environmentID:        uuid.UUID(row.EnvironmentID.Bytes),
		workspaceID:          uuid.UUID(row.WorkspaceID.Bytes),
		actorID:              row.ActorID,
		status:               row.Status,
		currentAttemptNumber: row.CurrentAttemptNumber,
		currentRunLeaseID:    row.CurrentRunLeaseID,
		stateVersion:         row.StateVersion,
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
			return nil, cancellationAuthority("parent-owned Run graph contains a cycle", nil)
		}
		ids = append(ids, uuid.UUID(row.ID.Bytes))
	}
	if len(ids) == 0 || ids[0] != targetID {
		return nil, cancellationAuthority("parent-owned Run graph is incomplete", nil)
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
	waitsByChild, err := lockCancellationWaits(ctx, tx, lockOrder, cancelIDs)
	if err != nil {
		return nil, err
	}
	if err := lockCancellationCheckpoints(ctx, tx, lockOrder); err != nil {
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
		return cancellationAuthority("lock cancellation Workspaces", err)
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
		return cancellationAuthority("lock cancellation Attempts", err)
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
		return nil, cancellationAuthority("lock cancellation Run Leases", err)
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
		return cancellationAuthority("lock cancellation Workspace Leases", err)
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
		return nil, cancellationAuthority("lock cancellation Waits", err)
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
			resumeRequestVersion:     row.ResumeRequestVersion,
			handoffRuntimeInstanceID: row.HandoffRuntimeInstanceID,
			handoffWorkspaceMountID:  row.HandoffWorkspaceMountID,
			baseWorkspaceVersionID:   row.BaseWorkspaceVersionID,
		}
		if wait.childRunID.Valid {
			childID := uuid.UUID(wait.childRunID.Bytes)
			if _, duplicate := waitsByChild[childID]; duplicate {
				return nil, cancellationAuthority(
					"multiple active parent Waits name one child Run",
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
			"cancelled child Wait relation does not match",
			nil,
		)
	}
	if wait.workspaceID != parent.workspaceID {
		return pgtype.UUID{}, pgtype.UUID{}, cancellationAuthority(
			"cancelled child Wait Workspace does not match parent",
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
	queries := db.New(tx)
	if run.actorID.Valid {
		var affected int64
		if termination.actorCancellation {
			affected, err = queries.DetachActorFromCancelledRun(
				ctx,
				db.DetachActorFromCancelledRunParams{
					ActorID:     run.actorID,
					WorkspaceID: pgvalue.UUID(run.workspaceID),
					RunID:       pgvalue.UUID(run.id),
				},
			)
		} else {
			affected, err = queries.FailActorForRunTermination(
				ctx,
				db.FailActorForRunTerminationParams{
					FailureCode: termination.actorFailureCode,
					RunID:       pgvalue.UUID(run.id),
					ActorID:     run.actorID,
					WorkspaceID: pgvalue.UUID(run.workspaceID),
				},
			)
		}
		if err != nil || affected != 1 {
			return cancellationAuthority("terminalize owning Actor", err)
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
		return cancellationAuthority("terminalize Run suspension", err)
	}
	if err := queries.InvalidateRunCheckpoints(
		ctx,
		db.InvalidateRunCheckpointsParams{
			ReasonCode: termination.reasonCode,
			RunID:      pgvalue.UUID(run.id),
		},
	); err != nil {
		return cancellationAuthority("invalidate Run checkpoints", err)
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
			return cancellationAuthority("fence Run Workspace Lease", err)
		}
		if affected > 1 {
			return cancellationAuthority("multiple Run Workspace Leases were active", nil)
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
			return cancellationAuthority("terminalize current Run Lease", err)
		}
	}
	if err := queries.CloseRunRuntimes(
		ctx,
		db.CloseRunRuntimesParams{
			RetainedMountID:   preservedMountID,
			RunLeaseID:        run.currentRunLeaseID,
			RunID:             pgvalue.UUID(run.id),
			RetainedRuntimeID: preservedRuntimeID,
			ReasonCode:        termination.reasonCode,
		},
	); err != nil {
		return cancellationAuthority("request terminal Run runtime cleanup", err)
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
		return cancellationAuthority("terminalize current Run Attempt", err)
	}
	affected, err = queries.TerminalizeRun(
		ctx,
		db.TerminalizeRunParams{
			Status:               termination.runStatus,
			ReasonCode:           termination.reasonCode,
			ErrorPayload:         errorPayload,
			ID:                   pgvalue.UUID(run.id),
			ExpectedStateVersion: run.stateVersion,
		},
	)
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize Run", err)
	}
	if !run.actorID.Valid {
		if err := queries.ReleaseTaskWorkspace(
			ctx,
			db.ReleaseTaskWorkspaceParams{
				WorkspaceID: pgvalue.UUID(run.workspaceID),
				RunID:       pgvalue.UUID(run.id),
			},
		); err != nil {
			return cancellationAuthority("release terminal Task Workspace", err)
		}
	} else if !termination.actorCancellation {
		if err := queries.ReleaseActorWorkspace(
			ctx,
			db.ReleaseActorWorkspaceParams{
				WorkspaceID: pgvalue.UUID(run.workspaceID),
				ActorID:     run.actorID,
			},
		); err != nil {
			return cancellationAuthority("release terminal Actor Workspace", err)
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
		return cancellationAuthority("record Run terminal event", err)
	}
	return nil
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
		return cancellationAuthority("cancelled child Wait fence does not match", nil)
	}
	if !wait.handoffRuntimeInstanceID.Valid {
		return resolveCancelledDifferentWorkspaceChildWait(ctx, tx, parent, child, wait)
	}
	errorPayload := json.RawMessage(`{"code":"child_run_cancelled","message":"Child Run was cancelled","retryable":false}`)
	var resumeWorkspaceVersion any
	if wait.handoffRuntimeInstanceID.Valid {
		if !wait.baseWorkspaceVersionID.Valid {
			return cancellationAuthority("cancelled handoff child has no base Workspace version", nil)
		}
		resumeWorkspaceVersion = uuid.UUID(wait.baseWorkspaceVersionID.Bytes)
	}
	switch wait.suspensionState {
	case db.RunWaitStateHot:
		if !wait.currentRunLeaseID.Valid ||
			!parent.currentRunLeaseID.Valid ||
			wait.currentRunLeaseID != parent.currentRunLeaseID {
			return cancellationAuthority("hot cancelled child Wait Lease does not match", nil)
		}
		var nextVersion int64
		if err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'running',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id = $4
RETURNING state_version`,
			parent.id,
			parent.stateVersion,
			parent.currentAttemptNumber,
			uuid.UUID(parent.currentRunLeaseID.Bytes),
		).Scan(&nextVersion); err != nil {
			return cancellationAuthority("resume hot cancelled child parent", err)
		}
		command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = 'cancelled',
       condition_error = $2::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = 'child_run_cancelled',
       suspension_state = 'released',
       expected_run_state_version = $3,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND condition_state = 'pending'
   AND suspension_state = 'hot'`,
			wait.id,
			errorPayload,
			nextVersion,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("release hot cancelled child Wait", err)
		}
	case db.RunWaitStateCheckpointing:
		command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = 'cancelled',
       condition_error = $2::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = 'child_run_cancelled',
       resume_workspace_version_id = COALESCE(
           resume_workspace_version_id,
           $3::uuid
       ),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'`,
			wait.id,
			errorPayload,
			resumeWorkspaceVersion,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("complete checkpointing cancelled child Wait", err)
		}
	case db.RunWaitStateParked:
		if !wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			parent.currentRunLeaseID.Valid {
			return cancellationAuthority("parked cancelled child Wait fence does not match", nil)
		}
		var nextVersion int64
		if err := tx.QueryRow(ctx, `
	UPDATE runs
	   SET status = 'queued',
	       state_version = state_version + 1,
	       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id IS NULL
RETURNING state_version`,
			parent.id,
			parent.stateVersion,
			parent.currentAttemptNumber,
		).Scan(&nextVersion); err != nil {
			return cancellationAuthority("queue parked cancelled child parent", err)
		}
		nextResumeVersion := wait.resumeRequestVersion + 1
		command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = 'cancelled',
       condition_error = $2::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = 'child_run_cancelled',
       suspension_state = 'resume_pending',
       resume_request_version = $3,
       expected_run_state_version = $4,
       resume_workspace_version_id = COALESCE(
           resume_workspace_version_id,
           $5::uuid
       ),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND condition_state = 'pending'
   AND suspension_state = 'parked'`,
			wait.id,
			errorPayload,
			nextResumeVersion,
			nextVersion,
			resumeWorkspaceVersion,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("resume parked cancelled child Wait", err)
		}
		command, err = tx.Exec(ctx, `
INSERT INTO outbox_messages (
    id,
    lane,
    topic,
    partition_key,
    payload,
    available_at
) VALUES (
    $1,
    'control',
    'run.resume',
    $2::uuid::text,
    jsonb_build_object(
        'environmentId', $3::uuid::text,
        'runId', $4::uuid::text,
        'runWaitId', $5::uuid::text,
        'resumeRequestVersion', $6::bigint
    ),
    transaction_timestamp()
)`,
			uuid.Must(uuid.NewV7()),
			wait.workspaceID,
			parent.environmentID,
			parent.id,
			wait.id,
			nextResumeVersion,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("publish cancelled child resume intent", err)
		}
	default:
		return cancellationAuthority("cancelled child Wait is already resuming", nil)
	}
	return nil
}

func resolveCancelledDifferentWorkspaceChildWait(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	child cancellationRun,
	wait cancellationWait,
) error {
	result, err := json.Marshal(map[string]any{
		"ok": false,
		"error": map[string]any{
			"code": "child_run_cancelled", "message": "Child Run was cancelled",
			"retryable": false,
		},
		"run": map[string]any{"id": child.publicID},
	})
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
	switch wait.suspensionState {
	case db.RunWaitStateHot:
		if !wait.currentRunLeaseID.Valid || !parent.currentRunLeaseID.Valid ||
			wait.currentRunLeaseID != parent.currentRunLeaseID {
			return cancellationAuthority("hot cancelled child Wait Lease does not match", nil)
		}
		var nextVersion int64
		if err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'running', state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id = $4
RETURNING state_version`,
			parent.id, parent.stateVersion, parent.currentAttemptNumber,
			uuid.UUID(parent.currentRunLeaseID.Bytes),
		).Scan(&nextVersion); err != nil {
			return cancellationAuthority("resume hot cancelled child parent", err)
		}
		command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = $2::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released', expected_run_state_version = $3,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND condition_state = 'pending'
   AND suspension_state = 'hot'`, wait.id, result, nextVersion)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("release hot cancelled child Wait", err)
		}
	case db.RunWaitStateCheckpointing:
		command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = $2::jsonb,
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'`, wait.id, result)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("complete checkpointing cancelled child Wait", err)
		}
	case db.RunWaitStateParked:
		if !wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			parent.currentRunLeaseID.Valid {
			return cancellationAuthority("parked cancelled child Wait fence does not match", nil)
		}
		var nextVersion int64
		if err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'queued', state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id IS NULL
RETURNING state_version`,
			parent.id, parent.stateVersion, parent.currentAttemptNumber,
		).Scan(&nextVersion); err != nil {
			return cancellationAuthority("queue parked cancelled child parent", err)
		}
		nextResumeVersion := wait.resumeRequestVersion + 1
		command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = $2::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending', resume_request_version = $3,
       expected_run_state_version = $4, updated_at = transaction_timestamp()
 WHERE id = $1
   AND condition_state = 'pending'
   AND suspension_state = 'parked'`,
			wait.id, result, nextResumeVersion, nextVersion,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("resume parked cancelled child Wait", err)
		}
		command, err = tx.Exec(ctx, `
INSERT INTO outbox_messages (
    id, lane, topic, partition_key, payload, available_at
) VALUES (
    $1, 'control', 'run.resume', $2,
    jsonb_build_object(
        'environmentId', $3::text,
        'runId', $4::text,
        'runWaitId', $5::text,
        'resumeRequestVersion', $6::bigint
    ),
    transaction_timestamp()
)`,
			uuid.Must(uuid.NewV7()),
			parent.workspaceID.String(), parent.environmentID.String(),
			parent.id.String(), wait.id.String(), nextResumeVersion,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("publish cancelled child resume", err)
		}
	default:
		return cancellationAuthority("cancelled child Wait suspension is ineligible", nil)
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
