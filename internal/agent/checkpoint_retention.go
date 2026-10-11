package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const retirableCheckpoint = `(k.status IN ('cancelled','lost') OR (k.status='consumed' AND k.controls_reconciled_at IS NOT NULL))
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=k.environment_id AND l.computer_id=k.computer_id AND (l.epoch=k.source_lease_epoch OR l.epoch=k.target_lease_epoch) AND l.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=k.environment_id AND s.computer_id=k.computer_id AND (s.computer_lease_epoch=k.source_lease_epoch OR s.computer_lease_epoch=k.target_lease_epoch) AND s.status IN ('requested','captured'))
 AND EXISTS(SELECT 1 FROM computer_checkpoint_objects o WHERE o.environment_id=k.environment_id AND o.checkpoint_id=k.id)`

func releaseCheckpointObjects(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT k.environment_id,k.computer_id,k.id FROM computer_checkpoints k
 JOIN computers c ON c.environment_id=k.environment_id AND c.id=k.computer_id
 WHERE `+retirableCheckpoint+` ORDER BY k.environment_id,k.id LIMIT 100 FOR NO KEY UPDATE OF c,k SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct{ environment, computer, checkpoint uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.environment, &c.computer, &c.checkpoint)
		return c, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range candidates {
		err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
			// Publication and restore also hold this Computer before the checkpoint.
			// No Session membership or controls change in this terminal-only operation.
			var locked uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE SKIP LOCKED`, c.environment, c.computer).Scan(&locked); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			var eligible bool
			if err := tx.QueryRow(ctx, `SELECT `+retirableCheckpoint+` FROM computer_checkpoints k WHERE k.environment_id=$1 AND k.id=$2 AND k.computer_id=$3 FOR NO KEY UPDATE OF k SKIP LOCKED`, c.environment, c.checkpoint, c.computer).Scan(&eligible); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			if !eligible {
				return nil
			}
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
				return err
			}
			var org uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, c.environment).Scan(&org); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `WITH removed AS (DELETE FROM computer_checkpoint_objects WHERE environment_id=$1 AND checkpoint_id=$2 RETURNING digest) SELECT DISTINCT digest FROM removed ORDER BY digest`, c.environment, c.checkpoint)
			if err != nil {
				return err
			}
			digests, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, digest := range digests {
				if err := releaseComputerCASMembership(ctx, tx, org, digest); err != nil {
					return err
				}
			}
			return nil
		})
		var pgErr *pgconn.PgError
		if err != nil && !attachmentWon(err) && !(errors.As(err, &pgErr) && pgErr.Code == "55P03") {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
