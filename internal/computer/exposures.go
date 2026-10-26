package computer

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// ExposureReceipt identifies only newly committed version pins. Existing pins
// survive audit expiry and never manufacture another exposure on a retry.
type ExposureReceipt struct {
	ID        uuid.UUID
	SecretID  uuid.UUID
	VersionID uuid.UUID
}

func RecordSessionExposures(ctx context.Context, tx pgx.Tx, env, session uuid.UUID, epoch int64, secrets []uuid.UUID) ([]ExposureReceipt, error) {
	rows, err := tx.Query(ctx, `INSERT INTO secret_exposures(id,environment_id,session_id,process_epoch,secret_id,version_id)
 SELECT uuidv7(),$1,$2,$3,s.id,s.current_version_id FROM secrets s WHERE s.environment_id=$1 AND s.id=ANY($4::uuid[]) AND s.status='active'
 ON CONFLICT(environment_id,session_id,process_epoch,secret_id) WHERE session_id IS NOT NULL DO NOTHING
 RETURNING id,secret_id,version_id`, env, session, epoch, secrets)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ExposureReceipt])
}

func RecordCommandExposures(ctx context.Context, tx pgx.Tx, env, command uuid.UUID, secrets []uuid.UUID) ([]ExposureReceipt, error) {
	rows, err := tx.Query(ctx, `INSERT INTO secret_exposures(id,environment_id,command_id,secret_id,version_id)
 SELECT uuidv7(),$1,$2,s.id,s.current_version_id FROM secrets s WHERE s.environment_id=$1 AND s.id=ANY($3::uuid[]) AND s.status='active'
 ON CONFLICT(environment_id,command_id,secret_id) WHERE command_id IS NOT NULL DO NOTHING
 RETURNING id,secret_id,version_id`, env, command, secrets)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ExposureReceipt])
}
