package dispatch

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Run/Session owners reconcile their own authority. This existing maintenance
// lane repairs only unowned Computers; it never restarts a terminated command.
func (d *Authority) reconcileUnownedComputers(ctx context.Context, limit int32) error {
	rows, err := d.pool.Query(ctx, `SELECT id FROM computers
 WHERE owner_run_id IS NULL AND owner_session_id IS NULL
 AND status IN ('active','recovery_required')
 AND (
   (status='recovery_required' AND recovery_preparation_count<8 AND recovery_failure IS NULL)
   OR (recovery_failure IS NOT NULL AND EXISTS(SELECT 1 FROM workspace_processes p WHERE p.workspace_id=computers.id AND p.status='pending'))
   OR (recovery_completed_at IS NULL AND recovery_preparation_count=8 AND
       (status='active' OR EXISTS(SELECT 1 FROM workspace_processes p WHERE p.workspace_id=computers.id AND p.status='pending')))
   OR (EXISTS(SELECT 1 FROM workspace_mounts m JOIN runtime_instances r ON r.id=m.runtime_instance_id
       WHERE m.workspace_id=computers.id AND r.reclaimed_at IS NOT NULL AND m.status IN ('mounting','mounted','unmounting')))
 )
 AND NOT EXISTS(SELECT 1 FROM runtime_instances r WHERE r.workspace_id=computers.id AND r.reclaimed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM workspace_leases l WHERE l.workspace_id=computers.id AND l.status IN ('active','releasing'))
 AND NOT EXISTS(SELECT 1 FROM workspace_processes p WHERE p.workspace_id=computers.id AND p.status IN ('starting','running','exit_requested'))
 ORDER BY updated_at,id LIMIT $1`, limit)
	if err != nil {
		return err
	}
	var ids []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err = d.reconcileUnownedComputer(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (d *Authority) reconcileUnownedComputer(ctx context.Context, id pgtype.UUID) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	// Placement locks pending processes before the Computer. Lock the same set,
	// then revalidate it after taking Computer admission authority.
	rows, err := tx.Query(ctx, `SELECT org_id,id,revision FROM workspace_processes WHERE workspace_id=$1 AND status='pending' ORDER BY id FOR UPDATE`, id)
	if err != nil {
		return err
	}
	var pending []ReadyWorkspaceExecCandidate
	for rows.Next() {
		var p ReadyWorkspaceExecCandidate
		if err = rows.Scan(&p.OrgID, &p.ProcessID, &p.ExpectedRevision); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var source pgtype.UUID
	var count int32
	var failure []byte
	err = tx.QueryRow(ctx, `SELECT CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_version_id,head_version_id) ELSE head_version_id END,CASE WHEN recovery_completed_at IS NULL THEN recovery_preparation_count ELSE 0 END,recovery_failure FROM computers WHERE id=$1
 AND owner_run_id IS NULL AND owner_session_id IS NULL AND status IN ('active','recovery_required') FOR UPDATE`, id).Scan(&source, &count, &failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var pendingCount int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM workspace_processes WHERE workspace_id=$1 AND status='pending'`, id).Scan(&pendingCount); err != nil {
		return err
	}
	if pendingCount != len(pending) {
		return nil
	}
	var safe bool
	if err = tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM runtime_instances WHERE workspace_id=$1 AND reclaimed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM workspace_leases WHERE workspace_id=$1 AND status IN ('active','releasing'))
 AND NOT EXISTS(SELECT 1 FROM workspace_processes WHERE workspace_id=$1 AND status IN ('starting','running','exit_requested'))
 AND ($3=8 OR $4::boolean OR EXISTS(SELECT 1 FROM computer_versions v JOIN computers c ON c.id=v.computer_id
 WHERE c.id=$1 AND v.id=$2 AND c.head_version_id=v.id AND v.status='committed' AND v.payload_not_retired))`, id, source, count, len(failure) > 0).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_mounts SET status='failed',failed_at=now(),terminal_at=now(),
 terminal_reason_code='execution_lost',updated_at=now() WHERE workspace_id=$1 AND status IN ('mounting','mounted','unmounting')`, id); err != nil {
		return err
	}
	if count == 8 || len(failure) > 0 {
		reason := "computer_recovery_exhausted"
		if len(failure) > 0 {
			var detail struct {
				Code string `json:"code"`
			}
			if err = json.Unmarshal(failure, &detail); err != nil {
				return err
			}
			reason = detail.Code
		} else {
			failure = []byte(`{"code":"computer_recovery_exhausted","message":"Computer preparation limit reached","details":{}}`)
		}
		for _, p := range pending {
			if err = failPendingWorkspaceExec(ctx, tx, p, reason, failure); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE computers SET status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',recovery_failure=$2,revision=revision+1,updated_at=now() WHERE id=$1`, id, failure); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE computers SET status='active',desired_state='active',dirty_state='clean',revision=revision+1,updated_at=now() WHERE id=$1 AND status='recovery_required'`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
