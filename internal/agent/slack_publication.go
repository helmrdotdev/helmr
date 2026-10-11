package agent

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

type slackPublication struct {
	channel *uuid.UUID
	active  bool
}

func slackContentNonempty(content json.RawMessage) bool {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return false
	}
	for _, part := range parts {
		if part.Type == "json" || part.Text != "" {
			return true
		}
	}
	return false
}

// Every Slack conversation is bound at admission. Output can never create a
// replacement thread or change its sender after disconnection.
func lockSlackPublication(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) (slackPublication, error) {
	var p slackPublication
	err := tx.QueryRow(ctx, `SELECT t.channel_id FROM sessions s JOIN slack_threads t ON (t.environment_id,t.front_session_id)=(s.environment_id,s.root_session_id) JOIN slack_thread_sources src ON src.thread_id=t.id AND (src.environment_id,src.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2`, env, session).Scan(&p.channel)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.active, err = LockSlackChannels(ctx, tx, env, []uuid.UUID{*p.channel})
	return p, err
}

func (p slackPublication) accept(ctx context.Context, tx pgx.Tx, env, session, turn uuid.UUID, sequence int64) error {
	if p.channel == nil || p.active {
		return nil
	}
	data, _ := json.Marshal(map[string]any{"reason": "route_unavailable", "source_seq": sequence})
	return eventData(ctx, tx, env, session, turn, "slack.delivery_unavailable", data)
}
