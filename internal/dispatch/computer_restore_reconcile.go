package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Restores compete for the same queue slots as fresh work. A rejected batch rolls
// back every grant and is retried by the reconciliation loop until preparation
// expires; it must neither reserve parked slots nor partially activate members.
func (d *Authority) reconcileComputerRestores(ctx context.Context, limit int32) (int, error) {
	var committed int
	var after pgtype.UUID
	var failures []error
	for committed < int(limit) {
		rows, err := d.pool.Query(ctx, `SELECT i.id,i.worker_host_id,i.worker_group_id,i.worker_epoch,i.desired_version
 FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id
 WHERE ($2::uuid IS NULL OR i.id>$2) AND i.admission_state='restoring'
 AND i.desired_state='ready' AND i.observed_state='ready' AND i.observed_desired_version=i.desired_version
 AND i.mount_state='mounted' AND i.reclaimed_at IS NULL
 AND i.preparation_expires_at>clock_timestamp() AND i.writer_expires_at>clock_timestamp()
 AND cp.status='ready' AND cp.resume_committed_at IS NULL AND (cp.expires_at IS NULL OR cp.expires_at>clock_timestamp())
 ORDER BY i.id LIMIT $1`, limit-int32(committed), after)
		if err != nil {
			return committed, errors.Join(append(failures, err)...)
		}
		var candidates []ComputerPreparationFence
		for rows.Next() {
			var fence ComputerPreparationFence
			if err = rows.Scan(&fence.RuntimeID, &fence.WorkerID, &fence.WorkerGroupID, &fence.WorkerEpoch, &fence.DesiredVersion); err != nil {
				rows.Close()
				return committed, errors.Join(append(failures, err)...)
			}
			candidates = append(candidates, fence)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return committed, errors.Join(append(failures, err)...)
		}
		if len(candidates) == 0 {
			break
		}
		for _, fence := range candidates {
			after = fence.RuntimeID
			err := d.commitReadyComputerRestore(ctx, fence)
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrCapacityUnavailable) {
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("commit Computer restore: %w", err))
			} else {
				committed++
			}
		}
	}
	return committed, errors.Join(failures...)
}

func (d *Authority) commitReadyComputerRestore(ctx context.Context, fence ComputerPreparationFence) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = d.CommitComputerRestore(ctx, tx, fence); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
