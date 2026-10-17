package computer

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RefreshImageSpec consumes one due refresh interval. A usable attempt coalesces
// background and foreground demand; an unusable or unfenced attempt leaves the
// spec due. This does not change any existing Computer's pin or attached chain.
func RefreshImageSpec(ctx context.Context, database db.TxBeginner, env, spec uuid.UUID) (ImageBuild, error) {
	var result ImageBuild
	if env == uuid.Nil() || spec == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var timeout int64
		if err := tx.QueryRow(ctx, `SELECT preparation_timeout_ms FROM environments WHERE id=$1 FOR NO KEY UPDATE`, env).Scan(&timeout); err != nil {
			return err
		}
		if err := LockImageSpecSecrets(ctx, tx, env, spec); err != nil {
			return err
		}
		if err := LockImageSpec(ctx, tx, env, spec); err != nil {
			return err
		}
		var due, now time.Time
		if err := tx.QueryRow(ctx, `SELECT next_refresh_at,clock_timestamp() FROM image_specs
 WHERE environment_id=$1 AND id=$2 AND next_refresh_at<=clock_timestamp()`, env, spec).Scan(&due, &now); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM secret_bindings b
 WHERE b.environment_id=$1 AND b.image_spec_id=$2 AND NOT EXISTS(
 SELECT 1 FROM secrets s WHERE s.environment_id=b.environment_id AND s.id=b.secret_id AND s.status='active'))`, env, spec).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrNotReady
		}
		var attempt uuid.UUID
		var usable bool
		err := tx.QueryRow(ctx, `SELECT p.id,`+usableImageBuildSQL+` FROM image_builds p WHERE p.environment_id=$1 AND p.image_spec_id=$2
 AND (p.status IN ('queued','running') OR (p.worker_host_id IS NOT NULL AND p.fenced_at IS NULL))`, env, spec).Scan(&attempt, &usable)
		if errors.Is(err, pgx.ErrNoRows) {
			var successorPending bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_builds p
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.image_spec_id)
 WHERE p.environment_id=$1 AND p.image_spec_id=$2
 AND NOT EXISTS(SELECT 1 FROM image_builds successor WHERE successor.environment_id=p.environment_id AND successor.successor_of=p.id)
 AND `+imageBuildSuccessorEligibilitySQL+`)`, env, spec).Scan(&successorPending); err != nil {
				return err
			}
			if successorPending {
				return ErrNotReady
			}
			attempt = uuid.NewV7()
			if _, err := tx.Exec(ctx, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at)
 VALUES($1,$2,$3,$4,$5)`, env, attempt, spec, "refresh:"+due.UTC().Format(time.RFC3339Nano), now.Add(time.Duration(timeout)*time.Millisecond)); err != nil {
				return err
			}
			if err := notifyImageBuildChanged(ctx, tx, env, attempt); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !usable {
			return ErrNotReady
		}
		if _, err := tx.Exec(ctx, `UPDATE image_specs SET next_refresh_at=clock_timestamp()+refresh_every_ms*interval '1 millisecond'
 WHERE environment_id=$1 AND id=$2`, env, spec); err != nil {
			return err
		}
		result, err = ReadImageBuild(ctx, tx, env, attempt)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ImageBuild{}, ErrNotReady
	}
	if err != nil {
		return ImageBuild{}, err
	}
	return result, nil
}
