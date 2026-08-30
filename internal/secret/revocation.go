package secret

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type database interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type WorkspaceExecCandidate struct {
	OrgID                pgtype.UUID
	ProcessID            pgtype.UUID
	WorkspaceID          pgtype.UUID
	ExpectedStateVersion int64
}

type WorkspaceExecRecoverer func(context.Context, WorkspaceExecCandidate) error

type RunFinalization struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
}

type RunFinalizer func(context.Context, pgx.Tx, RunFinalization) error

type RevocationReconciler struct {
	db            database
	queries       *db.Queries
	execRecoverer WorkspaceExecRecoverer
	runFinalizer  RunFinalizer
}

func NewRevocationReconciler(
	database database,
	execRecoverer WorkspaceExecRecoverer,
	runFinalizer RunFinalizer,
) (*RevocationReconciler, error) {
	if database == nil {
		return nil, errors.New("secret revocation database is required")
	}
	if execRecoverer == nil {
		return nil, errors.New("workspace exec recoverer is required")
	}
	if runFinalizer == nil {
		return nil, errors.New("run finalizer is required")
	}
	return &RevocationReconciler{
		db:            database,
		queries:       db.New(database),
		execRecoverer: execRecoverer,
		runFinalizer:  runFinalizer,
	}, nil
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
	if environmentID == uuid.Nil() || secretID == uuid.Nil() ||
		revocationGeneration <= 0 {
		return 0, errors.New("secret revocation authority is required")
	}
	if limit <= 0 {
		return 0, errors.New("secret revocation batch limit must be positive")
	}
	runs, err := r.queries.ListSecretRevocationRuns(
		ctx,
		db.ListSecretRevocationRunsParams{
			SecretID:             pgvalue.UUID(secretID),
			RevocationGeneration: revocationGeneration,
			EnvironmentID:        pgvalue.UUID(environmentID),
			RowLimit:             limit,
		},
	)
	if err != nil {
		return 0, fmt.Errorf("list secret-revoked run candidates: %w", err)
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
	processes, err := r.queries.ListSecretRevocationProcesses(
		ctx,
		db.ListSecretRevocationProcessesParams{
			SecretID:             pgvalue.UUID(secretID),
			RevocationGeneration: revocationGeneration,
			EnvironmentID:        pgvalue.UUID(environmentID),
			RowLimit:             limit - int32(examined),
		},
	)
	if err != nil {
		return examined, fmt.Errorf(
			"list secret-revoked process candidates: %w",
			err,
		)
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

func (r *RevocationReconciler) failRun(
	ctx context.Context,
	candidate db.ListSecretRevocationRunsRow,
	secretID uuid.UUID,
	revocationGeneration int64,
) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin secret-revoked run reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	valid, err := lockAndValidateRevocation(
		ctx,
		q,
		pgvalue.MustUUIDValue(candidate.WorkspaceID),
		secretID,
		revocationGeneration,
	)
	if err != nil {
		return err
	}
	if !valid {
		return tx.Commit(ctx)
	}
	err = r.runFinalizer(
		ctx,
		tx,
		RunFinalization{
			OrgID:         pgvalue.MustUUIDValue(candidate.OrgID),
			ProjectID:     pgvalue.MustUUIDValue(candidate.ProjectID),
			EnvironmentID: pgvalue.MustUUIDValue(candidate.EnvironmentID),
			RunID:         pgvalue.MustUUIDValue(candidate.ID),
		},
	)
	if err != nil {
		return fmt.Errorf("fail secret-revoked run graph: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit secret-revoked run reconciliation: %w", err)
	}
	return nil
}

func (r *RevocationReconciler) fenceProcess(
	ctx context.Context,
	candidate db.ListSecretRevocationProcessesRow,
	secretID uuid.UUID,
	revocationGeneration int64,
) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin secret-revoked process fence: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	valid, err := lockAndValidateRevocation(
		ctx,
		q,
		pgvalue.MustUUIDValue(candidate.WorkspaceID),
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
			OrgID:       candidate.OrgID,
			WorkspaceID: candidate.WorkspaceID,
		},
	); errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	} else if err != nil {
		return fmt.Errorf("lock secret-revoked process workspace: %w", err)
	}
	authority, err := q.LockWorkspaceExecSecretRevocationAuthority(
		ctx,
		db.LockWorkspaceExecSecretRevocationAuthorityParams{
			OrgID:                candidate.OrgID,
			ProcessID:            candidate.ID,
			WorkspaceID:          candidate.WorkspaceID,
			ExpectedStateVersion: candidate.StateVersion,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit existing secret-revoked process fence: %w", err)
		}
		return r.recoverProcess(ctx, candidate)
	} else if err != nil {
		return fmt.Errorf("lock secret-revoked process authority: %w", err)
	}
	if _, err := q.FenceWorkspaceExecLeaseForSecretRevocation(
		ctx,
		db.FenceWorkspaceExecLeaseForSecretRevocationParams{
			LeaseID:   authority.WorkspaceLease.ID,
			ProcessID: authority.WorkspaceProcess.ID,
		},
	); err != nil {
		return fmt.Errorf("fence secret-revoked workspace exec lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit secret-revoked process fence: %w", err)
	}
	return r.recoverProcess(ctx, candidate)
}

func (r *RevocationReconciler) recoverProcess(
	ctx context.Context,
	candidate db.ListSecretRevocationProcessesRow,
) error {
	err := r.execRecoverer(
		ctx,
		WorkspaceExecCandidate{
			OrgID:                candidate.OrgID,
			ProcessID:            candidate.ID,
			WorkspaceID:          candidate.WorkspaceID,
			ExpectedStateVersion: candidate.StateVersion,
		},
	)
	if err != nil {
		return fmt.Errorf("recover secret-revoked workspace exec: %w", err)
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
		return false, fmt.Errorf("lock workspace secret set for revocation: %w", err)
	}
	if len(rows) > maxWorkspaceSecretPlacements {
		return false, errors.New("workspace secret placements exceed their bound")
	}
	for _, row := range rows {
		if row.SecretID == pgvalue.UUID(secretID) {
			return row.SecretState == "revoked" &&
				row.RevocationGeneration == revocationGeneration, nil
		}
	}
	return false, nil
}
