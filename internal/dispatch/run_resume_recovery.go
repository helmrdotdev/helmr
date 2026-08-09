package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type nestedResumeCandidate struct {
	runID                  pgtype.UUID
	orgID                  pgtype.UUID
	workspaceID            pgtype.UUID
	waitID                 pgtype.UUID
	checkpointID           pgtype.UUID
	checkpointBaseID       pgtype.UUID
	checkpointPrivateID    pgtype.UUID
	sourceRunLeaseID       pgtype.UUID
	sourceWorkspaceLeaseID pgtype.UUID
	runtimeID              pgtype.UUID
	mountID                pgtype.UUID
	mountGeneration        int64
	ownershipGeneration    int64
	parentWriterGeneration int64
}

type nestedResumeEdge struct {
	waitID                 pgtype.UUID
	parentRunID            pgtype.UUID
	childRunID             pgtype.UUID
	parentAttempt          int32
	expectedParentVersion  int64
	priorRunLeaseID        pgtype.UUID
	suspendCheckpointID    pgtype.UUID
	runtimeID              pgtype.UUID
	mountID                pgtype.UUID
	mountGeneration        int64
	ownershipGeneration    int64
	parentWriterGeneration int64
	childWriterGeneration  int64
	checkpointID           pgtype.UUID
	checkpointBaseID       pgtype.UUID
	checkpointPrivateID    pgtype.UUID
	sourceRunLeaseID       pgtype.UUID
	sourceWorkspaceLeaseID pgtype.UUID
}

type nestedResumeRun struct {
	id              pgtype.UUID
	entrypointKind  string
	actorID         pgtype.UUID
	orgID           pgtype.UUID
	projectID       pgtype.UUID
	environmentID   pgtype.UUID
	workspaceID     pgtype.UUID
	attempt         int32
	stateVersion    int64
	status          db.RunStatus
	currentLeaseID  pgtype.UUID
	activeStartedAt pgtype.Timestamptz
	activeElapsedMs int64
	maxActiveMs     int64
	traceID         pgtype.Text
	rootSpanID      string
}

type nestedResumePhysical struct {
	leaseID                  pgtype.UUID
	leaseState               db.RunLeaseState
	leaseExpiresAt           pgtype.Timestamptz
	startDeadlineAt          pgtype.Timestamptz
	workerID                 pgtype.UUID
	workerEpoch              int64
	workerCurrentEpoch       int64
	workerState              db.WorkerInstanceState
	workerLostAt             pgtype.Timestamptz
	workerTerminationReadyAt pgtype.Timestamptz
	runtimeID                pgtype.UUID
	runtimeDesired           db.RuntimeDesiredState
	runtimeObserved          db.RuntimeObservedState
	runtimeConverged         bool
	runtimeLostAt            pgtype.Timestamptz
	runtimeFailedAt          pgtype.Timestamptz
	mountID                  pgtype.UUID
	mountGeneration          int64
	mountState               db.WorkspaceMountState
	mountLostAt              pgtype.Timestamptz
	mountFailedAt            pgtype.Timestamptz
	workspaceLeaseID         pgtype.UUID
	workspaceLeaseState      db.WorkspaceLeaseState
	workspaceLeaseExpiry     pgtype.Timestamptz
	writerGeneration         int64
	leaseMountGeneration     int64
}

type nestedResumeWait struct {
	id                     pgtype.UUID
	resumeRequestVersion   int64
	resumeWriterGeneration int64
	restoreCheckpointID    pgtype.UUID
	runtimeID              pgtype.UUID
	mountID                pgtype.UUID
	mountGeneration        int64
	ownershipGeneration    int64
	parentWriterGeneration int64
	childWriterGeneration  int64
	baseVersionID          pgtype.UUID
	resumeVersionID        pgtype.UUID
}

type nestedResumeCursor struct {
	mu        sync.Mutex
	after     pgtype.UUID
	highWater pgtype.UUID
}

type nestedResumeLister func(
	context.Context,
	int32,
	pgtype.UUID,
	pgtype.UUID,
) ([]nestedResumeCandidate, error)

type nestedResumeHighWater func(context.Context) (pgtype.UUID, error)

func (cursor *nestedResumeCursor) next(
	ctx context.Context,
	limit int32,
	highWater nestedResumeHighWater,
	list nestedResumeLister,
) ([]nestedResumeCandidate, error) {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()

	if !cursor.highWater.Valid {
		var err error
		cursor.highWater, err = highWater(ctx)
		if err != nil {
			return nil, err
		}
		if !cursor.highWater.Valid {
			return nil, nil
		}
	}
	candidates, err := list(ctx, limit, cursor.after, cursor.highWater)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 && cursor.after.Valid {
		cursor.after = pgtype.UUID{}
		cursor.highWater = pgtype.UUID{}
		cursor.highWater, err = highWater(ctx)
		if err != nil {
			return nil, err
		}
		if !cursor.highWater.Valid {
			return nil, nil
		}
		candidates, err = list(ctx, limit, cursor.after, cursor.highWater)
		if err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		cursor.after = pgtype.UUID{}
		cursor.highWater = pgtype.UUID{}
		return nil, nil
	}
	cursor.after = candidates[len(candidates)-1].runID
	return candidates, nil
}

// RecoverExpiredRunResumes repairs both attachment-owning resumes and nested
// same-Workspace resumes. The generated query retains the ordinary/root
// recovery path. Nested recovery is application-owned because it must decide
// whether an enclosing in-kernel handoff can be retained or its lineage must
// be unwound.
func (d *Authority) RecoverExpiredRunResumes(
	ctx context.Context,
	limit int32,
) ([]db.RecoverExpiredRunResumesRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	nestedLimit := (limit + 1) / 2
	recovered, err := d.recoverExpiredNestedRunResumes(ctx, nestedLimit)
	if err != nil {
		return nil, err
	}
	remaining := limit - int32(len(recovered))
	if remaining <= 0 {
		return recovered, nil
	}
	roots, err := db.New(d.pool).RecoverExpiredRunResumes(ctx, remaining)
	if err != nil {
		return nil, err
	}
	recovered = append(recovered, roots...)
	remaining = limit - int32(len(recovered))
	if remaining <= 0 {
		return recovered, nil
	}
	more, err := d.recoverExpiredNestedRunResumes(ctx, remaining)
	if err != nil {
		return nil, err
	}
	return append(recovered, more...), nil
}

