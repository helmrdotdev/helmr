package slack

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/jackc/pgx/v5"
)

var errFormIdentityUnlinked = errors.New("the Slack answer opener requires identity linking")

// readFormQuestion is called only after transport and control-signature checks.
// Current link/membership and binding locks span the read and any receipt write;
// opening a modal happens after this transaction has released all locks.
func readFormQuestion(ctx context.Context, tx pgx.Tx, app, team, actor string, control controlEnvelope, source time.Time) (agent.AskView, int64, error) {
	var empty agent.AskView
	if !validControl(control) || (control.Action != "open_answer" && control.Action != "answer") || actor == "" || (control.Action == "answer" && control.Actor != actor) {
		return empty, 0, errControlInvalid
	}
	if control.Action == "answer" {
		opened, err := slackMessageTime(control.SourceTimestamp)
		if err != nil || !opened.Equal(source) {
			return empty, 0, errControlInvalid
		}
	}
	target := control.Target
	var channel uuid.UUID
	var enabled time.Time
	err := tx.QueryRow(ctx, `SELECT c.id,pub.created_at FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id AND t.front_session_id=p.session_id JOIN slack_channels c ON c.id=t.channel_id JOIN agent_publications pub ON pub.id=c.publication_id WHERE p.id=$1 AND p.thread_id=$2 AND p.environment_id=$3 AND p.session_id=$4 AND c.installation_id=$5 AND t.deleted_at IS NULL`, target.Participant, target.Thread, target.Environment, target.Session, target.Installation).Scan(&channel, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, 0, errControlInvalid
	}
	if err != nil {
		return empty, 0, err
	}
	active, err := agent.LockSlackChannels(ctx, tx, target.Environment, []uuid.UUID{channel})
	if err != nil {
		return empty, 0, err
	}
	if !active || source.Before(enabled) {
		return empty, 0, errControlInvalid
	}
	var credential int64
	var bot string
	var authorized, connected time.Time
	err = tx.QueryRow(ctx, `SELECT credential_revision,bot_user_id,authorized_at,connected_at FROM slack_installations WHERE id=$1 AND app_id=$2 AND team_id=$3`, target.Installation, app, team).Scan(&credential, &bot, &authorized, &connected)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, 0, errControlInvalid
	}
	if err != nil {
		return empty, 0, err
	}
	if actor == bot || source.Before(authorized) || source.Before(connected) {
		return empty, 0, errControlInvalid
	}
	_, code, err := linkedHuman(ctx, tx, team, actor, target.Environment, source, true)
	if err != nil {
		return empty, 0, err
	}
	if code != "" {
		if code == "identity_unlinked" {
			return empty, 0, errFormIdentityUnlinked
		}
		return empty, 0, errControlInvalid
	}
	var view agent.AskView
	err = tx.QueryRow(ctx, `SELECT id,status,question FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, target.Environment, target.Session, target.Turn, target.Ask).Scan(&view.ID, &view.Status, &view.Question)
	if err != nil {
		return empty, 0, err
	}
	var unexpired bool
	if err = tx.QueryRow(ctx, `SELECT $1>clock_timestamp()`, time.Unix(control.ExpiresAt, 0)).Scan(&unexpired); err != nil {
		return empty, 0, err
	}
	if !unexpired {
		return empty, 0, errControlExpired
	}
	return view, credential, nil
}
