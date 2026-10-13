package agent

import (
	"context"
	"encoding/json"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// Activation starts a declared trigger at a promotion cutover with its next
// occurrence and, when applicable, its pinned Slack channel binding.
type Activation struct {
	SlackChannelBindingID *uuid.UUID `json:"slack_channel_binding_id"`
	Agent                 uuid.UUID  `json:"agent"`
	Key                   string     `json:"key"`
	Next                  time.Time  `json:"next"`
	ID                    uuid.UUID  `json:"id"`
}

// ReplaceActivations closes the preceding promotion's intervals and opens the
// selected Deployment's intervals. The caller holds the Environment lock and
// the Deployment share lock, and commits its current-Deployment switch in this
// transaction. Existing occurrences retain their original activation identity.
func ReplaceActivations(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time, entries []Activation) error {
	if _, err := tx.Exec(ctx, `UPDATE trigger_activations SET active_until=$2 WHERE environment_id=$1 AND active_until IS NULL`, env, cutover); err != nil {
		return err
	}
	if len(entries) > 0 {
		raw, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,next_fire_at,slack_channel_binding_id)
 SELECT $1,v.id,v.agent,$2,v.key,$3,v.next,v.slack_channel_binding_id
 FROM jsonb_to_recordset($4::jsonb) AS v(id uuid,agent uuid,key text,next timestamptz,slack_channel_binding_id uuid)`, env, deployment, cutover, raw)
		if err != nil {
			return err
		}
	}
	return nil
}
