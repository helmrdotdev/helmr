package session

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

type actorInputReconcileDB interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type Reconciler struct {
	db actorInputReconcileDB
}

func NewReconciler(database actorInputReconcileDB) (*Reconciler, error) {
	if database == nil {
		return nil, errors.New("actor reconciliation database is required")
	}
	return &Reconciler{db: database}, nil
}

func (r *Reconciler) ReconcileClose(
	ctx context.Context,
	environmentID uuid.UUID,
	actorID uuid.UUID,
) (deferred bool, returnErr error) {
	locator, err := db.New(r.db).GetActor(ctx, db.GetActorParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		ID:            pgvalue.UUID(actorID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if locator.State != "closing" {
		return false, nil
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin actor close reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	bindings, err := q.LockWorkspaceSecretsForAdmission(ctx, locator.WorkspaceID)
	if err != nil {
		return false, err
	}
	actor, err := q.LockActorClose(ctx, db.LockActorCloseParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		SessionID:     pgvalue.UUID(actorID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if actor.WorkspaceID != locator.WorkspaceID {
		return false, ErrAuthority
	}
	_, deferred, err = ReconcileClose(ctx, q, actor, bindings)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return deferred, nil
}

// ReconcileInput repairs append delivery after the append transaction. It is safe to
// run after an in-transaction match or continuation because every transition is
// guarded by the Actor current-Run CAS and the pending Wait CAS.
func (r *Reconciler) ReconcileInput(
	ctx context.Context,
	environmentID uuid.UUID,
	actorID uuid.UUID,
	recordID uuid.UUID,
) (deferred bool, returnErr error) {
	locator, err := db.New(r.db).GetActor(ctx, db.GetActorParams{
		EnvironmentID: pgvalue.UUID(environmentID), ID: pgvalue.UUID(actorID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin actor input reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	bindings, err := q.LockWorkspaceSecretsForAdmission(ctx, locator.WorkspaceID)
	if err != nil {
		return false, err
	}
	actor, err := q.LockActorForInputReconcile(ctx, db.LockActorForInputReconcileParams{
		EnvironmentID: pgvalue.UUID(environmentID), SessionID: pgvalue.UUID(actorID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil || actor.WorkspaceID != locator.WorkspaceID {
		return false, ErrAuthority
	}
	var currentRun db.Run
	if actor.CurrentRunID.Valid {
		currentRun, err = q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{
			EnvironmentID: actor.EnvironmentID, RunID: actor.CurrentRunID, SessionID: actor.ID,
		})
		if err != nil {
			return false, ErrAuthority
		}
	}
	workspace, err := q.LockActorInputWorkspace(ctx, db.LockActorInputWorkspaceParams{
		EnvironmentID: actor.EnvironmentID, ID: actor.WorkspaceID, SessionID: actor.ID,
	})
	if err != nil {
		return false, ErrAuthority
	}
	if actor.CurrentRunID.Valid {
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
			RunID: currentRun.ID, Number: currentRun.CurrentAttemptNumber, WorkspaceID: workspace.ID,
		})
		if err != nil || attempt.TerminalAt.Valid {
			return false, ErrAuthority
		}
	}
	record, err := q.GetActorInputRecordByIDForUpdate(ctx, db.GetActorInputRecordByIDForUpdateParams{
		EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(recordID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil || !record.ID.Valid || uuid.UUID(record.ID.Bytes) != recordID {
		return false, ErrAuthority
	}
	wait, err := q.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
		EnvironmentID: actor.EnvironmentID, SessionID: actor.ID,
		RunID: currentRun.ID, AttemptNumber: currentRun.CurrentAttemptNumber,
		AfterInputSequence: pgtype.Int8{Int64: record.Sequence - 1, Valid: true},
	})
	if err == nil {
		if _, err := CompleteWait(ctx, q, wait, record); err != nil {
			return false, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if CanStartContinuation(actor) {
		if _, err := CreateContinuation(ctx, q, actor, workspace, bindings); errors.Is(err, pgx.ErrNoRows) {
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (r *Reconciler) ReconcileTimeouts(ctx context.Context, limit int32) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(r.db).ListPendingActorInputWaitTimeouts(ctx, limit)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, candidate := range candidates {
		if !candidate.SessionID.Valid || !candidate.AfterInputSequence.Valid {
			return resolved, ErrAuthority
		}
		tx, err := r.db.Begin(ctx)
		if err != nil {
			return resolved, err
		}
		q := db.New(tx)
		_, err = q.LockWorkspaceSecretsForAdmission(ctx, candidate.WorkspaceID)
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		actor, err := q.LockActorForInputReconcile(ctx, db.LockActorForInputReconcileParams{
			EnvironmentID: candidate.EnvironmentID, SessionID: candidate.SessionID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if !actor.CurrentRunID.Valid || actor.CurrentRunID != candidate.RunID {
			_ = tx.Rollback(context.Background())
			continue
		}
		run, err := q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{
			EnvironmentID: candidate.EnvironmentID, RunID: candidate.RunID, SessionID: candidate.SessionID,
		})
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		workspace, err := q.LockActorInputWorkspace(ctx, db.LockActorInputWorkspaceParams{
			EnvironmentID: candidate.EnvironmentID, ID: candidate.WorkspaceID, SessionID: candidate.SessionID,
		})
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
			RunID: run.ID, Number: run.CurrentAttemptNumber, WorkspaceID: workspace.ID,
		})
		if err != nil || attempt.TerminalAt.Valid {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		wait, err := q.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
			EnvironmentID: candidate.EnvironmentID, SessionID: candidate.SessionID,
			RunID: candidate.RunID, AttemptNumber: candidate.AttemptNumber,
			AfterInputSequence: candidate.AfterInputSequence,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if wait.ID != candidate.ID {
			_ = tx.Rollback(context.Background())
			continue
		}
		if !wait.TimeoutAt.Valid {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		now, err := q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if !now.Valid {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		if now.Time.Before(wait.TimeoutAt.Time) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if _, err := FailWait(ctx, q, wait, "wait_timeout"); err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if err := tx.Commit(ctx); err != nil {
			return resolved, err
		}
		resolved++
	}
	return resolved, nil
}
