package computer

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pinning and publication take the spec lock. Deletion and waiter expiry only
// remove protection, so they cannot create an unobserved payload user here.
// Keep the newest usable image even if a newer sequence has stale Secrets.
const retirableSupersededImage = `i.root_id IS NOT NULL
 AND p.fenced_at IS NOT NULL AND p.disk_released_at IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=i.environment_id AND c.image_id=i.id
   AND (c.deleted_at IS NULL OR c.initial_root_id IS NOT NULL))
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=i.environment_id
   AND (c.image_build_id=p.id OR c.image_build_id=p.successor_of)
   AND c.initial_root_digest IS NULL AND c.preparation_failed_at IS NULL AND c.deleted_at IS NULL
   AND c.preparation_deadline_at>clock_timestamp()
   AND (c.preparation_max_age_ms IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=c.preparation_max_age_ms))
 AND i.id IS DISTINCT FROM (SELECT newest.id FROM images newest
   JOIN image_builds p ON (p.environment_id,p.id)=(newest.environment_id,newest.image_build_id)
   JOIN image_specs spec ON (spec.environment_id,spec.id)=(newest.environment_id,newest.image_spec_id)
   WHERE newest.environment_id=i.environment_id AND newest.image_spec_id=i.image_spec_id
   AND newest.root_id IS NOT NULL AND p.status='succeeded' AND ` + usableImageSecretsSQL + `
   ORDER BY newest.seq DESC LIMIT 1)`

// retireSupersededImages drops only payload ownership. Image/build identities
// and exposure lineage remain; graph collection independently proves unreachability.
// Discovery skips busy Secret owners before applying the batch bound. Mutation
// reacquires every lock and rechecks eligibility in its own transaction.
func retireSupersededImages(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT i.environment_id,i.image_spec_id,i.id FROM images i
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(i.environment_id,i.image_spec_id)
 WHERE `+retirableSupersededImage+`
 AND NOT EXISTS(SELECT 1 FROM secret_bindings b WHERE b.environment_id=i.environment_id AND b.image_spec_id=i.image_spec_id
   AND NOT EXISTS(SELECT 1 FROM secrets s WHERE (s.environment_id,s.id)=(b.environment_id,b.secret_id) FOR SHARE SKIP LOCKED))
 ORDER BY i.environment_id,i.id LIMIT 100 FOR NO KEY UPDATE OF spec,i SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct{ env, spec, image uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.env, &c.spec, &c.image)
		return c, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range candidates {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
				return err
			}
			if err := LockImageSpecSecrets(ctx, tx, c.env, c.spec); err != nil {
				return err
			}
			var locked uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM image_specs WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE SKIP LOCKED`, c.env, c.spec).Scan(&locked); err != nil {
				return err
			}
			var eligible bool
			if err := tx.QueryRow(ctx, `SELECT `+retirableSupersededImage+` FROM images i
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 WHERE i.environment_id=$1 AND i.id=$2 AND i.image_spec_id=$3 FOR NO KEY UPDATE OF i SKIP LOCKED`, c.env, c.image, c.spec).Scan(&eligible); err != nil {
				return err
			}
			if !eligible {
				return nil
			}
			_, err := tx.Exec(ctx, `UPDATE images SET root_id=NULL,payload_retired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, c.env, c.image)
			return err
		})
		cancel()
		var pgErr *pgconn.PgError
		if err != nil && !errors.Is(err, pgx.ErrNoRows) && !(errors.As(err, &pgErr) && pgErr.Code == "55P03") {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
