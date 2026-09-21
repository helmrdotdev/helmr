package dispatch

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
)

// RecoverExpiredRuntimeReservations revokes timed-out preparation or unused
// ready reservations, and retains revoked initial uploads for later cleanup.
// The returned count covers closed reservations, not abandoned upload records.
// Only the worker's physical cleanup proof releases capacity.
func (d *Authority) RecoverExpiredRuntimeReservations(ctx context.Context, limit int32) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(d.pool).ListExpiredRuntimeReservations(ctx, limit)
	if err != nil {
		return 0, err
	}
	var recovered int
	var failures []error
	for _, candidate := range candidates {
		changed, err := d.closeExpiredRuntimeReservation(ctx, candidate)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if changed {
			recovered++
		}
	}
	// This also catches terminal runtimes and cleared reservations that no longer
	// appear in the expiry scan. It must run even when no runtime was closed.
	if _, err := db.New(d.pool).AbandonRevokedComputerInitializations(ctx, limit); err != nil {
		failures = append(failures, fmt.Errorf("abandon revoked Computer initialization: %w", err))
	}
	return recovered, errors.Join(failures...)
}

func (d *Authority) closeExpiredRuntimeReservation(ctx context.Context, candidate db.RuntimeInstance) (bool, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var graph run.OwnedFinalization
	if candidate.ReservedRunID.Valid {
		// Take cancellation's graph order before touching the runtime. This is
		// revocation only: no new worker authority or capacity is granted.
		graph, err = run.LockOwnedFinalization(ctx, tx, run.OwnedFinalizationRequest{
			OrgID: uuid.UUID(candidate.OrgID.Bytes), ProjectID: uuid.UUID(candidate.ProjectID.Bytes),
			EnvironmentID: uuid.UUID(candidate.EnvironmentID.Bytes), RunID: uuid.UUID(candidate.ReservedRunID.Bytes),
		})
		if err != nil {
			return false, err
		}
	}
	closed, err := db.New(tx).CloseExpiredRuntimeReservation(ctx, db.CloseExpiredRuntimeReservationParams{
		ID: candidate.ID, DesiredVersion: candidate.DesiredVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if closed.ObservedState == db.RuntimeObservedStateAllocated && closed.ReservedRunID.Valid {
		var chargeable bool
		if err := tx.QueryRow(ctx, `SELECT status = 'queued' AND current_run_lease_id IS NULL AND current_attempt_number = $2 FROM runs WHERE id = $1`, closed.ReservedRunID, closed.ReservedAttemptNumber).Scan(&chargeable); err != nil {
			return false, err
		}
		if chargeable {
			if _, err := graph.ChargeRuntimePreparationFailure(ctx); err != nil {
				return false, fmt.Errorf("charge expired runtime preparation: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
