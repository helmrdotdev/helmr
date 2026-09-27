package dispatch

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"strconv"
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
	q := db.New(tx)
	if _, err = q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ComputerID}); err != nil {
		return false, err
	}
	var id pgtype.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM computer_instances WHERE id=$1 AND computer_id=$2 FOR UPDATE`, candidate.ID, candidate.ComputerID).Scan(&id); err != nil {
		return false, err
	}
	_, err = q.ExpireComputerInstance(ctx, db.ExpireComputerInstanceParams{ID: candidate.ID, EnvironmentID: candidate.EnvironmentID, WriterGeneration: candidate.WriterGeneration, DesiredVersion: candidate.DesiredVersion})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Closing an unacknowledged destination revokes delivery, not the one-shot
	// checkpoint commitment. A late Worker cannot activate this destination and
	// the durable intent must no longer remain eligible for delivery.
	if _, err = tx.Exec(ctx, `UPDATE control_outbox o SET status='dead_lettered',claimed_by=NULL,claim_expires_at=NULL,last_error='Computer restore destination expired'
 FROM computer_checkpoints cp,computer_instances i
 WHERE i.id=$1 AND i.desired_state='closed' AND cp.id=i.source_checkpoint_id
 AND cp.resume_computer_instance_id=i.id AND cp.resume_committed_at IS NOT NULL
 AND o.topic=$2 AND o.status IN ('pending','claimed')
 AND o.payload->>'checkpoint_id'=cp.id::text AND o.payload->>'computer_instance_id'=i.id::text
 AND o.payload->>'desired_version'=$3 AND o.payload->>'writer_generation'=$4`,
		candidate.ID, ComputerRestoreActivationTopic, strconv.FormatInt(candidate.DesiredVersion, 10), strconv.FormatInt(candidate.WriterGeneration, 10)); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
