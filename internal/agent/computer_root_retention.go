package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

const retirableComputerRoots = `c.deleted_at IS NOT NULL AND (c.initial_root_id IS NOT NULL OR c.recovery_save_id IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=c.environment_id AND p.computer_id=c.id AND p.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('requested','captured'))
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints k WHERE k.environment_id=c.environment_id AND k.computer_id=c.id AND k.status NOT IN ('cancelled','lost') AND k.controls_reconciled_at IS NULL)`

func retireDeletedComputerRoots(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id FROM computers c WHERE `+retirableComputerRoots+` ORDER BY c.environment_id,c.id LIMIT 100 FOR NO KEY UPDATE OF c SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct{ environment, computer uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.environment, &c.computer)
		return c, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range candidates {
		err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
			var eligible bool
			if err := tx.QueryRow(ctx, `SELECT `+retirableComputerRoots+` FROM computers c WHERE c.environment_id=$1 AND c.id=$2 FOR NO KEY UPDATE OF c SKIP LOCKED`, c.environment, c.computer).Scan(&eligible); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			if !eligible {
				return nil
			}
			_, err := tx.Exec(ctx, `UPDATE computers SET initial_payload_retired_at=CASE WHEN initial_root_id IS NOT NULL THEN clock_timestamp() ELSE initial_payload_retired_at END,initial_root_id=NULL,recovery_save_id=NULL WHERE environment_id=$1 AND id=$2`, c.environment, c.computer)
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
