package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrRunCancellationNotFound  = errors.New("run not found")
	ErrRunCancellationConflict  = errors.New("run has another terminal outcome")
	ErrRunCancellationAuthority = errors.New("run cancellation authority is inconsistent")
)

const maxRunCancellationGraphSize = 1000

type RunCancellationDB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type RunCancellationRequest struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunPublicID   string
}

type RunCancellationResult struct {
	RunID         uuid.UUID
	RunPublicID   string
	Changed       bool
	CancelledRuns int
}

type RunCanceller struct {
	db RunCancellationDB
}

type cancellationRun struct {
	id                   uuid.UUID
	publicID             string
	parentRunID          pgtype.UUID
	parentOwnsLifecycle  pgtype.Bool
	environmentID        uuid.UUID
	workspaceID          uuid.UUID
	actorID              pgtype.UUID
	status               RunStatus
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
	conditionState           WaitState
	suspensionState          RunWaitState
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

func NewRunCanceller(database RunCancellationDB) (*RunCanceller, error) {
	if database == nil {
		return nil, errors.New("run cancellation database is required")
	}
	return &RunCanceller{db: database}, nil
}

type OwnedRunFinalizationGraphRequest struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
}

type OwnedRunFinalizationGraph struct {
	tx          pgx.Tx
	currentRun  uuid.UUID
	descendants []cancellationRun
}

