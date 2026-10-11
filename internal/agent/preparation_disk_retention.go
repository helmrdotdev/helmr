package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func releasePreparationDiskAttachments(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT p.environment_id,p.preparation_spec_id,p.id FROM computer_preparations p
 JOIN computer_preparation_specs spec ON spec.environment_id=p.environment_id AND spec.id=p.preparation_spec_id
 WHERE p.disk_released_at IS NULL AND p.write_key_id IS NOT NULL AND p.fenced_at IS NOT NULL AND p.status IN ('succeeded','failed')
 AND (p.status='failed' OR EXISTS(SELECT 1 FROM computer_images i WHERE i.environment_id=p.environment_id AND i.preparation_id=p.id))
 ORDER BY p.environment_id,p.id LIMIT 100 FOR NO KEY UPDATE OF spec,p SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct{ environment, spec, id uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.environment, &c.spec, &c.id)
		return c, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range candidates {
		err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
			var locked uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM computer_preparation_specs WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE SKIP LOCKED`, c.environment, c.spec).Scan(&locked); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			var eligible bool
			if err := tx.QueryRow(ctx, `SELECT p.disk_released_at IS NULL AND p.write_key_id IS NOT NULL AND p.fenced_at IS NOT NULL AND p.status IN ('succeeded','failed')
 AND (p.status='failed' OR EXISTS(SELECT 1 FROM computer_images i WHERE i.environment_id=p.environment_id AND i.preparation_id=p.id))
 FROM computer_preparations p WHERE p.environment_id=$1 AND p.id=$2 AND p.preparation_spec_id=$3 FOR NO KEY UPDATE OF p SKIP LOCKED`, c.environment, c.id, c.spec).Scan(&eligible); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			if !eligible {
				return nil
			}
			if _, err := tx.Exec(ctx, `DELETE FROM computer_object_pins WHERE environment_id=$1 AND preparation_id=$2`, c.environment, c.id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE computer_preparations SET disk_released_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, c.environment, c.id)
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
