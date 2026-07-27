package secret

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type database interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type WorkspaceExecCandidate struct {
	OrgID                pgtype.UUID
	ProcessID            pgtype.UUID
	WorkspaceID          pgtype.UUID
	ExpectedStateVersion int64
}

type WorkspaceExecRecoverer func(context.Context, WorkspaceExecCandidate) error

func (recover WorkspaceExecRecoverer) RecoverWorkspaceExec(
	ctx context.Context,
	candidate WorkspaceExecCandidate,
) error {
	return recover(ctx, candidate)
}

type RevocationReconciler struct {
	db            database
	execRecoverer WorkspaceExecRecoverer
}

type runCandidate struct {
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	workspaceID   uuid.UUID
	runID         uuid.UUID
}

type processCandidate struct {
	orgID        uuid.UUID
	workspaceID  uuid.UUID
	processID    uuid.UUID
	stateVersion int64
}

func NewRevocationReconciler(
	database database,
	execRecoverer WorkspaceExecRecoverer,
) (*RevocationReconciler, error) {
	if database == nil {
		return nil, errors.New("Secret revocation database is required")
	}
	if execRecoverer == nil {
		return nil, errors.New("Workspace exec recoverer is required")
	}
	return &RevocationReconciler{db: database, execRecoverer: execRecoverer}, nil
}

// ReconcileBatch advances the execution fences affected by one committed
// Secret revocation. Each execution is handled in its own bounded transaction.
func (r *RevocationReconciler) ReconcileBatch(
	ctx context.Context,
	environmentID uuid.UUID,
	secretID uuid.UUID,
	revocationGeneration int64,
	limit int32,
) (int, error) {
	if environmentID == uuid.Nil || secretID == uuid.Nil ||
		revocationGeneration <= 0 {
		return 0, errors.New("Secret revocation authority is required")
	}
	if limit <= 0 {
		return 0, errors.New("Secret revocation batch limit must be positive")
	}
	runs, err := r.listRunCandidates(
		ctx,
		environmentID,
		secretID,
		revocationGeneration,
		limit,
	)
	if err != nil {
		return 0, err
	}
	examined := 0
	for _, candidate := range runs {
		if err := r.failRun(
			ctx,
			candidate,
			secretID,
			revocationGeneration,
		); err != nil {
			return examined, err
		}
		examined++
	}
	if examined >= int(limit) {
		return examined, nil
	}
	processes, err := r.listProcessCandidates(
		ctx,
		environmentID,
		secretID,
		revocationGeneration,
		limit-int32(examined),
	)
	if err != nil {
		return examined, err
	}
	for _, candidate := range processes {
		if err := r.fenceProcess(
			ctx,
			candidate,
			secretID,
			revocationGeneration,
		); err != nil {
			return examined, err
		}
		examined++
	}
	return examined, nil
}

