package agent

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Shared by successor creation and fresh attachment under the recipe and Secret
// owner locks. Fresh demand must not pre-empt an eligible attached chain's one
// replacement and force two sequential preparations for the same current inputs.
const preparationSuccessorEligibilitySQL = `p.successor_of IS NULL AND p.status='succeeded' AND p.fenced_at IS NOT NULL
 AND EXISTS(SELECT 1 FROM computer_images i WHERE i.environment_id=p.environment_id AND i.preparation_id=p.id)
 AND NOT EXISTS(SELECT 1 FROM computer_secret_bindings binding WHERE binding.environment_id=spec.environment_id AND binding.preparation_spec_id=spec.id AND NOT EXISTS(
   SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
   WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id AND x.secret_id=binding.secret_id AND s.status='active'))
 AND NOT EXISTS(SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
   WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id AND s.status<>'active')
 AND EXISTS(SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
   WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id AND s.current_version_id<>x.version_id)
 AND NOT EXISTS(SELECT 1 FROM computer_preparations other WHERE other.environment_id=p.environment_id AND other.preparation_spec_id=p.preparation_spec_id
   AND (other.status IN ('queued','running') OR (other.worker_host_id IS NOT NULL AND other.fenced_at IS NULL)))
 AND EXISTS(SELECT 1 FROM computers c JOIN computer_images i ON (i.environment_id,i.preparation_id)=(c.environment_id,c.preparation_id)
   WHERE c.environment_id=p.environment_id AND c.preparation_id=p.id AND c.initial_root_digest IS NULL AND c.preparation_failed_at IS NULL AND c.deleted_at IS NULL
   AND c.preparation_deadline_at>clock_timestamp()
   AND (c.preparation_max_age_ms IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=c.preparation_max_age_ms))`

// EnsurePreparationSuccessor consumes the one automatic replacement allowed for
// a successfully published image made stale solely by ordinary Secret rotation.
// It never changes original Computer attachments, deadlines or reservations.
func EnsurePreparationSuccessor(ctx context.Context, pool db.TxBeginner, env, predecessor uuid.UUID) (Preparation, error) {
	var result Preparation
	if env == uuid.Nil() || predecessor == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND retired_at IS NULL FOR NO KEY UPDATE`, env).Scan(&locked); err != nil {
			return err
		}
		p, err := readPreparation(ctx, tx, env, predecessor)
		if err != nil {
			return err
		}
		if err = lockPreparationImageSecrets(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		if err = lockPreparationSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		var existing uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM computer_preparations WHERE environment_id=$1 AND successor_of=$2`, env, predecessor).Scan(&existing)
		if err == nil {
			result, err = readPreparation(ctx, tx, env, existing)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT `+preparationSuccessorEligibilitySQL+`
 FROM computer_preparations p JOIN computer_preparation_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.preparation_spec_id)
 WHERE p.environment_id=$1 AND p.id=$2 FOR NO KEY UPDATE OF p`, env, predecessor).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrNotReady
		}
		// Pinning, attachment and waiter expiry all acquire the spec first. The
		// selected Computer set is therefore stable until this transaction ends.
		rows, err := tx.Query(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND preparation_id=$2
 AND initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL
 ORDER BY id FOR NO KEY UPDATE`, env, predecessor)
		if err != nil {
			return err
		}
		_, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		var deadline *time.Time
		if err = tx.QueryRow(ctx, `SELECT max(c.preparation_deadline_at) FROM computers c JOIN computer_images i ON (i.environment_id,i.preparation_id)=(c.environment_id,c.preparation_id)
 WHERE c.environment_id=$1 AND c.preparation_id=$2 AND c.initial_root_digest IS NULL AND c.preparation_failed_at IS NULL AND c.deleted_at IS NULL AND c.preparation_deadline_at>clock_timestamp()
 AND (c.preparation_max_age_ms IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=c.preparation_max_age_ms)`, env, predecessor).Scan(&deadline); err != nil {
			return err
		}
		if deadline == nil {
			return ErrNotReady
		}
		id := uuid.NewV7()
		_, err = tx.Exec(ctx, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,successor_of,retry_key,deadline_at)
 VALUES($1,$2,$3,$4,$5,$6)`, env, id, p.SpecID, predecessor, "successor:"+predecessor.String(), *deadline)
		if err != nil {
			return err
		}
		var live bool
		if err = tx.QueryRow(ctx, `SELECT deadline_at>clock_timestamp() FROM computer_preparations WHERE environment_id=$1 AND id=$2`, env, id).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNotReady
		}
		result, err = readPreparation(ctx, tx, env, id)
		return err
	})
	if err != nil {
		return Preparation{}, hideMissing(err)
	}
	return result, nil
}
