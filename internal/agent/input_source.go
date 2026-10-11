package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// Native Slack provenance is read from its accepted receipt, never authored
// input. Slack admission and the receipt commit in one transaction. Terminal
// history retention cannot remove this payload while input is executable.
func loadSlackInputSource(ctx context.Context, tx pgx.Tx, env, turn, message uuid.UUID, source *TurnSource) error {
	var payload []byte
	var slack SlackInputSource
	err := tx.QueryRow(ctx, `SELECT i.team_id,r.slack_user_id,r.payload FROM slack_requests r
 JOIN slack_installations i ON i.id=r.installation_id
 WHERE r.environment_id=$1 AND r.turn_id=$2 AND r.status='accepted'
 AND r.message_id IS NOT DISTINCT FROM $3::uuid
 AND r.operation IN ('start','enqueue','send') ORDER BY r.id LIMIT 1`, env, turn, nullableID(message)).Scan(&slack.TeamID, &slack.UserID, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var gesture struct {
		Channel   string `json:"channel"`
		Timestamp string `json:"message_ts"`
		Thread    string `json:"thread_ts"`
	}
	if err = json.Unmarshal(payload, &gesture); err != nil {
		return err
	}
	if gesture.Channel == "" || gesture.Timestamp == "" {
		return errors.New("accepted Slack input has no source reference")
	}
	slack.ChannelID, slack.MessageTS, slack.ThreadTS = gesture.Channel, gesture.Timestamp, gesture.Thread
	if slack.ThreadTS == "" {
		slack.ThreadTS = slack.MessageTS
	}
	source.Slack = &slack
	return nil
}

func loadMessageSource(ctx context.Context, tx pgx.Tx, env uuid.UUID, message *MessageDelivery) error {
	if err := tx.QueryRow(ctx, `SELECT caller_kind,CASE WHEN caller_kind='session' THEN caller_id END,origin_turn_id FROM turn_messages WHERE environment_id=$1 AND id=$2`, env, message.ID).Scan(&message.Source.Kind, &message.Source.RequesterSessionID, &message.Source.OriginTurnID); err != nil {
		return err
	}
	return loadSlackInputSource(ctx, tx, env, message.TurnID, message.ID, &message.Source)
}