func (d *Authority) recoverExpiredNestedRunResumes(
	ctx context.Context,
	limit int32,
) ([]db.RecoverExpiredRunResumesRow, error) {
	candidates, err := d.nestedResumes.next(
		ctx,
		limit,
		d.nestedResumeHighWater,
		d.listExpiredNestedRunResumesAfter,
	)
	if err != nil {
		return nil, err
	}
	recovered := make([]db.RecoverExpiredRunResumesRow, 0, len(candidates))
	for _, candidate := range candidates {
		row, ok, err := d.recoverExpiredNestedRunResume(ctx, candidate)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		if ok {
			recovered = append(recovered, row)
		}
	}
	return recovered, nil
}

func (d *Authority) listExpiredNestedRunResumes(
	ctx context.Context,
	limit int32,
) ([]nestedResumeCandidate, error) {
	return d.listExpiredNestedRunResumesAfter(
		ctx,
		limit,
		pgtype.UUID{},
		pgtype.UUID{},
	)
}

func (d *Authority) listExpiredNestedRunResumesAfter(
	ctx context.Context,
	limit int32,
	after pgtype.UUID,
	highWater pgtype.UUID,
) ([]nestedResumeCandidate, error) {
	rows, err := d.pool.Query(ctx, `
SELECT runs.id, runs.org_id, runs.workspace_id, resume_wait.id,
       resume_checkpoint.id,
       resume_checkpoint.base_workspace_version_id,
       resume_checkpoint.private_workspace_version_id,
       resume_checkpoint.source_run_lease_id,
       resume_checkpoint.source_workspace_lease_id,
       resume_wait.handoff_runtime_instance_id,
       resume_wait.handoff_workspace_mount_id,
       resume_wait.handoff_mount_generation,
       resume_wait.ownership_generation,
       resume_wait.parent_writer_generation
  FROM runs
  JOIN run_leases
    ON run_leases.id = runs.current_run_lease_id
   AND run_leases.run_id = runs.id
   AND run_leases.attempt_number = runs.current_attempt_number
   AND run_leases.workspace_id = runs.workspace_id
   AND run_leases.state IN ('assigned', 'starting', 'running')
  JOIN run_waits AS resume_wait
    ON resume_wait.run_id = runs.id
   AND resume_wait.attempt_number = runs.current_attempt_number
   AND resume_wait.workspace_id = runs.workspace_id
   AND resume_wait.current_run_lease_id = run_leases.id
   AND resume_wait.suspension_state = 'resuming'
   AND resume_wait.handoff_runtime_instance_id IS NOT NULL
   AND resume_wait.handoff_workspace_mount_id IS NOT NULL
   AND resume_wait.handoff_mount_generation IS NOT NULL
   AND resume_wait.ownership_generation IS NOT NULL
   AND resume_wait.child_writer_generation IS NOT NULL
   AND resume_wait.resume_writer_generation IS NOT NULL
   AND resume_wait.condition_state = 'completed'
   AND resume_wait.handoff_resume_checkpoint_id IS NOT NULL
   AND resume_wait.resume_workspace_version_id IS NOT NULL
  JOIN run_checkpoints AS resume_checkpoint
    ON resume_checkpoint.id = resume_wait.handoff_resume_checkpoint_id
   AND resume_checkpoint.kind = 'handoff_resume'
   AND resume_checkpoint.run_id = resume_wait.run_id
   AND resume_checkpoint.attempt_number = resume_wait.attempt_number
   AND resume_checkpoint.run_wait_id = resume_wait.id
   AND resume_checkpoint.workspace_id = resume_wait.workspace_id
   AND resume_checkpoint.state = 'ready'
   AND resume_checkpoint.private_workspace_version_id =
       resume_wait.resume_workspace_version_id
   AND (resume_checkpoint.expires_at IS NULL
        OR resume_checkpoint.expires_at > transaction_timestamp())
  JOIN run_waits AS enclosing_wait
    ON enclosing_wait.child_run_id = runs.id
   AND enclosing_wait.workspace_id = runs.workspace_id
   AND enclosing_wait.child_parent_owned IS TRUE
   AND enclosing_wait.condition_state = 'pending'
   AND enclosing_wait.suspension_state = 'parked'
   AND enclosing_wait.handoff_runtime_instance_id =
       resume_wait.handoff_runtime_instance_id
   AND enclosing_wait.handoff_workspace_mount_id =
       resume_wait.handoff_workspace_mount_id
   AND enclosing_wait.child_writer_generation =
       resume_wait.resume_writer_generation
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.workspace_id = runs.workspace_id
   AND runtime_instances.reclaimed_at IS NULL
  JOIN workspace_mounts
    ON workspace_mounts.id = resume_wait.handoff_workspace_mount_id
   AND workspace_mounts.runtime_instance_id =
       resume_wait.handoff_runtime_instance_id
   AND workspace_mounts.workspace_id = runs.workspace_id
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
 WHERE runs.entrypoint_kind = 'task'
   AND runs.session_id IS NULL
   AND runs.parent_owns_lifecycle IS TRUE
   AND runs.status IN ('queued', 'running')
   AND ($2::uuid IS NULL OR runs.id > $2)
   AND ($3::uuid IS NULL OR runs.id <= $3)
   AND (
       run_leases.expires_at <= transaction_timestamp()
       OR (run_leases.state IN ('assigned', 'starting')
           AND run_leases.start_deadline_at <= transaction_timestamp())
       OR runtime_instances.lost_at <= transaction_timestamp()
       OR runtime_instances.failed_at <= transaction_timestamp()
       OR workspace_mounts.lost_at <= transaction_timestamp()
       OR workspace_mounts.failed_at <= transaction_timestamp()
       OR worker_instances.lost_at <= transaction_timestamp()
       OR worker_instances.termination_ready_at <= transaction_timestamp()
       OR worker_instances.current_epoch IS DISTINCT FROM run_leases.worker_epoch
   )
 ORDER BY runs.id
 LIMIT $1`,
		limit,
		after,
		highWater,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []nestedResumeCandidate
	for rows.Next() {
		var candidate nestedResumeCandidate
		if err := rows.Scan(
			&candidate.runID,
			&candidate.orgID,
			&candidate.workspaceID,
			&candidate.waitID,
			&candidate.checkpointID,
			&candidate.checkpointBaseID,
			&candidate.checkpointPrivateID,
			&candidate.sourceRunLeaseID,
			&candidate.sourceWorkspaceLeaseID,
			&candidate.runtimeID,
			&candidate.mountID,
			&candidate.mountGeneration,
			&candidate.ownershipGeneration,
			&candidate.parentWriterGeneration,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (d *Authority) nestedResumeHighWater(ctx context.Context) (pgtype.UUID, error) {
	var highWater pgtype.UUID
	err := d.pool.QueryRow(ctx, `
SELECT id
  FROM runs
 ORDER BY id DESC
 LIMIT 1`).Scan(&highWater)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	return highWater, err
}

func (d *Authority) recoverExpiredNestedRunResume(
	ctx context.Context,
	candidate nestedResumeCandidate,
) (db.RecoverExpiredRunResumesRow, bool, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, err
	}
	defer rollback(ctx, tx)

	edges, err := discoverNestedResumeEdges(ctx, tx, candidate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.RecoverExpiredRunResumesRow{}, false, nil
		}
		return db.RecoverExpiredRunResumesRow{}, false, err
	}
	rootKind, rootActorID, err := discoverNestedResumeRoot(
		ctx,
		tx,
		edges[0].parentRunID,
	)
	if err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("discover nested resume root: %w", err)
	}
	if rootKind == "actor" {
		var actorID pgtype.UUID
		if !rootActorID.Valid {
			return db.RecoverExpiredRunResumesRow{}, false, nil
		}
		if err := tx.QueryRow(ctx, `
SELECT id
  FROM sessions
 WHERE id = $1
   AND current_run_id = $2
   AND state IN ('open', 'closing')
 FOR UPDATE`,
			rootActorID,
			edges[0].parentRunID,
		).Scan(&actorID); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock nested resume actor: %w", err)
		}
	} else if rootKind != "task" || rootActorID.Valid {
		return db.RecoverExpiredRunResumesRow{}, false, nil
	}
	lineage := make([]nestedResumeRun, 0, len(edges)+1)
	for index, edge := range edges {
		run, err := lockNestedResumeRun(ctx, tx, edge.parentRunID)
		if err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock nested resume lineage run: %w", err)
		}
		if index == 0 {
			if run.entrypointKind != rootKind || run.actorID != rootActorID {
				return db.RecoverExpiredRunResumesRow{}, false, nil
			}
		} else if run.entrypointKind != "task" || run.actorID.Valid {
			return db.RecoverExpiredRunResumesRow{}, false, nil
		}
		lineage = append(lineage, run)
	}
	current, err := lockNestedResumeRun(ctx, tx, candidate.runID)
	if err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume run: %w", err)
	}
	if current.entrypointKind != "task" || current.actorID.Valid {
		return db.RecoverExpiredRunResumesRow{}, false, nil
	}
	lineage = append(lineage, current)

	var workspaceOwnership, workspaceWriter int64
	var workspaceState db.WorkspaceState
	var workspaceDesired db.WorkspaceDesiredState
	var workspaceDirty db.WorkspaceDirtyState
	var ownerRunID, ownerActorID pgtype.UUID
	root := lineage[0]
	err = tx.QueryRow(ctx, `
SELECT workspaces.ownership_generation, workspaces.writer_generation,
       workspaces.state, workspaces.desired_state, workspaces.dirty_state,
       workspaces.owner_run_id, workspaces.owner_session_id
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE workspaces.id = $1
   AND workspaces.environment_id = $2
   AND environments.org_id = $3
   AND environments.project_id = $4
   AND workspaces.state = 'active'
   AND workspaces.desired_state = 'active'
 FOR UPDATE OF workspaces`,
		candidate.workspaceID,
		root.environmentID,
		candidate.orgID,
		root.projectID,
	).Scan(
		&workspaceOwnership,
		&workspaceWriter,
		&workspaceState,
		&workspaceDesired,
		&workspaceDirty,
		&ownerRunID,
		&ownerActorID,
	)
	if err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock nested resume workspace: %w", err)
	}
	if workspaceDirty != db.WorkspaceDirtyStateClean {
		return db.RecoverExpiredRunResumesRow{}, false, nil
	}
	if (root.entrypointKind == "task" &&
		(ownerRunID != root.id || ownerActorID.Valid)) ||
		(root.entrypointKind == "actor" &&
			(!root.actorID.Valid || ownerActorID != root.actorID || ownerRunID.Valid)) {
		return db.RecoverExpiredRunResumesRow{}, false, nil
	}

	for _, run := range lineage {
		var attempt int32
		if err := tx.QueryRow(ctx, `
SELECT number
  FROM run_attempts
 WHERE run_id = $1
   AND number = $2
   AND workspace_id = $3
   AND entrypoint_kind = $4
   AND terminal_at IS NULL
 FOR UPDATE`,
			run.id,
			run.attempt,
			candidate.workspaceID,
			run.entrypointKind,
		).Scan(&attempt); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock nested resume attempt: %w", err)
		}
	}

	physical, err := lockNestedResumePhysical(ctx, tx, current)
	if err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock nested resume physical authority: %w", err)
	}
	// Preserve one lock phase for each lease class: outer-to-inner checkpoint
	// sources, the current checkpoint source, and finally the current resume.
	// No workspace lease is locked until every run lease is held.
	for _, edge := range edges {
		if err := lockNestedResumeCheckpointRunLease(
			ctx,
			tx,
			edge.sourceRunLeaseID,
			edge.parentRunID,
			edge.parentAttempt,
			candidate.workspaceID,
			edge.runtimeID,
		); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock outer nested resume source run lease: %w", err)
		}
	}
	if err := lockNestedResumeCheckpointRunLease(
		ctx,
		tx,
		candidate.sourceRunLeaseID,
		current.id,
		current.attempt,
		candidate.workspaceID,
		candidate.runtimeID,
	); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume source run lease: %w", err)
	}
	if err := lockNestedResumeCurrentRunLease(ctx, tx, current, physical); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume run lease: %w", err)
	}
	for _, edge := range edges {
		if err := lockNestedResumeCheckpointWorkspaceLease(
			ctx,
			tx,
			edge.sourceRunLeaseID,
			edge.sourceWorkspaceLeaseID,
			candidate.workspaceID,
			edge.runtimeID,
			edge.mountID,
			edge.checkpointBaseID,
			edge.ownershipGeneration,
			edge.parentWriterGeneration,
			edge.mountGeneration,
		); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock outer nested resume source workspace lease: %w", err)
		}
	}
	if err := lockNestedResumeCheckpointWorkspaceLease(
		ctx,
		tx,
		candidate.sourceRunLeaseID,
		candidate.sourceWorkspaceLeaseID,
		candidate.workspaceID,
		candidate.runtimeID,
		candidate.mountID,
		candidate.checkpointBaseID,
		candidate.ownershipGeneration,
		candidate.parentWriterGeneration,
		candidate.mountGeneration,
	); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume source workspace lease: %w", err)
	}
	if err := lockNestedResumeCurrentWorkspaceLease(
		ctx,
		tx,
		current,
		physical,
		candidate.checkpointPrivateID,
		candidate.ownershipGeneration,
	); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume workspace lease: %w", err)
	}
	for _, edge := range edges {
		if err := lockNestedResumeEdge(ctx, tx, edge, candidate.workspaceID); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock outer nested resume wait: %w", err)
		}
	}
	wait, err := lockNestedResumeWait(ctx, tx, candidate, current, physical)
	if err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume wait: %w", err)
	}
	for _, edge := range edges {
		var sourceRunLeaseID, sourceWorkspaceLeaseID, baseID, privateID pgtype.UUID
		if err := tx.QueryRow(ctx, `
SELECT source_run_lease_id,
       source_workspace_lease_id,
       base_workspace_version_id,
       private_workspace_version_id
  FROM run_checkpoints
 WHERE id = $1
   AND kind = 'suspend'
   AND run_id = $2
   AND attempt_number = $3
   AND run_wait_id = $4
   AND workspace_id = $5
   AND state = 'ready'
   AND (expires_at IS NULL OR expires_at > transaction_timestamp())
 FOR UPDATE`,
			edge.suspendCheckpointID,
			edge.parentRunID,
			edge.parentAttempt,
			edge.waitID,
			candidate.workspaceID,
		).Scan(
			&sourceRunLeaseID,
			&sourceWorkspaceLeaseID,
			&baseID,
			&privateID,
		); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock outer nested resume checkpoint: %w", err)
		}
		if sourceRunLeaseID != edge.sourceRunLeaseID ||
			sourceWorkspaceLeaseID != edge.sourceWorkspaceLeaseID ||
			baseID != edge.checkpointBaseID ||
			privateID != edge.checkpointPrivateID ||
			edge.priorRunLeaseID != edge.sourceRunLeaseID {
			return db.RecoverExpiredRunResumesRow{}, false, nil
		}
	}
	var sourceRunLeaseID, sourceWorkspaceLeaseID, baseID, privateID pgtype.UUID
	if err := tx.QueryRow(ctx, `
SELECT source_run_lease_id,
       source_workspace_lease_id,
       base_workspace_version_id,
       private_workspace_version_id
  FROM run_checkpoints
 WHERE id = $1
   AND kind = 'handoff_resume'
   AND run_id = $2
   AND attempt_number = $3
   AND run_wait_id = $4
   AND workspace_id = $5
   AND state = 'ready'
   AND (expires_at IS NULL OR expires_at > transaction_timestamp())
 FOR UPDATE`,
		wait.restoreCheckpointID,
		current.id,
		current.attempt,
		wait.id,
		current.workspaceID,
	).Scan(
		&sourceRunLeaseID,
		&sourceWorkspaceLeaseID,
		&baseID,
		&privateID,
	); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, fmt.Errorf("lock current nested resume checkpoint: %w", err)
	}
	if sourceRunLeaseID != candidate.sourceRunLeaseID ||
		sourceWorkspaceLeaseID != candidate.sourceWorkspaceLeaseID ||
		baseID != candidate.checkpointBaseID ||
		privateID != wait.resumeVersionID ||
		privateID != candidate.checkpointPrivateID {
		return db.RecoverExpiredRunResumesRow{}, false, nil
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, err
	}
	reason, physicalLost := nestedResumeLossReason(now, physical)
	if reason == "" {
		return db.RecoverExpiredRunResumesRow{}, false, nil
	}
	safeRetained := !physicalLost &&
		(physical.leaseState == db.RunLeaseStateAssigned ||
			physical.leaseState == db.RunLeaseStateStarting) &&
		current.status == db.RunStatusQueued &&
		!current.activeStartedAt.Valid &&
		physical.runtimeID == wait.runtimeID &&
		physical.mountID == wait.mountID &&
		physical.mountGeneration == physical.leaseMountGeneration &&
		physical.mountGeneration >= wait.mountGeneration &&
		physical.runtimeDesired == db.RuntimeDesiredStateReady &&
		physical.runtimeObserved == db.RuntimeObservedStateReady &&
		physical.runtimeConverged &&
		physical.mountState == db.WorkspaceMountStateMounted &&
		workspaceOwnership == wait.ownershipGeneration &&
		workspaceWriter == wait.resumeWriterGeneration &&
		edges[len(edges)-1].childWriterGeneration == wait.resumeWriterGeneration

	if err := expireNestedResumeLeases(
		ctx,
		tx,
		physical,
		reason,
	); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, err
	}
	if safeRetained {
		if err := requeueNestedResume(
			ctx,
			tx,
			current,
			wait,
			edges[len(edges)-1],
		); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, err
		}
	} else {
		if err := failNestedResumeLineage(
			ctx,
			tx,
			lineage,
			edges,
			wait,
			physical,
			now,
		); err != nil {
			return db.RecoverExpiredRunResumesRow{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.RecoverExpiredRunResumesRow{}, false, err
	}
	return db.RecoverExpiredRunResumesRow{
		ID: wait.id, OrgID: current.orgID, RunID: current.id,
	}, true, nil
}

func discoverNestedResumeEdges(
	ctx context.Context,
	tx pgx.Tx,
	candidate nestedResumeCandidate,
) ([]nestedResumeEdge, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE edges AS (
    SELECT handoff.id, handoff.run_id, handoff.child_run_id, 0 AS depth
      FROM run_waits AS handoff
     WHERE handoff.child_run_id = $1
       AND handoff.workspace_id = $2
       AND handoff.child_parent_owned IS TRUE
       AND handoff.condition_state = 'pending'
       AND handoff.suspension_state = 'parked'
    UNION ALL
    SELECT outer_wait.id, outer_wait.run_id, outer_wait.child_run_id,
           edges.depth + 1
      FROM edges
      JOIN runs AS parent
        ON parent.id = edges.run_id
       AND parent.workspace_id = $2
       AND parent.parent_owns_lifecycle IS TRUE
      JOIN run_waits AS outer_wait
        ON outer_wait.child_run_id = parent.id
       AND outer_wait.workspace_id = $2
       AND outer_wait.child_parent_owned IS TRUE
       AND outer_wait.condition_state = 'pending'
       AND outer_wait.suspension_state = 'parked'
)
SELECT handoff.id, handoff.run_id, handoff.child_run_id,
       handoff.attempt_number, handoff.expected_run_state_version,
       handoff.prior_run_lease_id, handoff.suspend_checkpoint_id,
       handoff.handoff_runtime_instance_id,
       handoff.handoff_workspace_mount_id,
       handoff.handoff_mount_generation,
       handoff.ownership_generation,
       handoff.parent_writer_generation,
       handoff.child_writer_generation,
       checkpoint.id,
       checkpoint.base_workspace_version_id,
       checkpoint.private_workspace_version_id,
       checkpoint.source_run_lease_id,
       checkpoint.source_workspace_lease_id
  FROM edges
  JOIN run_waits AS handoff ON handoff.id = edges.id
  JOIN run_checkpoints AS checkpoint
    ON checkpoint.id = handoff.suspend_checkpoint_id
   AND checkpoint.kind = 'suspend'
   AND checkpoint.run_id = handoff.run_id
   AND checkpoint.attempt_number = handoff.attempt_number
   AND checkpoint.run_wait_id = handoff.id
   AND checkpoint.workspace_id = handoff.workspace_id
   AND checkpoint.state = 'ready'
   AND checkpoint.private_workspace_version_id =
       handoff.base_workspace_version_id
   AND (checkpoint.expires_at IS NULL
        OR checkpoint.expires_at > transaction_timestamp())
 ORDER BY edges.depth DESC`,
		candidate.runID,
		candidate.workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []nestedResumeEdge
	for rows.Next() {
		var edge nestedResumeEdge
		if err := rows.Scan(
			&edge.waitID,
			&edge.parentRunID,
			&edge.childRunID,
			&edge.parentAttempt,
			&edge.expectedParentVersion,
			&edge.priorRunLeaseID,
			&edge.suspendCheckpointID,
			&edge.runtimeID,
			&edge.mountID,
			&edge.mountGeneration,
			&edge.ownershipGeneration,
			&edge.parentWriterGeneration,
			&edge.childWriterGeneration,
			&edge.checkpointID,
			&edge.checkpointBaseID,
			&edge.checkpointPrivateID,
			&edge.sourceRunLeaseID,
			&edge.sourceWorkspaceLeaseID,
		); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(edges) == 0 || edges[len(edges)-1].childRunID != candidate.runID {
		return nil, pgx.ErrNoRows
	}
	return edges, nil
}

func discoverNestedResumeRoot(
	ctx context.Context,
	tx pgx.Tx,
	runID pgtype.UUID,
) (string, pgtype.UUID, error) {
	var kind string
	var actorID pgtype.UUID
	err := tx.QueryRow(ctx, `
SELECT entrypoint_kind, session_id
  FROM runs
 WHERE id = $1`,
		runID,
	).Scan(&kind, &actorID)
	return kind, actorID, err
}

func lockNestedResumeRun(
	ctx context.Context,
	tx pgx.Tx,
	runID pgtype.UUID,
) (nestedResumeRun, error) {
	var run nestedResumeRun
	err := tx.QueryRow(ctx, `
SELECT id, entrypoint_kind, session_id, org_id, project_id, environment_id,
       workspace_id,
       current_attempt_number, state_version, status, current_run_lease_id,
       active_started_at, active_elapsed_ms, max_active_duration_ms,
       trace_id, root_span_id
 FROM runs
 WHERE id = $1
 FOR UPDATE`,
		runID,
	).Scan(
		&run.id,
		&run.entrypointKind,
		&run.actorID,
		&run.orgID,
		&run.projectID,
		&run.environmentID,
		&run.workspaceID,
		&run.attempt,
		&run.stateVersion,
		&run.status,
		&run.currentLeaseID,
		&run.activeStartedAt,
		&run.activeElapsedMs,
		&run.maxActiveMs,
		&run.traceID,
		&run.rootSpanID,
	)
	return run, err
}

func lockNestedResumePhysical(
	ctx context.Context,
	tx pgx.Tx,
	run nestedResumeRun,
) (nestedResumePhysical, error) {
	var physical nestedResumePhysical
	err := tx.QueryRow(ctx, `
SELECT run_leases.id, run_leases.state, run_leases.expires_at,
       run_leases.start_deadline_at, run_leases.worker_instance_id,
       run_leases.worker_epoch, run_leases.runtime_instance_id,
       workspace_leases.id, workspace_leases.state, workspace_leases.expires_at,
       workspace_leases.writer_generation,
       workspace_leases.mount_fencing_generation,
       workspace_leases.workspace_mount_id
  FROM run_leases
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_leases.state IN ('active', 'releasing')
 WHERE run_leases.id = $1
   AND run_leases.run_id = $2
   AND run_leases.attempt_number = $3
   AND run_leases.workspace_id = $4
   AND run_leases.state IN ('assigned', 'starting', 'running')`,
		run.currentLeaseID,
		run.id,
		run.attempt,
		run.workspaceID,
	).Scan(
		&physical.leaseID,
		&physical.leaseState,
		&physical.leaseExpiresAt,
		&physical.startDeadlineAt,
		&physical.workerID,
		&physical.workerEpoch,
		&physical.runtimeID,
		&physical.workspaceLeaseID,
		&physical.workspaceLeaseState,
		&physical.workspaceLeaseExpiry,
		&physical.writerGeneration,
		&physical.leaseMountGeneration,
		&physical.mountID,
	)
	if err != nil {
		return nestedResumePhysical{}, err
	}
	err = tx.QueryRow(ctx, `
SELECT state, current_epoch, lost_at, termination_ready_at
  FROM worker_instances
 WHERE id = $1
 FOR UPDATE`,
		physical.workerID,
	).Scan(
		&physical.workerState,
		&physical.workerCurrentEpoch,
		&physical.workerLostAt,
		&physical.workerTerminationReadyAt,
	)
	if err != nil {
		return nestedResumePhysical{}, err
	}
	err = tx.QueryRow(ctx, `
SELECT desired_state, observed_state, observed_desired_version = desired_version,
       lost_at, failed_at
  FROM runtime_instances
 WHERE id = $1
   AND workspace_id = $2
   AND worker_instance_id = $3
   AND worker_epoch = $4
   AND reclaimed_at IS NULL
 FOR UPDATE`,
		physical.runtimeID,
		run.workspaceID,
		physical.workerID,
		physical.workerEpoch,
	).Scan(
		&physical.runtimeDesired,
		&physical.runtimeObserved,
		&physical.runtimeConverged,
		&physical.runtimeLostAt,
		&physical.runtimeFailedAt,
	)
	if err != nil {
		return nestedResumePhysical{}, err
	}
	err = tx.QueryRow(ctx, `
SELECT fencing_generation, state, lost_at, failed_at
  FROM workspace_mounts
 WHERE id = $1
   AND runtime_instance_id = $2
   AND workspace_id = $3
   AND worker_instance_id = $4
   AND worker_epoch = $5
 FOR UPDATE`,
		physical.mountID,
		physical.runtimeID,
		run.workspaceID,
		physical.workerID,
		physical.workerEpoch,
	).Scan(
		&physical.mountGeneration,
		&physical.mountState,
		&physical.mountLostAt,
		&physical.mountFailedAt,
	)
	if err != nil {
		return nestedResumePhysical{}, err
	}
	return physical, nil
}

func lockNestedResumeCheckpointRunLease(
	ctx context.Context,
	tx pgx.Tx,
	runLeaseID pgtype.UUID,
	runID pgtype.UUID,
	attempt int32,
	workspaceID pgtype.UUID,
	runtimeID pgtype.UUID,
) error {
	var lockedRunLeaseID pgtype.UUID
	err := tx.QueryRow(ctx, `
SELECT id
  FROM run_leases
 WHERE id = $1
   AND run_id = $2
   AND attempt_number = $3
   AND workspace_id = $4
   AND runtime_instance_id = $5
   AND state = 'checkpointed'
 FOR UPDATE`,
		runLeaseID,
		runID,
		attempt,
		workspaceID,
		runtimeID,
	).Scan(&lockedRunLeaseID)
	if err != nil {
		return fmt.Errorf("lock source run lease: %w", err)
	}
	return nil
}

func lockNestedResumeCurrentRunLease(
	ctx context.Context,
	tx pgx.Tx,
	run nestedResumeRun,
	physical nestedResumePhysical,
) error {
	var leaseID pgtype.UUID
	return tx.QueryRow(ctx, `
SELECT id
  FROM run_leases
 WHERE id = $1
   AND run_id = $2
   AND attempt_number = $3
   AND workspace_id = $4
   AND runtime_instance_id = $5
   AND state = $6
   AND expires_at = $7
   AND start_deadline_at = $8
 FOR UPDATE`,
		physical.leaseID,
		run.id,
		run.attempt,
		run.workspaceID,
		physical.runtimeID,
		physical.leaseState,
		physical.leaseExpiresAt,
		physical.startDeadlineAt,
	).Scan(&leaseID)
}

func lockNestedResumeCheckpointWorkspaceLease(
	ctx context.Context,
	tx pgx.Tx,
	runLeaseID pgtype.UUID,
	workspaceLeaseID pgtype.UUID,
	workspaceID pgtype.UUID,
	runtimeID pgtype.UUID,
	mountID pgtype.UUID,
	baseID pgtype.UUID,
	ownershipGeneration int64,
	writerGeneration int64,
	mountGeneration int64,
) error {
	var lockedWorkspaceLeaseID, lockedRunLeaseOwnerID pgtype.UUID
	var lockedRuntimeID, lockedMountID, lockedBaseID pgtype.UUID
	var lockedState db.WorkspaceLeaseState
	var lockedOwnership, lockedWriter, lockedMountGeneration int64
	err := tx.QueryRow(ctx, `
SELECT id, state, owner_run_lease_id, runtime_instance_id,
       workspace_mount_id, base_version_id, ownership_generation,
       writer_generation, mount_fencing_generation
  FROM workspace_leases
 WHERE id = $1
   AND workspace_id = $2
   AND owner_run_lease_id = $3
 FOR UPDATE`,
		workspaceLeaseID,
		workspaceID,
		runLeaseID,
	).Scan(
		&lockedWorkspaceLeaseID,
		&lockedState,
		&lockedRunLeaseOwnerID,
		&lockedRuntimeID,
		&lockedMountID,
		&lockedBaseID,
		&lockedOwnership,
		&lockedWriter,
		&lockedMountGeneration,
	)
	if err != nil {
		return fmt.Errorf("lock source workspace lease: %w", err)
	}
	if (lockedState != db.WorkspaceLeaseStateReleased &&
		lockedState != db.WorkspaceLeaseStateFenced) ||
		lockedRunLeaseOwnerID != runLeaseID ||
		lockedRuntimeID != runtimeID ||
		lockedMountID != mountID ||
		lockedBaseID != baseID ||
		lockedOwnership != ownershipGeneration ||
		lockedWriter != writerGeneration ||
		lockedMountGeneration != mountGeneration {
		return fmt.Errorf(
			"source workspace lease provenance mismatch: state=%s runtime=%s mount=%s base=%s ownership=%d writer=%d mount_generation=%d: %w",
			lockedState,
			pgvalue.UUIDString(lockedRuntimeID),
			pgvalue.UUIDString(lockedMountID),
			pgvalue.UUIDString(lockedBaseID),
			lockedOwnership,
			lockedWriter,
			lockedMountGeneration,
			pgx.ErrNoRows,
		)
	}
	return nil
}

func lockNestedResumeCurrentWorkspaceLease(
	ctx context.Context,
	tx pgx.Tx,
	run nestedResumeRun,
	physical nestedResumePhysical,
	baseID pgtype.UUID,
	ownershipGeneration int64,
) error {
	var workspaceLeaseID pgtype.UUID
	return tx.QueryRow(ctx, `
SELECT id
  FROM workspace_leases
 WHERE id = $1
   AND workspace_id = $2
   AND owner_run_lease_id = $3
   AND runtime_instance_id = $4
   AND workspace_mount_id = $5
   AND state = $6
   AND expires_at = $7
   AND writer_generation = $8
   AND mount_fencing_generation = $9
   AND base_version_id = $10
   AND ownership_generation = $11
 FOR UPDATE`,
		physical.workspaceLeaseID,
		run.workspaceID,
		physical.leaseID,
		physical.runtimeID,
		physical.mountID,
		physical.workspaceLeaseState,
		physical.workspaceLeaseExpiry,
		physical.writerGeneration,
		physical.leaseMountGeneration,
		baseID,
		ownershipGeneration,
	).Scan(&workspaceLeaseID)
}

func lockNestedResumeEdge(
	ctx context.Context,
	tx pgx.Tx,
	edge nestedResumeEdge,
	workspaceID pgtype.UUID,
) error {
	var waitID pgtype.UUID
	return tx.QueryRow(ctx, `
SELECT id
  FROM run_waits
 WHERE id = $1
   AND run_id = $2
   AND child_run_id = $3
   AND workspace_id = $4
   AND child_parent_owned IS TRUE
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
   AND current_run_lease_id IS NULL
   AND prior_run_lease_id = $5
   AND suspend_checkpoint_id = $6
   AND handoff_runtime_instance_id = $7
   AND handoff_workspace_mount_id = $8
   AND handoff_mount_generation = $9
   AND ownership_generation = $10
   AND parent_writer_generation = $11
   AND child_writer_generation = $12
   AND base_workspace_version_id = $13
 FOR UPDATE`,
		edge.waitID,
		edge.parentRunID,
		edge.childRunID,
		workspaceID,
		edge.priorRunLeaseID,
		edge.suspendCheckpointID,
		edge.runtimeID,
		edge.mountID,
		edge.mountGeneration,
		edge.ownershipGeneration,
		edge.parentWriterGeneration,
		edge.childWriterGeneration,
		edge.checkpointPrivateID,
	).Scan(&waitID)
}

func lockNestedResumeWait(
	ctx context.Context,
	tx pgx.Tx,
	candidate nestedResumeCandidate,
	run nestedResumeRun,
	physical nestedResumePhysical,
) (nestedResumeWait, error) {
	var wait nestedResumeWait
	err := tx.QueryRow(ctx, `
SELECT id, resume_request_version, resume_writer_generation,
       CASE WHEN condition_state = 'completed'
            THEN handoff_resume_checkpoint_id
            ELSE suspend_checkpoint_id
       END,
       handoff_runtime_instance_id, handoff_workspace_mount_id,
       handoff_mount_generation, ownership_generation,
       parent_writer_generation, child_writer_generation,
       base_workspace_version_id, resume_workspace_version_id
  FROM run_waits
 WHERE id = $1
   AND run_id = $2
   AND attempt_number = $3
   AND workspace_id = $4
   AND current_run_lease_id = $5
   AND suspension_state = 'resuming'
   AND condition_state = 'completed'
   AND handoff_resume_checkpoint_id = $7
   AND resume_writer_generation = $6
 FOR UPDATE`,
		candidate.waitID,
		run.id,
		run.attempt,
		run.workspaceID,
		physical.leaseID,
		physical.writerGeneration,
		candidate.checkpointID,
	).Scan(
		&wait.id,
		&wait.resumeRequestVersion,
		&wait.resumeWriterGeneration,
		&wait.restoreCheckpointID,
		&wait.runtimeID,
		&wait.mountID,
		&wait.mountGeneration,
		&wait.ownershipGeneration,
		&wait.parentWriterGeneration,
		&wait.childWriterGeneration,
		&wait.baseVersionID,
		&wait.resumeVersionID,
	)
	if err == nil &&
		(wait.restoreCheckpointID != candidate.checkpointID ||
			wait.runtimeID != candidate.runtimeID ||
			wait.mountID != candidate.mountID ||
			wait.mountGeneration != candidate.mountGeneration ||
			wait.ownershipGeneration != candidate.ownershipGeneration ||
			wait.parentWriterGeneration != candidate.parentWriterGeneration ||
			wait.resumeVersionID != candidate.checkpointPrivateID) {
		return nestedResumeWait{}, pgx.ErrNoRows
	}
	return wait, err
}

func nestedResumeLossReason(
	now time.Time,
	physical nestedResumePhysical,
) (string, bool) {
	physicalLost := physical.workerState != db.WorkerInstanceStateActive ||
		physical.workerCurrentEpoch != physical.workerEpoch ||
		atOrBefore(physical.workerLostAt, now) ||
		atOrBefore(physical.workerTerminationReadyAt, now) ||
		atOrBefore(physical.runtimeLostAt, now) ||
		atOrBefore(physical.runtimeFailedAt, now) ||
		atOrBefore(physical.mountLostAt, now) ||
		atOrBefore(physical.mountFailedAt, now)
	if physicalLost {
		if atOrBefore(physical.runtimeFailedAt, now) ||
			atOrBefore(physical.mountFailedAt, now) {
			return "runtime_failed", true
		}
		return "worker_lost", true
	}
	if atOrBefore(physical.leaseExpiresAt, now) ||
		((physical.leaseState == db.RunLeaseStateAssigned ||
			physical.leaseState == db.RunLeaseStateStarting) &&
			atOrBefore(physical.startDeadlineAt, now)) {
		return "lease_expired", false
	}
	return "", false
}

func atOrBefore(value pgtype.Timestamptz, now time.Time) bool {
	return value.Valid && !value.Time.After(now)
}

func expireNestedResumeLeases(
	ctx context.Context,
	tx pgx.Tx,
	physical nestedResumePhysical,
	reason string,
) error {
	command, err := tx.Exec(ctx, `
UPDATE run_leases
   SET state = 'expired', terminal_at = transaction_timestamp(),
       terminal_reason_code = $2, updated_at = transaction_timestamp()
 WHERE id = $1
   AND state = $3
   AND expires_at = $4
   AND start_deadline_at = $5`,
		physical.leaseID,
		reason,
		physical.leaseState,
		physical.leaseExpiresAt,
		physical.startDeadlineAt,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	command, err = tx.Exec(ctx, `
UPDATE workspace_leases
   SET state = 'expired', terminal_at = transaction_timestamp(),
       terminal_reason_code = $2, updated_at = transaction_timestamp()
 WHERE id = $1
   AND owner_run_lease_id = $3
   AND state = $4
   AND expires_at = $5`,
		physical.workspaceLeaseID,
		reason,
		physical.leaseID,
		physical.workspaceLeaseState,
		physical.workspaceLeaseExpiry,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func requeueNestedResume(
	ctx context.Context,
	tx pgx.Tx,
	run nestedResumeRun,
	wait nestedResumeWait,
	enclosing nestedResumeEdge,
) error {
	var stateVersion int64
	err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'queued', current_run_lease_id = NULL,
       active_started_at = NULL, state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND state_version = $2
   AND status = 'queued'
   AND current_run_lease_id = $3
   AND active_started_at IS NULL
RETURNING state_version`,
		run.id,
		run.stateVersion,
		run.currentLeaseID,
	).Scan(&stateVersion)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET suspension_state = 'resume_pending', current_run_lease_id = NULL,
       resume_writer_generation = NULL,
       resume_request_version = resume_request_version + 1,
       expected_run_state_version = $2,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND suspension_state = 'resuming'
   AND current_run_lease_id = $3
   AND resume_request_version = $4
   AND resume_writer_generation = $5`,
		wait.id,
		stateVersion,
		run.currentLeaseID,
		wait.resumeRequestVersion,
		wait.resumeWriterGeneration,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	command, err = tx.Exec(ctx, `
UPDATE run_waits
   SET child_writer_generation = NULL,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND child_run_id = $2
   AND suspension_state = 'parked'
   AND condition_state = 'pending'
   AND child_writer_generation = $3`,
		enclosing.waitID,
		run.id,
		wait.resumeWriterGeneration,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func failNestedResumeLineage(
	ctx context.Context,
	tx pgx.Tx,
	lineage []nestedResumeRun,
	edges []nestedResumeEdge,
	wait nestedResumeWait,
	physical nestedResumePhysical,
	now time.Time,
) error {
	failedAt := pgvalue.Timestamptz(now)
	errorObject, err := json.Marshal(map[string]any{
		"code":      "same_workspace_handoff_runtime_lost",
		"message":   "nested same-Workspace resume lost its retained runtime",
		"retryable": false,
	})
	if err != nil {
		return err
	}
	failureObject, err := json.Marshal(map[string]any{
		"code":    "same_workspace_handoff_runtime_lost",
		"message": "Nested same-Workspace resume lost its retained runtime",
		"details": map[string]any{},
	})
	if err != nil {
		return err
	}
	reason := pgvalue.Text("same_workspace_handoff_runtime_lost")
	current := lineage[len(lineage)-1]
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET suspension_state = 'failed', current_run_lease_id = NULL,
       suspension_terminal_at = $2,
       suspension_reason_code = 'same_workspace_handoff_runtime_lost',
       suspension_error = $3::jsonb, updated_at = $2
 WHERE id = $1
   AND run_id = $4
   AND suspension_state = 'resuming'
   AND current_run_lease_id = $5
   AND resume_request_version = $6`,
		wait.id,
		failedAt,
		errorObject,
		current.id,
		current.currentLeaseID,
		wait.resumeRequestVersion,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	q := db.New(tx)
	if _, err := q.FailNestedSameWorkspaceAttempt(
		ctx,
		db.FailNestedSameWorkspaceAttemptParams{
			Error: errorObject, FailedAt: failedAt, RunID: current.id,
			AttemptNumber: current.attempt, WorkspaceID: current.workspaceID,
		},
	); err != nil {
		return err
	}
	command, err = tx.Exec(ctx, `
UPDATE runs
   SET status = 'system_failed',
	   failure = $2::jsonb, current_run_lease_id = NULL,
       active_elapsed_ms = LEAST(
           max_active_duration_ms,
           active_elapsed_ms + CASE
               WHEN active_started_at IS NULL THEN 0
               ELSE GREATEST(
                   floor(extract(epoch FROM ($3::timestamptz - active_started_at))
                         * 1000)::bigint,
                   0
               )
           END
       ),
       active_started_at = NULL, state_version = state_version + 1,
       terminal_at = $3, updated_at = $3
 WHERE id = $1
   AND state_version = $4
   AND current_run_lease_id = $5`,
		current.id,
		failureObject,
		failedAt,
		current.stateVersion,
		current.currentLeaseID,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	if err := appendNestedResumeFailureEvent(ctx, q, current, reason); err != nil {
		return err
	}

	for index := len(edges) - 1; index > 0; index-- {
		edge := edges[index]
		parent := lineage[index]
		if _, err := q.FailNestedSameWorkspaceWait(
			ctx,
			db.FailNestedSameWorkspaceWaitParams{
				Error: errorObject, FailedAt: failedAt, ReasonCode: reason,
				RunWaitID: edge.waitID, EnvironmentID: parent.environmentID,
				RunID: parent.id, AttemptNumber: parent.attempt,
				WorkspaceID: parent.workspaceID, ChildRunID: edge.childRunID,
				HandoffRuntimeInstanceID: edge.runtimeID,
				HandoffWorkspaceMountID:  edge.mountID,
				HandoffMountGeneration:   pgtype.Int8{Int64: edge.mountGeneration, Valid: true},
				OwnershipGeneration:      pgtype.Int8{Int64: edge.ownershipGeneration, Valid: true},
			},
		); err != nil {
			return err
		}
		if _, err := q.FailNestedSameWorkspaceAttempt(
			ctx,
			db.FailNestedSameWorkspaceAttemptParams{
				Error: errorObject, FailedAt: failedAt, RunID: parent.id,
				AttemptNumber: parent.attempt, WorkspaceID: parent.workspaceID,
			},
		); err != nil {
			return err
		}
		if _, err := q.FailNestedSameWorkspaceRun(
			ctx,
			db.FailNestedSameWorkspaceRunParams{
				Failure: failureObject, FailedAt: failedAt, RunID: parent.id,
				EnvironmentID: parent.environmentID, WorkspaceID: parent.workspaceID,
				AttemptNumber: parent.attempt,
			},
		); err != nil {
			return err
		}
		if err := appendNestedResumeFailureEvent(ctx, q, parent, reason); err != nil {
			return err
		}
	}

	rootEdge := edges[0]
	root := lineage[0]
	_, err = q.CompleteSameWorkspaceChildFailure(
		ctx,
		db.CompleteSameWorkspaceChildFailureParams{
			CompletedAt: failedAt, ConditionState: db.WaitStateFailed,
			ConditionError: errorObject, ReasonCode: reason,
			RunWaitID: rootEdge.waitID, EnvironmentID: root.environmentID,
			ParentRunID: root.id, WorkspaceID: root.workspaceID,
			ParentAttemptNumber: root.attempt, ChildRunID: rootEdge.childRunID,
			ExpectedParentStateVersion: rootEdge.expectedParentVersion,
			ParentRunLeaseID:           rootEdge.priorRunLeaseID,
			SuspendCheckpointID:        rootEdge.suspendCheckpointID,
			ChildWriterGeneration: pgtype.Int8{
				Int64: rootEdge.childWriterGeneration, Valid: true,
			},
		},
	)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = desired_version + 1,
       desired_at = $2, desired_reason = 'nested_run_resume_lost',
       updated_at = $2
 WHERE id = $1
   AND desired_state = 'ready'`,
		physical.runtimeID,
		failedAt,
	); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
UPDATE workspace_mounts
   SET state = 'unmounting', stopped_at = COALESCE(stopped_at, $2),
       updated_at = $2
 WHERE id = $1
   AND state = 'mounted'`,
		physical.mountID,
		failedAt,
	)
	return err
}

func appendNestedResumeFailureEvent(
	ctx context.Context,
	q *db.Queries,
	run nestedResumeRun,
	reason pgtype.Text,
) error {
	payload, err := json.Marshal(map[string]string{"reason": reason.String})
	if err != nil {
		return err
	}
	_, err = q.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID: run.orgID, RunID: run.id, Kind: "run.failed",
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("append nested resume failure event: %w", err)
	}
	return nil
}
