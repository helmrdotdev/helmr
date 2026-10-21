package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"time"
	"uuid"
)

// A seed is optional launch data. Keep current-generation content while its
// image payload is usable, plus the latest finalized candidate for promotion.
// Retain every artifact a live VM may still page.
// Historical deployment/image metadata remains after its payload is retired.
const obsoleteBootSeed = `(EXISTS(SELECT 1 FROM environments e JOIN deployments old ON old.environment_id=e.id AND old.id=b.deployment_id WHERE e.id=b.environment_id AND b.deployment_id IS DISTINCT FROM e.current_deployment_id AND EXISTS(SELECT 1 FROM deployments newer WHERE newer.environment_id=e.id AND (newer.created_at,newer.id)>(old.created_at,old.id)))
 OR (b.image_id IS NOT NULL AND NOT (` + usableBootSeedImageSQL + `))
 OR EXISTS(SELECT 1 FROM boot_seed_builds newer JOIN images ni ON (ni.environment_id,ni.id)=(newer.environment_id,newer.image_id)
 JOIN images old ON (old.environment_id,old.id)=(b.environment_id,b.image_id)
 WHERE newer.environment_id=b.environment_id AND newer.deployment_id=b.deployment_id AND newer.image_spec_id=b.image_spec_id
 AND newer.cpu_millis=b.cpu_millis AND newer.memory_bytes=b.memory_bytes AND ni.seq>old.seq
 AND newer.status='succeeded' AND newer.payload_retired_at IS NULL))`

const retirableBootSeed = `b.payload_retired_at IS NULL AND (b.worker_host_id IS NULL OR b.fenced_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=b.environment_id AND l.boot_seed_build_id=b.id AND l.fenced_at IS NULL)
 AND (b.status='failed' OR ` + obsoleteBootSeed + `)`

func retireBootSeeds(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT b.environment_id,b.id FROM boot_seed_builds b WHERE `+retirableBootSeed+` ORDER BY b.environment_id,b.id LIMIT 100`)
	if err != nil {
		return err
	}
	type candidate struct{ env, id uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.env, &c.id)
		return c, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range candidates {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := db.RunTx(callCtx, database, func(tx pgx.Tx) error {
			if _, err := tx.Exec(callCtx, `SET LOCAL lock_timeout='100ms'`); err != nil {
				return err
			}
			if _, err := LockBootSeedBuild(callCtx, tx, c.env, c.id); err != nil {
				return err
			}
			var eligible bool
			if err := tx.QueryRow(callCtx, `SELECT `+retirableBootSeed+` FROM boot_seed_builds b WHERE environment_id=$1 AND id=$2`, c.env, c.id).Scan(&eligible); err != nil {
				return err
			}
			if !eligible {
				return nil
			}
			// Never-claimed waiting/queued work has no physical writer to stop.
			if err := TerminateBootSeedBuild(callCtx, tx, c.env, c.id, "boot_seed_superseded"); err != nil {
				return err
			}
			var org uuid.UUID
			if err := tx.QueryRow(callCtx, `SELECT org_id FROM environments WHERE id=$1`, c.env).Scan(&org); err != nil {
				return err
			}
			rows, err := tx.Query(callCtx, `WITH removed AS (DELETE FROM boot_seed_objects WHERE environment_id=$1 AND build_id=$2 RETURNING digest) SELECT DISTINCT digest FROM removed ORDER BY digest`, c.env, c.id)
			if err != nil {
				return err
			}
			digests, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, digest := range digests {
				if err := releaseComputerCASMembership(callCtx, tx, org, digest); err != nil {
					return err
				}
			}
			_, err = tx.Exec(callCtx, `UPDATE boot_seed_builds SET payload_retired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, c.env, c.id)
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
