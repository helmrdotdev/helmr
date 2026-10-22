package slack

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// The adapter compares and renders the same snapshot. Sampling again after
// rendering could acknowledge a change that the published card never showed.
// Aliases s and t are the front Session and its Slack thread.
const lifecycleStateSQL = `jsonb_build_object(
 'computer_id',s.computer_id,'status',s.status,'policy',s.suspension,
 'idle',s.close_after_idle_ms,'muted',t.input_muted,
 'checkpoint',COALESCE((SELECT cp.status FROM checkpoints cp WHERE cp.environment_id=s.environment_id AND cp.computer_id=s.computer_id AND cp.status IN ('capturing','sealed','ready','restoring','aborting') ORDER BY cp.created_at DESC LIMIT 1),''),
 'awake',EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.status='active' AND l.fenced_at IS NULL),
 'control',COALESCE((SELECT jsonb_build_object('kind',c.kind,'status',c.status,'failure',COALESCE(c.error_code,'')) FROM computer_controls c WHERE c.environment_id=s.environment_id AND c.computer_id=s.computer_id ORDER BY c.accepted_at DESC,c.id DESC LIMIT 1),'{}'::jsonb))`

type lifecycleState struct {
	ComputerID *uuid.UUID `json:"computer_id"`
	Status     string     `json:"status"`
	Policy     string     `json:"policy"`
	Idle       *int64     `json:"idle"`
	Muted      bool       `json:"muted"`
	Checkpoint string     `json:"checkpoint"`
	Awake      bool       `json:"awake"`
	Control    struct {
		Kind    string `json:"kind"`
		Status  string `json:"status"`
		Failure string `json:"failure"`
	} `json:"control"`
}

func readLifecycleState(ctx context.Context, tx pgx.Tx, target controlTarget) (lifecycleState, []byte, error) {
	var state lifecycleState
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT `+lifecycleStateSQL+` FROM sessions s JOIN slack_threads t ON t.id=$3 AND t.front_session_id=s.id WHERE s.environment_id=$1 AND s.id=$2`, target.Environment, target.Session, target.Thread).Scan(&raw)
	if err != nil {
		return state, nil, err
	}
	err = json.Unmarshal(raw, &state)
	return state, raw, err
}
