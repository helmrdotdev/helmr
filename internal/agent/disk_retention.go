package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RunComputerDiskRetention releases domain attachments before the independent
// physical CAS sweep. Historical identities and publication receipts remain.
func RunComputerDiskRetention(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("computer disk retention requires database and logger")
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := ReconcileComputerDiskRetention(passCtx, database)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "Computer disk retention failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Candidate predicates remove stable ineligible owners before the batch bound.
// Every candidate is checked again under its publication/restore owner lock.
func ReconcileComputerDiskRetention(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT l.environment_id,l.computer_id,l.epoch FROM computer_leases l JOIN computers owner ON owner.environment_id=l.environment_id AND owner.id=l.computer_id
 WHERE l.fenced_at IS NOT NULL AND l.base_root_id IS NOT NULL AND l.disk_released_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=l.environment_id AND s.computer_id=l.computer_id AND s.computer_lease_epoch=l.epoch AND s.status IN ('requested','captured'))
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints k WHERE k.environment_id=l.environment_id AND k.computer_id=l.computer_id AND (k.source_lease_epoch=l.epoch OR k.target_lease_epoch=l.epoch) AND k.status NOT IN ('cancelled','lost') AND k.controls_reconciled_at IS NULL)
 ORDER BY l.environment_id,l.computer_id,l.epoch LIMIT 100 FOR NO KEY UPDATE OF owner,l SKIP LOCKED`)
	if err != nil {
		return err
	}
	leases, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (computerLifecyclePosition, error) {
		var p computerLifecyclePosition
		err := row.Scan(&p.environment, &p.computer, &p.epoch)
		return p, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, p := range leases {
		if err := tryReleaseComputerLeaseDisk(ctx, database, p.environment, p.computer, p.epoch); err != nil && !errors.Is(err, ErrNotReady) {
			failures = append(failures, err)
		}
	}
	if err := retireDeletedComputerRoots(ctx, database); err != nil {
		failures = append(failures, err)
	}
	rows, err = database.Query(ctx, `SELECT s.environment_id,s.computer_id,s.id FROM computer_saves s JOIN computers owner ON owner.environment_id=s.environment_id AND owner.id=s.computer_id WHERE `+retirableSave+` ORDER BY s.environment_id,s.id LIMIT 100 FOR NO KEY UPDATE OF owner,s SKIP LOCKED`)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	type candidate struct{ environment, computer, save uuid.UUID }
	saves, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.environment, &c.computer, &c.save)
		return c, err
	})
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, c := range saves {
		if err := retireComputerSave(ctx, database, c.environment, c.computer, c.save); err != nil {
			failures = append(failures, err)
		}
	}
	if err := releaseCheckpointObjects(ctx, database); err != nil {
		failures = append(failures, err)
	}
	if err := releasePreparationDiskAttachments(ctx, database); err != nil {
		failures = append(failures, err)
	}
	if err := retireRevokedComputerImages(ctx, database); err != nil {
		failures = append(failures, err)
	}
	if err := collectComputerDiskGraph(ctx, database); err != nil {
		failures = append(failures, err)
	}
	if err := releaseDeletedComputerStorage(ctx, database); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// A source's physical closure also joins its upload work. Neither an expired
// writer nor an unresolved publication is a retention clock. Completed Turns
// retain their own save identity without retaining every old disk version.
const retirableSave = `s.status IN ('published','failed')
 AND (s.root_id IS NOT NULL OR EXISTS(SELECT 1 FROM computer_object_pins p WHERE p.environment_id=s.environment_id AND p.save_id=s.id))
 AND EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.epoch=s.computer_lease_epoch AND l.fenced_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=s.environment_id AND c.recovery_save_id=s.id)
 AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.id=s.turn_id AND t.status IN ('running','finalizing'))
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=s.environment_id AND l.restored_from_save_id=s.id AND l.disk_released_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints k WHERE k.environment_id=s.environment_id AND k.disk_save_id=s.id AND (k.status NOT IN ('cancelled','lost') AND k.controls_reconciled_at IS NULL))`

func retireComputerSave(ctx context.Context, database db.TxBeginner, environment, computer, save uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE SKIP LOCKED`, environment, computer).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND id=$3 FOR NO KEY UPDATE SKIP LOCKED`, environment, computer, save).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT `+retirableSave+` FROM computer_saves s WHERE s.environment_id=$1 AND s.id=$2`, environment, save).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE computer_saves SET root_id=NULL,payload_retired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2 AND status='published' AND root_id IS NOT NULL`, environment, save); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM computer_object_pins WHERE environment_id=$1 AND save_id=$2`, environment, save)
		return err
	})
}

// Collection skips busy owners; a later pass retries them without starving
// unrelated owners behind the same ordered discovery prefix.
func tryReleaseComputerLeaseDisk(ctx context.Context, database db.TxBeginner, environment, computer uuid.UUID, epoch int64) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE SKIP LOCKED`, environment, computer).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var selected int64
		if err := tx.QueryRow(ctx, `SELECT epoch FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 FOR NO KEY UPDATE SKIP LOCKED`, environment, computer, epoch).Scan(&selected); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		return releaseComputerLeaseDisk(ctx, tx, environment, computer, epoch)
	})
}
