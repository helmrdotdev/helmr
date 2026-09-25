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
	rows, err := r.db.Query(ctx, `SELECT r.id,r.org_id,r.project_id,r.environment_id FROM runs r JOIN computers c ON c.owner_run_id=r.id AND c.id=r.workspace_id
WHERE r.status='retry_delayed' AND c.status='recovery_required'
AND NOT EXISTS(SELECT 1 FROM runtime_instances i WHERE i.workspace_id=c.id AND i.reclaimed_at IS NULL)
AND NOT EXISTS(SELECT 1 FROM workspace_leases l WHERE l.workspace_id=c.id AND l.status IN ('active','releasing'))
AND NOT EXISTS(SELECT 1 FROM workspace_processes p WHERE p.workspace_id=c.id AND p.status IN ('starting','running','exit_requested'))
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
	var safe bool
	err = tx.QueryRow(ctx, `SELECT c.status='recovery_required' AND c.owner_run_id=r.id AND r.status='retry_delayed'
AND NOT EXISTS(SELECT 1 FROM runtime_instances i WHERE i.workspace_id=c.id AND i.reclaimed_at IS NULL)
AND NOT EXISTS(SELECT 1 FROM workspace_leases l WHERE l.workspace_id=c.id AND l.status IN ('active','releasing'))
AND NOT EXISTS(SELECT 1 FROM workspace_processes p WHERE p.workspace_id=c.id AND p.status IN ('starting','running','exit_requested'))
FROM runs r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, id).Scan(&safe)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !safe {
		return nil
	}
	var unavailable bool
	var reason, message string
	if err = tx.QueryRow(ctx, `SELECT c.recovery_failure IS NOT NULL OR c.recovery_preparation_count>=8,coalesce(c.recovery_failure->>'code','computer_recovery_exhausted'),coalesce(c.recovery_failure->>'message','Computer cannot be prepared for Task retry') FROM runs r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, id).Scan(&unavailable, &reason, &message); err != nil {
		return err
	}
	if unavailable {
		if err = graph.failCurrentForLeaseLoss(ctx, id, executionLeaseLoss{reason: reason, state: db.RunLeaseStatusLost}, message, db.RunStatusSystemFailed); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Logical pending children are not live writers. Only physical authority blocks
	// admission; all old continuations were invalidated in the loss transaction.
	if _, err = tx.Exec(ctx, `UPDATE workspace_mounts m SET status='failed',failed_at=now(),terminal_at=now(),terminal_reason_code='execution_lost',updated_at=now()
FROM runs r WHERE r.id=$1 AND m.workspace_id=r.workspace_id AND m.status IN ('mounting','mounted','unmounting')`, id); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE computers c SET status='active',desired_state='active',dirty_state='clean',revision=c.revision+1,updated_at=now()
FROM runs r,computer_versions v WHERE r.id=$1 AND c.id=r.workspace_id AND c.owner_run_id=r.id AND c.status='recovery_required'
AND v.id=c.head_version_id AND v.workspace_id=c.id AND v.status='committed' AND v.payload_available
AND c.recovery_version_id=v.id AND r.base_workspace_version_id=v.id`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return cancellationAuthority("retry source no longer matches retained Computer", nil)
	}
	return tx.Commit(ctx)
}
