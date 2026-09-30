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

type ComputerCommandCandidate struct {
	OrgID            pgtype.UUID
	CommandID        pgtype.UUID
	ComputerID       pgtype.UUID
	ExpectedRevision int64
}

type ComputerCommandRecoverer func(context.Context, ComputerCommandCandidate) error

type RunFinalization struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
}

type RunFinalizer func(context.Context, pgx.Tx, RunFinalization) error

type RevocationReconciler struct {
	db            db.TxDB
	queries       *db.Queries
	execRecoverer ComputerCommandRecoverer
	runFinalizer  RunFinalizer
}

func NewRevocationReconciler(
	database db.TxDB,
	execRecoverer ComputerCommandRecoverer,
	runFinalizer RunFinalizer,
) (*RevocationReconciler, error) {
	if database == nil {
		return nil, errors.New("secret revocation database is required")
	}
	if execRecoverer == nil {
		return nil, errors.New("command recoverer is required")
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
		pgvalue.MustUUIDValue(candidate.ComputerID),
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
		pgvalue.MustUUIDValue(candidate.ComputerID),
		secretID,
		revocationGeneration,
	)
	if err != nil {
		return err
	}
	if !valid {
		return tx.Commit(ctx)
	}
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{
		OrgID: candidate.OrgID, CommandID: candidate.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return fmt.Errorf("locate secret-revoked Command: %w", err)
	}
	if target.ComputerID != candidate.ComputerID {
		return errors.New("secret-revoked Command placement changed")
	}
	if _, err := q.LockComputerAdmissionAuthority(ctx, db.LockComputerAdmissionAuthorityParams{
		EnvironmentID: target.EnvironmentID, ID: target.ComputerID,
	}); err != nil {
		return fmt.Errorf("lock secret-revoked Computer: %w", err)
	}
	if _, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{
		EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock secret-revoked Instance: %w", err)
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{
		EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: candidate.ID,
	})
	if err != nil {
		return fmt.Errorf("lock secret-revoked Command: %w", err)
	}
	if !command.TerminalAt.Valid && !command.CancelRequestedAt.Valid {
		command, err = q.StopSecretRevokedComputerCommand(ctx, db.StopSecretRevokedComputerCommandParams{
			EnvironmentID: target.EnvironmentID, CommandID: command.ID, ExpectedRevision: command.Revision,
		})
		if err != nil {
			return fmt.Errorf("fail secret-revoked Command: %w", err)
		}
	}
	candidate.Revision = command.Revision
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
		ComputerCommandCandidate{
			OrgID:            candidate.OrgID,
			CommandID:        candidate.ID,
			ComputerID:       candidate.ComputerID,
			ExpectedRevision: candidate.Revision,
		},
	)
	if err != nil {
		return fmt.Errorf("recover secret-revoked Command: %w", err)
	}
	return nil
}

func lockAndValidateRevocation(
	ctx context.Context,
	q *db.Queries,
	computerID uuid.UUID,
	secretID uuid.UUID,
	revocationGeneration int64,
) (bool, error) {
	rows, err := q.LockComputerSecretsForAdmission(
		ctx,
		pgvalue.UUID(computerID),
	)
	if err != nil {
		return false, fmt.Errorf("lock computer secret set for revocation: %w", err)
	}
	if len(rows) > maxComputerSecretPlacements {
		return false, errors.New("computer secret placements exceed their bound")
	}
	for _, row := range rows {
		if row.SecretID == pgvalue.UUID(secretID) {
			return row.SecretStatus == "revoked" &&
				row.RevocationGeneration == revocationGeneration, nil
		}
	}
	return false, nil
}
