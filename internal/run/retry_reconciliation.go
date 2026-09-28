package run

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type RetryReconciliationDB interface {
	db.DBTX
	CancellationDB
}

type RetryReconciler struct{ db RetryReconciliationDB }

func NewRetryReconciler(database RetryReconciliationDB) *RetryReconciler {
	return &RetryReconciler{db: database}
}

func (r *RetryReconciler) ReadyRunRetries(ctx context.Context, limit int32) ([]db.ReadyRunRetriesRow, error) {
	rows, err := r.db.Query(ctx, `SELECT r.id,r.org_id,r.project_id,r.environment_id FROM runs r JOIN computers c ON c.id=r.computer_id
WHERE r.status='retry_delayed' AND c.recovery_failure IS NOT NULL
ORDER BY r.retry_at,r.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, org, project, env uuid.UUID }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.org, &c.project, &c.env); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var failures []error
	for _, c := range candidates {
		if err = r.reconcile(ctx, c.id, c.org, c.project, c.env); err != nil {
			failures = append(failures, err)
		}
	}
	ready, err := db.New(r.db).ReadyRunRetries(ctx, limit)
	return ready, errors.Join(append(failures, err)...)
}

func (r *RetryReconciler) reconcile(ctx context.Context, id, org, project, env uuid.UUID) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{RunID: id, OrgID: org, ProjectID: project, EnvironmentID: env})
	if err != nil {
		return err
	}
	// Physical recovery owns the Computer. A retry only settles its logical
	// scope when that owner has published an unrecoverable outcome.
	var reason, message string
	err = tx.QueryRow(ctx, `SELECT c.recovery_failure->>'code',c.recovery_failure->>'message'
        FROM runs r JOIN computers c ON c.id=r.computer_id
        WHERE r.id=$1 AND r.status='retry_delayed' AND c.recovery_failure IS NOT NULL`, id).Scan(&reason, &message)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = graph.failCurrentForLeaseLoss(ctx, id, executionLeaseLoss{reason: reason, state: db.RunLeaseStatusLost}, message, db.RunStatusSystemFailed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
