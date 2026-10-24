package slack

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

const directMessageDescription = "DMs create personal Helmr Sessions. People with session viewing or management access to this Agent can also access them. New message: new Session. Thread reply: continue."

func validConversationID(channel string) bool {
	return definition.ValidSlackChannelID(channel) || slackroute.IsDirectMessage(channel)
}

// Opening a tab does not create a Session or a work receipt. Setting the same
// prompts again is harmless, including on Slack's retries. Agent View requires
// omitting thread_ts: suggestions belong to the Messages tab itself.
func (h EventHandler) receiveAppHome(ctx context.Context, envelope eventEnvelope) error {
	e := envelope.Event
	if e.Tab != "messages" || !slackroute.IsDirectMessage(e.Channel) || e.User == "" || e.BotID != "" || e.AppID != "" || envelope.TeamID == "" {
		return nil
	}
	var installation uuid.UUID
	var revision int64
	err := db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT i.id,i.credential_revision FROM slack_installations i
 JOIN slack_connections c ON c.slack_installation_id=i.id JOIN slack_app_registrations r ON r.id=c.slack_app_registration_id
 WHERE i.app_id=$1 AND i.team_id=$2 AND i.bot_user_id<>$3 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL
 AND c.revoked_at IS NULL AND r.retired_at IS NULL AND i.granted_scopes @> $4::text[]`, h.AppID, envelope.TeamID, e.User, slackroute.RequiredScopes()).Scan(&installation, &revision)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if h.Client == nil {
		h.logPromptFailure(ctx, "client_unavailable")
		return nil
	}
	if _, err = h.Client.verifyConversation(ctx, installation, revision, envelope.TeamID, e.Channel, e.User); err != nil {
		h.logPromptFailure(ctx, "conversation_unavailable")
		return nil
	}
	payload, _ := json.Marshal(map[string]any{
		"channel_id": e.Channel,
		"title":      directMessageDescription,
		"prompts":    []map[string]string{{"title": "Get started", "message": "What can you help me with?"}},
	})
	result := h.Client.Call(ctx, installation, revision, "assistant.threads.setSuggestedPrompts", payload)
	if result.Disposition != Acknowledged {
		h.logPromptFailure(ctx, result.Code)
	}
	return nil
}

// Guidance is best effort. Remote failures must not consume the Events API's
// delivery failure budget or turn opening a tab into retried work.
func (h EventHandler) logPromptFailure(ctx context.Context, code string) {
	if h.Log != nil {
		h.Log.WarnContext(ctx, "Slack Messages-tab guidance unavailable", "code", code)
	}
}

// verifyControlConversation checks the same retained peer as message admission before
// consuming a control receipt. Transactional admission rechecks peer identity,
// current grants and human authority under the route's locks.
func verifyControlConversation(ctx context.Context, pool db.TxBeginner, client *WebClient, request, thread uuid.UUID) (bool, error) {
	var installation, channelID, env uuid.UUID
	var active bool
	var channel, team, actor, peer, peerTeam string
	var revision int64
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT i.id,i.credential_revision,c.slack_channel_id,i.team_id,r.slack_user_id,COALESCE(t.recipient_user_id,''),COALESCE(t.recipient_team_id,''),c.id,c.environment_id
 FROM slack_requests r JOIN slack_threads t ON t.id=COALESCE($2::uuid,r.thread_id)
 JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id AND c.installation_id=r.installation_id
 JOIN slack_installations i ON i.id=c.installation_id WHERE r.id=$1`, request, nullableTurn(thread)).Scan(&installation, &revision, &channel, &team, &actor, &peer, &peerTeam, &channelID, &env)
		if err != nil {
			return err
		}
		active, err = slackroute.LockChannels(ctx, tx, env, []uuid.UUID{channelID})
		return err
	})
	// Ordinary admission owns nonexistent, stale and non-DM targets.
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !active {
		// Locked admission handles revoked routes without waiting for remote access.
		return true, nil
	}
	reject := func() (bool, error) {
		return false, db.RunTx(ctx, pool, func(tx pgx.Tx) error { return rejectMessage(ctx, tx, request, "permission_denied") })
	}
	if slackroute.IsDirectMessage(channel) && (peer == "" || peer != actor || peerTeam != team) {
		return reject()
	}
	if client == nil {
		return false, ErrChannelVerification
	}
	_, err = client.verifyConversation(ctx, installation, revision, team, channel, actor)
	if err != nil {
		var failure *channelVerificationFailure
		typed := errors.As(err, &failure)
		if typed && failure.result.Disposition == Rejected || !typed && errors.Is(err, ErrChannelVerification) {
			return reject()
		}
		return false, err
	}
	return true, nil
}
