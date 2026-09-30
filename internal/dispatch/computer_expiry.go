package dispatch

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
)

// ReconcileComputerInstances settles preparation, revokes expired physical
// authority, commits ready restores and requests idle capture. Only cleanup proof releases capacity/pins.
func (d *Authority) ReconcileComputerInstances(ctx context.Context, limit int32) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(d.pool).ListExpiredComputerInstances(ctx, limit)
	if err != nil {
		return 0, err
	}
	var recovered int
	var failures []error
	for _, candidate := range candidates {
		changed, err := d.expireComputerInstance(ctx, candidate)
		if err != nil {
			failures = append(failures, err)
		} else if changed {
			recovered++
		}
	}
	if err := d.reconcileComputerPreparations(ctx, limit); err != nil {
		failures = append(failures, err)
	}
	if err := d.failPreparationBlockedMembers(ctx, limit); err != nil {
		failures = append(failures, err)
	}
	restored, err := d.reconcileComputerRestores(ctx, limit)
	if err != nil {
		failures = append(failures, err)
	}
	captured, err := d.captureIdleComputers(ctx, limit)
	if err != nil {
		failures = append(failures, err)
	}
	return recovered + restored + captured, errors.Join(failures...)
}

func (d *Authority) expireComputerInstance(ctx context.Context, candidate db.ComputerInstance) (bool, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	expired, err := computer.ExpireInstance(ctx, tx, candidate)
	if err != nil || !expired {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
