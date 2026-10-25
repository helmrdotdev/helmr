package agent

import (
	"context"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// SealDeletedAgent prevents catch-up after restoration. The caller holds the
// Environment gate. Only an actual due cursor is recorded, never future work.
func SealDeletedAgent(ctx context.Context, tx pgx.Tx, env, agent uuid.UUID) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO trigger_occurrences(environment_id,activation_id,agent_id,trigger_key,scheduled_at,disposition,reason,evaluated_at)
 SELECT environment_id,id,agent_id,trigger_key,next_fire_at,'missed','agent_deleted',clock_timestamp() FROM trigger_activations
 WHERE environment_id=$1 AND agent_id=$2 AND active_until IS NOT NULL AND next_fire_at<active_until AND next_fire_at<=statement_timestamp()
 ON CONFLICT(environment_id,agent_id,trigger_key,scheduled_at) DO NOTHING`, env, agent); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE trigger_activations SET next_fire_at=active_until WHERE environment_id=$1 AND agent_id=$2 AND active_until IS NOT NULL AND next_fire_at<active_until`, env, agent)
	return tag.RowsAffected(), err
}
