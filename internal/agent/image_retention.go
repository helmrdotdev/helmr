package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Revocation permanently denies this image to all future allocations. Its
// identity and preparation exposure history remain for existing Computers'
// revocation checks. Their initial roots independently retain usable storage.
// An older but still eligible image is not an eviction candidate here.
const retirableImage = `i.root_id IS NOT NULL
 AND EXISTS(SELECT 1 FROM computer_preparations p WHERE p.environment_id=i.environment_id AND p.id=i.preparation_id AND p.fenced_at IS NOT NULL AND p.disk_released_at IS NOT NULL)
 AND EXISTS(SELECT 1 FROM secret_exposures x JOIN secrets s ON s.environment_id=x.environment_id AND s.id=x.secret_id WHERE x.environment_id=i.environment_id AND x.preparation_id=i.preparation_id AND s.status='revoked')`

func retireRevokedComputerImages(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT i.environment_id,i.preparation_spec_id,i.id FROM computer_images i
 JOIN computer_preparation_specs spec ON spec.environment_id=i.environment_id AND spec.id=i.preparation_spec_id
 WHERE `+retirableImage+` ORDER BY i.environment_id,i.id LIMIT 100 FOR NO KEY UPDATE OF spec,i SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct{ environment, spec, image uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.environment, &c.spec, &c.image)
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
			if err := tx.QueryRow(ctx, `SELECT `+retirableImage+` FROM computer_images i WHERE i.environment_id=$1 AND i.id=$2 AND i.preparation_spec_id=$3 FOR NO KEY UPDATE OF i SKIP LOCKED`, c.environment, c.image, c.spec).Scan(&eligible); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			if !eligible {
				return nil
			}
			_, err := tx.Exec(ctx, `UPDATE computer_images SET root_id=NULL,payload_retired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, c.environment, c.image)
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