// LockOwnedRunFinalizationGraphInTransaction acquires the global cancellation
// lock order before ordinary Run authority is locked. It lets terminal
// finalization re-lock its exact authority without later reaching from a
// Workspace lock back to an unlocked descendant Run.
func LockOwnedRunFinalizationGraphInTransaction(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedRunFinalizationGraphRequest,
) (OwnedRunFinalizationGraph, error) {
	if tx == nil || request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil || request.RunID == uuid.Nil {
		return OwnedRunFinalizationGraph{}, errors.New("owned Run finalization graph authority is required")
	}
	scope := RunCancellationRequest{
		OrgID: request.OrgID, ProjectID: request.ProjectID,
		EnvironmentID: request.EnvironmentID,
	}
	lineage, err := cancellationLineage(ctx, tx, request.RunID)
	if err != nil {
		return OwnedRunFinalizationGraph{}, err
	}
	descendantIDs, err := discoverOwnedCancellationRuns(
		ctx, tx, scope, request.RunID,
	)
	if err != nil {
		return OwnedRunFinalizationGraph{}, err
	}
	lockOrder := append(slices.Clone(lineage), descendantIDs[1:]...)
	if len(lockOrder) > maxRunCancellationGraphSize {
		return OwnedRunFinalizationGraph{}, cancellationAuthority(
			"owned Run finalization graph exceeds the transaction bound",
			nil,
		)
	}
	if err := lockCancellationActors(ctx, tx, scope, lockOrder); err != nil {
		return OwnedRunFinalizationGraph{}, err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lockOrder))
	for _, id := range lockOrder {
		run, err := lockCancellationRun(ctx, tx, scope, id)
		if err != nil {
			return OwnedRunFinalizationGraph{}, cancellationAuthority(
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
		return OwnedRunFinalizationGraph{}, err
	}
	if !slices.Equal(descendantIDs, reloaded) {
		return OwnedRunFinalizationGraph{}, cancellationAuthority(
			"owned Run finalization graph changed during lock acquisition",
			nil,
		)
	}
	descendants := make([]cancellationRun, 0, len(descendantIDs))
	for depth, id := range descendantIDs {
		run, found := locked[id]
		if !found {
			return OwnedRunFinalizationGraph{}, cancellationAuthority(
				"owned Run finalization descendant was not locked",
				nil,
			)
		}
		run.depth = depth
		descendants = append(descendants, run)
	}
	if _, err := lockCancellationResources(
		ctx, tx, lockOrder, descendants,
	); err != nil {
		return OwnedRunFinalizationGraph{}, err
	}
	return OwnedRunFinalizationGraph{
		tx: tx, currentRun: request.RunID, descendants: descendants,
	}, nil
}

// CancelDescendants terminalizes all still-active owned descendants without
// resolving the current Run's boundary Wait. The current Run is terminalized
// by the caller in the same transaction.
func (g OwnedRunFinalizationGraph) CancelDescendants(ctx context.Context) (int, error) {
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

func (c *RunCanceller) Cancel(
	ctx context.Context,
	request RunCancellationRequest,
) (RunCancellationResult, error) {
	if request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil ||
		strings.TrimSpace(request.RunPublicID) == "" {
		return RunCancellationResult{}, errors.New("run cancellation scope and public ID are required")
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return RunCancellationResult{}, fmt.Errorf("begin Run cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	targetID, err := findCancellationTarget(ctx, tx, request)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCancellationResult{}, ErrRunCancellationNotFound
	}
	if err != nil {
		return RunCancellationResult{}, cancellationAuthority("resolve target Run", err)
	}
	lineage, err := cancellationLineage(ctx, tx, targetID)
	if err != nil {
		return RunCancellationResult{}, err
	}
	descendants, err := discoverOwnedCancellationRuns(ctx, tx, request, targetID)
	if err != nil {
		return RunCancellationResult{}, err
	}
	lockOrder := append(slices.Clone(lineage), descendants[1:]...)
	if len(lockOrder) > maxRunCancellationGraphSize {
		return RunCancellationResult{}, cancellationAuthority(
			"run cancellation graph exceeds the transaction bound",
			nil,
		)
	}
	if err := lockCancellationActors(ctx, tx, request, lockOrder); err != nil {
		return RunCancellationResult{}, err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lockOrder))
	for _, id := range lockOrder {
		run, err := lockCancellationRun(ctx, tx, request, id)
		if err != nil {
			return RunCancellationResult{}, cancellationAuthority("lock Run graph", err)
		}
		locked[id] = run
	}
	target, ok := locked[targetID]
	if !ok {
		return RunCancellationResult{}, cancellationAuthority("target Run was not locked", nil)
	}
	result := RunCancellationResult{RunID: target.id, RunPublicID: target.publicID}
	if runStatusTerminal(target.status) {
		if target.status != RunStatusCancelled {
			return RunCancellationResult{}, ErrRunCancellationConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return RunCancellationResult{}, fmt.Errorf("commit Run cancellation replay: %w", err)
		}
		return result, nil
	}

	reloaded, err := discoverOwnedCancellationRuns(ctx, tx, request, targetID)
	if err != nil {
		return RunCancellationResult{}, err
	}
	if !slices.Equal(descendants, reloaded) {
		return RunCancellationResult{}, cancellationAuthority(
			"run cancellation graph changed during lock acquisition",
			nil,
		)
	}
	cancelled := make(map[uuid.UUID]struct{}, len(descendants))
	runs := make([]cancellationRun, 0, len(descendants))
	for depth, id := range descendants {
		run, found := locked[id]
		if !found {
			return RunCancellationResult{}, cancellationAuthority(
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
		return RunCancellationResult{}, err
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
				return RunCancellationResult{}, cancellationAuthority(
					"parent-owned Run parent was not locked",
					nil,
				)
			}
			boundaryWait, found = waitsByChild[target.id]
			if !found {
				return RunCancellationResult{}, cancellationAuthority(
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
				return RunCancellationResult{}, err
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
			return RunCancellationResult{}, err
		}
		if resolveBoundaryParent && run.id == target.id {
			if err := resolveCancelledChildWait(
				ctx,
				tx,
				boundaryParent,
				run,
				boundaryWait,
			); err != nil {
				return RunCancellationResult{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RunCancellationResult{}, fmt.Errorf("commit Run cancellation: %w", err)
	}
	result.Changed = true
	result.CancelledRuns = len(runs)
	return result, nil
}

func findCancellationTarget(
	ctx context.Context,
	tx pgx.Tx,
	request RunCancellationRequest,
) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
SELECT id
  FROM runs
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND public_id = $4`,
		request.OrgID,
		request.ProjectID,
		request.EnvironmentID,
		request.RunPublicID,
	).Scan(&id)
	return id, err
}

func cancellationLineage(
	ctx context.Context,
	tx pgx.Tx,
	targetID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE lineage AS (
    SELECT id,
           parent_run_id,
           parent_owns_lifecycle,
           0 AS depth,
           ARRAY[id] AS path,
           false AS cycle
      FROM runs
     WHERE id = $1
    UNION ALL
    SELECT parent.id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           lineage.depth + 1,
           lineage.path || parent.id,
           parent.id = ANY(lineage.path)
      FROM lineage
      JOIN runs AS parent
        ON parent.id = lineage.parent_run_id
     WHERE lineage.parent_owns_lifecycle IS TRUE
       AND NOT lineage.cycle
       AND lineage.depth < $2
)
SELECT id, depth, cycle
  FROM lineage
 ORDER BY depth DESC`, targetID, maxRunCancellationGraphSize)
	if err != nil {
		return nil, cancellationAuthority("load Run lineage", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var depth int
		var cycle bool
		if err := rows.Scan(&id, &depth, &cycle); err != nil {
			return nil, cancellationAuthority("scan Run lineage", err)
		}
		if cycle {
			return nil, cancellationAuthority("run lineage contains a cycle", nil)
		}
		ids = append(ids, id)
		if len(ids) > maxRunCancellationGraphSize {
			return nil, cancellationAuthority("run lineage exceeds the transaction bound", nil)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, cancellationAuthority("read Run lineage", err)
	}
	if len(ids) == 0 || ids[len(ids)-1] != targetID {
		return nil, cancellationAuthority("run lineage is incomplete", nil)
	}
	return ids, nil
}

func lockCancellationActors(
	ctx context.Context,
	tx pgx.Tx,
	request RunCancellationRequest,
	lineage []uuid.UUID,
) error {
	rows, err := tx.Query(ctx, `
SELECT actors.id
  FROM runs
  JOIN actors
    ON actors.id = runs.actor_id
   AND actors.environment_id = runs.environment_id
 WHERE runs.id = ANY($1::uuid[])
   AND runs.org_id = $2
   AND runs.project_id = $3
   AND runs.environment_id = $4
 ORDER BY actors.id
 FOR UPDATE OF actors`,
		lineage,
		request.OrgID,
		request.ProjectID,
		request.EnvironmentID,
	)
	if err != nil {
		return cancellationAuthority("lock Run lineage Actors", err)
	}
	defer rows.Close()
	for rows.Next() {
		var actorID uuid.UUID
		if err := rows.Scan(&actorID); err != nil {
			return cancellationAuthority("scan Run lineage Actor", err)
		}
	}
	if err := rows.Err(); err != nil {
		return cancellationAuthority("read Run lineage Actors", err)
	}
	return nil
}

func lockCancellationRun(
	ctx context.Context,
	tx pgx.Tx,
	request RunCancellationRequest,
	id uuid.UUID,
) (cancellationRun, error) {
	var run cancellationRun
	err := tx.QueryRow(ctx, `
SELECT id,
       public_id,
       parent_run_id,
       parent_owns_lifecycle,
       environment_id,
       workspace_id,
       actor_id,
       status,
       current_attempt_number,
       current_run_lease_id,
       state_version
  FROM runs
 WHERE id = $1
   AND org_id = $2
   AND project_id = $3
   AND environment_id = $4
 FOR UPDATE`,
		id,
		request.OrgID,
		request.ProjectID,
		request.EnvironmentID,
	).Scan(
		&run.id,
		&run.publicID,
		&run.parentRunID,
		&run.parentOwnsLifecycle,
		&run.environmentID,
		&run.workspaceID,
		&run.actorID,
		&run.status,
		&run.currentAttemptNumber,
		&run.currentRunLeaseID,
		&run.stateVersion,
	)
	return run, err
}

func discoverOwnedCancellationRuns(
	ctx context.Context,
	tx pgx.Tx,
	request RunCancellationRequest,
	targetID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE owned AS (
    SELECT id,
           0 AS depth,
           ARRAY[id] AS path,
           false AS cycle
      FROM runs
     WHERE id = $1
       AND org_id = $2
       AND project_id = $3
       AND environment_id = $4
    UNION ALL
    SELECT child.id,
           owned.depth + 1,
           owned.path || child.id,
           child.id = ANY(owned.path)
      FROM owned
      JOIN runs AS child
        ON child.parent_run_id = owned.id
       AND child.parent_owns_lifecycle IS TRUE
       AND child.org_id = $2
       AND child.project_id = $3
       AND child.environment_id = $4
       AND child.status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')
     WHERE NOT owned.cycle
       AND owned.depth < $5
)
SELECT id, depth, cycle
  FROM owned
 ORDER BY depth, id
 LIMIT $6`,
		targetID,
		request.OrgID,
		request.ProjectID,
		request.EnvironmentID,
		maxRunCancellationGraphSize+1,
		maxRunCancellationGraphSize+1,
	)
	if err != nil {
		return nil, cancellationAuthority("discover parent-owned Runs", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var depth int
		var cycle bool
		if err := rows.Scan(&id, &depth, &cycle); err != nil {
			return nil, cancellationAuthority("scan parent-owned Run", err)
		}
		if cycle {
			return nil, cancellationAuthority("parent-owned Run graph contains a cycle", nil)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, cancellationAuthority("read parent-owned Runs", err)
	}
	if len(ids) == 0 || ids[0] != targetID {
		return nil, cancellationAuthority("parent-owned Run graph is incomplete", nil)
	}
	if len(ids) > maxRunCancellationGraphSize {
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
	rows, err := tx.Query(ctx, `
SELECT id
  FROM workspaces
 WHERE id IN (
       SELECT workspace_id
         FROM runs
        WHERE id = ANY($1::uuid[])
 )
 ORDER BY id
 FOR UPDATE`, runIDs)
	return drainCancellationIDs(rows, err, "lock cancellation Workspaces")
}

func lockCancellationAttempts(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) error {
	rows, err := tx.Query(ctx, `
SELECT run_attempts.run_id
  FROM run_attempts
  JOIN runs
    ON runs.id = run_attempts.run_id
   AND runs.current_attempt_number = run_attempts.number
   AND runs.workspace_id = run_attempts.workspace_id
 WHERE runs.id = ANY($1::uuid[])
 ORDER BY array_position($1::uuid[], run_attempts.run_id), run_attempts.number
 FOR UPDATE OF run_attempts`, runIDs)
	return drainCancellationIDs(rows, err, "lock cancellation Attempts")
}

func lockCancellationRuntimes(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
	cancelIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
WITH target_runtimes AS (
    SELECT run_leases.runtime_instance_id AS id
      FROM runs
      JOIN run_leases
        ON run_leases.id = runs.current_run_lease_id
       AND run_leases.run_id = runs.id
     WHERE runs.id = ANY($2::uuid[])
    UNION
    SELECT runtime_instances.id
      FROM runtime_instances
     WHERE runtime_instances.reserved_run_id = ANY($2::uuid[])
    UNION
    SELECT run_waits.handoff_runtime_instance_id
      FROM run_waits
     WHERE run_waits.run_id = ANY($1::uuid[])
       AND run_waits.handoff_runtime_instance_id IS NOT NULL
       AND run_waits.suspension_state IN (
           'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
       )
)
SELECT runtime_instances.id
  FROM runtime_instances
  JOIN target_runtimes ON target_runtimes.id = runtime_instances.id
 ORDER BY runtime_instances.id
 FOR UPDATE OF runtime_instances`, runIDs, cancelIDs)
	return collectCancellationIDs(rows, err, "lock cancellation Runtimes")
}

func lockCancellationRunLeases(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
SELECT run_leases.id
  FROM runs
  JOIN run_leases
    ON run_leases.id = runs.current_run_lease_id
   AND run_leases.run_id = runs.id
 WHERE runs.id = ANY($1::uuid[])
 ORDER BY run_leases.id
 FOR UPDATE OF run_leases`, runIDs)
	return collectCancellationIDs(rows, err, "lock cancellation Run Leases")
}

func lockCancellationMounts(
	ctx context.Context,
	tx pgx.Tx,
	runtimeIDs []uuid.UUID,
) error {
	if len(runtimeIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT id
  FROM workspace_mounts
 WHERE runtime_instance_id = ANY($1::uuid[])
   AND state IN ('mounting', 'mounted', 'unmounting')
 ORDER BY id
 FOR UPDATE`, runtimeIDs)
	return drainCancellationIDs(rows, err, "lock cancellation Mounts")
}

func lockCancellationWorkspaceLeases(
	ctx context.Context,
	tx pgx.Tx,
	runLeaseIDs []uuid.UUID,
) error {
	if len(runLeaseIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT id
  FROM workspace_leases
 WHERE owner_run_lease_id = ANY($1::uuid[])
   AND state IN ('active', 'releasing')
 ORDER BY id
 FOR UPDATE`, runLeaseIDs)
	return drainCancellationIDs(rows, err, "lock cancellation Workspace Leases")
}

func lockCancellationWaits(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
	cancelIDs []uuid.UUID,
) (map[uuid.UUID]cancellationWait, error) {
	rows, err := tx.Query(ctx, `
SELECT id,
       run_id,
       workspace_id,
       child_run_id,
       condition_state,
       suspension_state,
       expected_run_state_version,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       resume_request_version,
       handoff_runtime_instance_id,
       handoff_workspace_mount_id,
       base_workspace_version_id
  FROM run_waits
 WHERE (
       run_id = ANY($1::uuid[])
       OR child_run_id = ANY($2::uuid[])
 )
   AND suspension_state IN (
       'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
   )
 ORDER BY array_position($1::uuid[], run_id), id
 FOR UPDATE`, runIDs, cancelIDs)
	if err != nil {
		return nil, cancellationAuthority("lock cancellation Waits", err)
	}
	defer rows.Close()
	waitsByChild := make(map[uuid.UUID]cancellationWait)
	for rows.Next() {
		var wait cancellationWait
		if err := rows.Scan(
			&wait.id,
			&wait.runID,
			&wait.workspaceID,
			&wait.childRunID,
			&wait.conditionState,
			&wait.suspensionState,
			&wait.expectedRunStateVersion,
			&wait.attemptNumber,
			&wait.currentRunLeaseID,
			&wait.priorRunLeaseID,
			&wait.suspendCheckpointID,
			&wait.resumeRequestVersion,
			&wait.handoffRuntimeInstanceID,
			&wait.handoffWorkspaceMountID,
			&wait.baseWorkspaceVersionID,
		); err != nil {
			return nil, cancellationAuthority("scan cancellation Wait", err)
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
	if err := rows.Err(); err != nil {
		return nil, cancellationAuthority("read cancellation Waits", err)
	}
	return waitsByChild, nil
}

func lockCancellationCheckpoints(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) error {
	rows, err := tx.Query(ctx, `
SELECT id
  FROM run_checkpoints
 WHERE run_id = ANY($1::uuid[])
   AND state IN ('creating', 'ready')
 ORDER BY array_position($1::uuid[], run_id), id
 FOR UPDATE`, runIDs)
	return drainCancellationIDs(rows, err, "lock cancellation Checkpoints")
}

func collectCancellationIDs(
	rows pgx.Rows,
	err error,
	operation string,
) ([]uuid.UUID, error) {
	if err != nil {
		return nil, cancellationAuthority(operation, err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, cancellationAuthority(operation, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, cancellationAuthority(operation, err)
	}
	return ids, nil
}

func drainCancellationIDs(rows pgx.Rows, err error, operation string) error {
	_, err = collectCancellationIDs(rows, err, operation)
	return err
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
	if runStatusTerminal(run.status) {
		return nil
	}
	errorPayload, err := json.Marshal(map[string]any{
		"code": "run_cancelled", "message": "Run was cancelled", "retryable": false,
	})
	if err != nil {
		return err
	}
	if run.actorID.Valid {
		command, err := tx.Exec(ctx, `
UPDATE actors
   SET current_run_id = NULL,
       run_generation = run_generation + 1,
       state_version = state_version + 1,
       manual_run_cancelled = true,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND workspace_id = $2
   AND current_run_id = $3
   AND state IN ('open', 'closing')`,
			uuid.UUID(run.actorID.Bytes),
			run.workspaceID,
			run.id,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("install Actor Run cancellation hold", err)
		}
	}
	if _, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = CASE
           WHEN condition_state = 'pending' THEN 'cancelled'::wait_state
           ELSE condition_state
       END,
       condition_result = CASE
           WHEN condition_state = 'pending' THEN NULL
           ELSE condition_result
       END,
       condition_error = CASE
           WHEN condition_state = 'pending' THEN $2::jsonb
           ELSE condition_error
       END,
       condition_terminal_at = CASE
           WHEN condition_state = 'pending' THEN transaction_timestamp()
           ELSE condition_terminal_at
       END,
       condition_reason_code = CASE
           WHEN condition_state = 'pending' THEN 'run_cancelled'
           ELSE condition_reason_code
       END,
       suspension_state = 'cancelled',
       current_run_lease_id = NULL,
       suspension_terminal_at = transaction_timestamp(),
       suspension_reason_code = 'run_cancelled',
       suspension_error = $2::jsonb,
       updated_at = transaction_timestamp()
 WHERE run_id = $1
   AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming')`,
		run.id,
		errorPayload,
	); err != nil {
		return cancellationAuthority("cancel Run suspension", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE run_checkpoints
   SET state = 'invalid',
       invalidated_at = transaction_timestamp(),
       invalidation_reason_code = 'run_cancelled'
 WHERE run_id = $1
   AND state IN ('creating', 'ready')`, run.id); err != nil {
		return cancellationAuthority("invalidate Run checkpoints", err)
	}
	if run.currentRunLeaseID.Valid {
		command, err := tx.Exec(ctx, `
UPDATE workspace_leases
   SET state = 'fenced',
       terminal_at = transaction_timestamp(),
       terminal_reason_code = 'run_cancelled',
       terminal_error = $2::jsonb,
       updated_at = transaction_timestamp()
 WHERE owner_run_lease_id = $1
   AND state IN ('active', 'releasing')`,
			uuid.UUID(run.currentRunLeaseID.Bytes),
			errorPayload,
		)
		if err != nil {
			return cancellationAuthority("fence Run Workspace Lease", err)
		}
		if command.RowsAffected() > 1 {
			return cancellationAuthority("multiple Run Workspace Leases were active", nil)
		}
		command, err = tx.Exec(ctx, `
UPDATE run_leases
   SET state = 'cancelled',
       terminal_at = transaction_timestamp(),
       terminal_reason_code = 'run_cancelled',
       terminal_error = $2::jsonb,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND run_id = $3
   AND state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')`,
			uuid.UUID(run.currentRunLeaseID.Bytes),
			errorPayload,
			run.id,
		)
		if err != nil || command.RowsAffected() != 1 {
			return cancellationAuthority("cancel current Run Lease", err)
		}
	}
	var retainedRuntime, retainedMount any
	if preservedRuntimeID.Valid {
		retainedRuntime = uuid.UUID(preservedRuntimeID.Bytes)
	}
	if preservedMountID.Valid {
		retainedMount = uuid.UUID(preservedMountID.Bytes)
	}
	if _, err := tx.Exec(ctx, `
WITH candidate_runtimes AS (
    SELECT runtime_instance_id AS id
      FROM run_leases
     WHERE id = $2::uuid
       AND run_id = $1
    UNION
    SELECT id
      FROM runtime_instances
     WHERE reserved_run_id = $1
    UNION
    SELECT handoff_runtime_instance_id
      FROM run_waits
     WHERE run_id = $1
       AND handoff_runtime_instance_id IS NOT NULL
), target_runtimes AS (
    SELECT id
      FROM candidate_runtimes
     WHERE id IS DISTINCT FROM $3::uuid
), closing_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = CASE
               WHEN desired_state = 'closed' THEN desired_version
               ELSE desired_version + 1
           END,
           desired_at = transaction_timestamp(),
           desired_reason = 'run_cancelled',
           updated_at = transaction_timestamp()
     WHERE id IN (SELECT id FROM target_runtimes)
       AND observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING id
)
UPDATE workspace_mounts
   SET state = 'unmounting',
       stopped_at = COALESCE(stopped_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE runtime_instance_id IN (
       SELECT id FROM target_runtimes
       UNION
       SELECT id FROM closing_runtimes
   )
   AND id IS DISTINCT FROM $4::uuid
   AND state IN ('mounting', 'mounted')`,
		run.id,
		run.currentRunLeaseID,
		retainedRuntime,
		retainedMount,
	); err != nil {
		return cancellationAuthority("request cancelled Run runtime cleanup", err)
	}
	command, err := tx.Exec(ctx, `
UPDATE run_attempts
   SET terminal_outcome = 'cancelled',
       terminal_reason_code = 'run_cancelled',
       terminal_error = $3::jsonb,
       terminal_at = transaction_timestamp()
 WHERE run_id = $1
   AND number = $2
   AND terminal_at IS NULL`,
		run.id,
		run.currentAttemptNumber,
		errorPayload,
	)
	if err != nil || command.RowsAffected() != 1 {
		return cancellationAuthority("cancel current Run Attempt", err)
	}
	command, err = tx.Exec(ctx, `
UPDATE runs
   SET status = 'cancelled',
       terminal_reason_code = 'run_cancelled',
       error = $2::jsonb,
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       active_elapsed_ms = LEAST(
           max_active_duration_ms,
           active_elapsed_ms + CASE
               WHEN active_started_at IS NULL THEN 0
               ELSE GREATEST(
                   floor(extract(epoch FROM (
                       transaction_timestamp() - active_started_at
                   )) * 1000)::bigint,
                   0
               )
           END
       ),
       active_started_at = NULL,
       terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND state_version = $3
   AND status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')`,
		run.id,
		errorPayload,
		run.stateVersion,
	)
	if err != nil || command.RowsAffected() != 1 {
		return cancellationAuthority("terminalize cancelled Run", err)
	}
	if !run.actorID.Valid {
		if _, err := tx.Exec(ctx, `
UPDATE workspaces
   SET owner_run_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND owner_run_id = $2
   AND owner_actor_id IS NULL`,
			run.workspaceID,
			run.id,
		); err != nil {
			return cancellationAuthority("release cancelled Task Workspace", err)
		}
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO telemetry_outbox (
    org_id,
    stream_kind,
    source_kind,
    source_id,
    project_id,
    environment_id,
    run_id,
    run_lease_id,
    attempt_number,
    trace_id,
    span_id,
    category,
    severity,
    source,
    kind,
    message,
    payload,
    redaction_class,
    snapshot_version,
    observed_at
)
SELECT org_id,
       'event',
       'run',
       id,
       project_id,
       environment_id,
       id,
       $2::uuid,
       current_attempt_number,
       trace_id,
       root_span_id,
       'lifecycle',
       'info',
       'control',
       'run.cancelled',
       'Run cancelled',
       jsonb_build_object('reasonCode', 'run_cancelled'),
       'internal',
       state_version,
       transaction_timestamp()
  FROM runs
 WHERE id = $1`,
		run.id,
		run.currentRunLeaseID,
	); err != nil {
		return cancellationAuthority("record Run cancellation event", err)
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
	if wait.conditionState != WaitStatePending ||
		parent.status != RunStatusWaiting ||
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
	case RunWaitStateHot:
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
	case RunWaitStateCheckpointing:
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
	case RunWaitStateParked:
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
    lane,
    topic,
    partition_key,
    payload,
    available_at
) VALUES (
    'control',
    'run.resume',
    $1::uuid::text,
    jsonb_build_object(
        'environmentId', $2::uuid::text,
        'runId', $3::uuid::text,
        'runWaitId', $4::uuid::text,
        'resumeRequestVersion', $5::bigint
    ),
    transaction_timestamp()
)`,
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
	case RunWaitStateHot:
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
	case RunWaitStateCheckpointing:
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
	case RunWaitStateParked:
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
    lane, topic, partition_key, payload, available_at
) VALUES (
    'control', 'run.resume', $1,
    jsonb_build_object(
        'environmentId', $2::text,
        'runId', $3::text,
        'runWaitId', $4::text,
        'resumeRequestVersion', $5::bigint
    ),
    transaction_timestamp()
)`,
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

func runStatusTerminal(status RunStatus) bool {
	switch status {
	case RunStatusSucceeded, RunStatusFailed, RunStatusCancelled,
		RunStatusExpired, RunStatusSystemFailed:
		return true
	default:
		return false
	}
}

func cancellationAuthority(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrRunCancellationAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrRunCancellationAuthority, operation, cause)
}