func (r *RevocationReconciler) listRunCandidates(
	ctx context.Context,
	environmentID uuid.UUID,
	secretID uuid.UUID,
	revocationGeneration int64,
	limit int32,
) ([]runCandidate, error) {
	rows, err := r.db.Query(ctx, `
WITH RECURSIVE affected_runs AS MATERIALIZED (
    SELECT DISTINCT runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.workspace_id,
           runs.id,
           runs.parent_run_id,
           runs.created_at
      FROM secret_resolutions
      JOIN runs
        ON runs.id = secret_resolutions.run_id
       AND runs.workspace_id = secret_resolutions.workspace_id
       AND runs.current_attempt_number = secret_resolutions.attempt_number
     WHERE secret_resolutions.secret_id = $1
       AND secret_resolutions.revocation_generation < $2
       AND runs.environment_id = $3
       AND runs.status IN (
           'queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested'
       )
), ancestor_walk AS (
    SELECT affected_runs.id AS candidate_id,
           affected_runs.parent_run_id,
           0 AS depth
      FROM affected_runs
    UNION ALL
    SELECT ancestor_walk.candidate_id,
           parent.parent_run_id,
           ancestor_walk.depth + 1
      FROM ancestor_walk
      JOIN runs AS parent ON parent.id = ancestor_walk.parent_run_id
), candidate_depths AS (
    SELECT candidate_id, max(depth) AS depth
      FROM ancestor_walk
     GROUP BY candidate_id
)
SELECT affected_runs.org_id,
       affected_runs.project_id,
       affected_runs.environment_id,
       affected_runs.workspace_id,
       affected_runs.id,
       affected_runs.created_at
  FROM affected_runs
  JOIN candidate_depths ON candidate_depths.candidate_id = affected_runs.id
 ORDER BY candidate_depths.depth, affected_runs.created_at, affected_runs.id
 LIMIT $4`,
		secretID,
		revocationGeneration,
		environmentID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list Secret-revoked Run candidates: %w", err)
	}
	defer rows.Close()
	candidates := make([]runCandidate, 0, limit)
	for rows.Next() {
		var candidate runCandidate
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(
			&candidate.orgID,
			&candidate.projectID,
			&candidate.environmentID,
			&candidate.workspaceID,
			&candidate.runID,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan Secret-revoked Run candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Secret-revoked Run candidates: %w", err)
	}
	return candidates, nil
}

func (r *RevocationReconciler) listProcessCandidates(
	ctx context.Context,
	environmentID uuid.UUID,
	secretID uuid.UUID,
	revocationGeneration int64,
	limit int32,
) ([]processCandidate, error) {
	rows, err := r.db.Query(ctx, `
SELECT DISTINCT workspace_processes.org_id,
       workspace_processes.workspace_id,
       workspace_processes.id,
       workspace_processes.state_version,
       workspace_processes.created_at
  FROM secret_resolutions
  JOIN workspace_processes
    ON workspace_processes.id = secret_resolutions.process_id
   AND workspace_processes.workspace_id = secret_resolutions.workspace_id
 WHERE secret_resolutions.secret_id = $1
   AND secret_resolutions.revocation_generation < $2
   AND workspace_processes.environment_id = $3
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
 ORDER BY workspace_processes.created_at, workspace_processes.id
 LIMIT $4`,
		secretID,
		revocationGeneration,
		environmentID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list Secret-revoked process candidates: %w", err)
	}
	defer rows.Close()
	candidates := make([]processCandidate, 0, limit)
	for rows.Next() {
		var candidate processCandidate
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(
			&candidate.orgID,
			&candidate.workspaceID,
			&candidate.processID,
			&candidate.stateVersion,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan Secret-revoked process candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Secret-revoked process candidates: %w", err)
	}
	return candidates, nil
}

func (r *RevocationReconciler) failRun(
	ctx context.Context,
	candidate runCandidate,
	secretID uuid.UUID,
	revocationGeneration int64,
) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Secret-revoked Run reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	valid, err := lockAndValidateRevocation(
		ctx,
		q,
		candidate.workspaceID,
		secretID,
		revocationGeneration,
	)
	if err != nil {
		return err
	}
	if !valid {
		return tx.Commit(ctx)
	}
	graph, err := db.LockOwnedRunFinalizationGraphInTransaction(
		ctx,
		tx,
		db.OwnedRunFinalizationGraphRequest{
			OrgID:         candidate.orgID,
			ProjectID:     candidate.projectID,
			EnvironmentID: candidate.environmentID,
			RunID:         candidate.runID,
		},
	)
	if err != nil {
		return fmt.Errorf("lock Secret-revoked Run graph: %w", err)
	}
	if _, err := graph.FailCurrentForSecretRevocation(ctx); err != nil {
		return fmt.Errorf("fail Secret-revoked Run graph: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Secret-revoked Run reconciliation: %w", err)
	}
	return nil
}

func (r *RevocationReconciler) fenceProcess(
	ctx context.Context,
	candidate processCandidate,
	secretID uuid.UUID,
	revocationGeneration int64,
) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Secret-revoked process fence: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	valid, err := lockAndValidateRevocation(
		ctx,
		q,
		candidate.workspaceID,
		secretID,
		revocationGeneration,
	)
	if err != nil {
		return err
	}
	if !valid {
		return tx.Commit(ctx)
	}
	if _, err := q.LockWorkspaceExecFailureWorkspace(
		ctx,
		db.LockWorkspaceExecFailureWorkspaceParams{
			OrgID:       pgvalue.UUID(candidate.orgID),
			WorkspaceID: pgvalue.UUID(candidate.workspaceID),
		},
	); errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	} else if err != nil {
		return fmt.Errorf("lock Secret-revoked process Workspace: %w", err)
	}
	authority, err := q.LockWorkspaceExecSecretRevocationAuthority(
		ctx,
		db.LockWorkspaceExecSecretRevocationAuthorityParams{
			OrgID:                pgvalue.UUID(candidate.orgID),
			ProcessID:            pgvalue.UUID(candidate.processID),
			WorkspaceID:          pgvalue.UUID(candidate.workspaceID),
			ExpectedStateVersion: candidate.stateVersion,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit existing Secret-revoked process fence: %w", err)
		}
		return r.recoverProcess(ctx, candidate)
	} else if err != nil {
		return fmt.Errorf("lock Secret-revoked process authority: %w", err)
	}
	if _, err := q.FenceWorkspaceExecLeaseForSecretRevocation(
		ctx,
		db.FenceWorkspaceExecLeaseForSecretRevocationParams{
			LeaseID:   authority.WorkspaceLease.ID,
			ProcessID: authority.WorkspaceProcess.ID,
		},
	); err != nil {
		return fmt.Errorf("fence Secret-revoked Workspace exec Lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Secret-revoked process fence: %w", err)
	}
	return r.recoverProcess(ctx, candidate)
}

func (r *RevocationReconciler) recoverProcess(
	ctx context.Context,
	candidate processCandidate,
) error {
	err := r.execRecoverer.RecoverWorkspaceExec(
		ctx,
		WorkspaceExecCandidate{
			OrgID:                pgvalue.UUID(candidate.orgID),
			ProcessID:            pgvalue.UUID(candidate.processID),
			WorkspaceID:          pgvalue.UUID(candidate.workspaceID),
			ExpectedStateVersion: candidate.stateVersion,
		},
	)
	if err != nil {
		return fmt.Errorf("recover Secret-revoked Workspace exec: %w", err)
	}
	return nil
}

func lockAndValidateRevocation(
	ctx context.Context,
	q *db.Queries,
	workspaceID uuid.UUID,
	secretID uuid.UUID,
	revocationGeneration int64,
) (bool, error) {
	rows, err := q.LockWorkspaceSecretsForAdmission(
		ctx,
		pgvalue.UUID(workspaceID),
	)
	if err != nil {
		return false, fmt.Errorf("lock Workspace Secret set for revocation: %w", err)
	}
	if len(rows) > maxWorkspaceSecretPlacements {
		return false, errors.New("Workspace Secret placements exceed their bound")
	}
	for _, row := range rows {
		if row.SecretID == pgvalue.UUID(secretID) {
			return row.SecretState == "revoked" &&
				row.RevocationGeneration == revocationGeneration, nil
		}
	}
	return false, nil
}
