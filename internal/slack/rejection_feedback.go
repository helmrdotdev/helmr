package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type rejectionFeedback struct {
	installation         uuid.UUID
	credentialRevision   int64
	payload              []byte
	channel, actor, team string
}

// takeRejectedFeedback disposes terminal rejected input or accepted input-control
// feedback and its private hint
// before external I/O. Identity and digest remain for retry comparison.
// Feedback is best effort: a crash may lose one, but neither an unknown
// HTTP outcome nor a duplicate receipt may repeat it. No core owner is invented.
func takeRejectedFeedback(ctx context.Context, pool db.TxBeginner, config ProjectionConfig) (*rejectionFeedback, bool, error) {
	var intent *rejectionFeedback
	found := false
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var id, installation uuid.UUID
		var raw []byte
		var actor, code, team string
		var active bool
		var revision int64
		err := tx.QueryRow(ctx, `SELECT r.id,r.installation_id,r.payload,r.slack_user_id,COALESCE(r.error,r.operation,''),i.credential_revision,i.team_id,
 i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND r.source_occurred_at>=i.authorized_at AND r.source_occurred_at>=i.connected_at
 FROM slack_requests r JOIN slack_installations i ON i.id=r.installation_id
 WHERE (r.status='rejected' OR (r.status='accepted' AND r.operation IN ('mute','unmute','sleep','wake','close','help','hibernate','confirm_close'))) AND r.payload_expired_at IS NULL ORDER BY r.finished_at,r.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&id, &installation, &raw, &actor, &code, &revision, &team, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if _, err = tx.Exec(ctx, `UPDATE slack_requests SET payload=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			return err
		}
		if !active || len(raw) == 0 || code == "input_ignored" {
			return nil
		}
		var payload struct {
			Channel string           `json:"channel"`
			Thread  string           `json:"thread_ts"`
			Control *controlEnvelope `json:"control"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			return nil
		}
		channel := ""
		if payload.Control != nil {
			c := payload.Control
			if !validControl(*c) || c.Target.Installation != installation {
				return nil
			}
			err = tx.QueryRow(ctx, `SELECT c.slack_channel_id FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN slack_connections pub ON pub.id=c.connection_id JOIN slack_requests r ON r.id=$1
 WHERE t.id=$2 AND c.installation_id=r.installation_id AND pub.revoked_at IS NULL AND r.source_occurred_at>=pub.created_at AND (left(c.slack_channel_id,1)<>'D' OR (t.recipient_user_id=r.slack_user_id AND t.recipient_team_id=c.team_id))`, id, c.Target.Thread).Scan(&channel)
		} else {
			err = tx.QueryRow(ctx, `SELECT $2::text FROM slack_connections pub JOIN slack_requests r ON r.id=$1
 WHERE pub.slack_installation_id=r.installation_id AND pub.revoked_at IS NULL AND r.source_occurred_at>=pub.created_at`, id, payload.Channel).Scan(&channel)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !validConversationID(channel) {
			return nil
		}
		direct := slackroute.IsDirectMessage(channel)
		if direct {
			var granted bool
			if err = tx.QueryRow(ctx, `SELECT granted_scopes @> $2::text[] FROM slack_installations WHERE id=$1`, installation, slackroute.DirectMessageScopes()).Scan(&granted); err != nil {
				return err
			}
			if !granted {
				return nil
			}
		}
		text := "Helmr could not accept that action. Check your access in Console, then try again."
		var blocks []any
		switch code {
		case "sleep", "close", "help":
			var target controlTarget
			var source time.Time
			var root string
			err := tx.QueryRow(ctx, `SELECT r.installation_id,r.thread_id,r.thread_source_id,r.environment_id,r.session_id,r.source_occurred_at,t.thread_ts FROM slack_requests r JOIN slack_threads t ON t.id=r.thread_id WHERE r.id=$1 AND t.deleted_at IS NULL`, id).Scan(&target.Installation, &target.Thread, &target.Participant, &target.Environment, &target.Session, &source, &root)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			_, denied, err := linkedHuman(ctx, tx, team, actor, target.Environment, source, false)
			if err != nil {
				return err
			}
			if denied != "" {
				return nil
			}
			text, blocks, err = lifecyclePresentation(ctx, tx, config, target, code, actor, true)
			if err != nil {
				return err
			}
			payload.Thread = root
		case "wake":
			text = "Wake accepted. Check the Computer state for completion. No prompt was sent and Stop holds remain unchanged."
		case "hibernate":
			text = "Hibernate accepted. Background progress pauses when capture succeeds. Check the Computer state for completion."
		case "confirm_close":
			text = "Close accepted. The Session and its owned Sessions will close after accepted work settles. Wake cannot undo closure. Other work may still occupy the Computer."
		case "computer_busy":
			text = "Cannot hibernate while foreground work or another control is active. Wait for it to settle or use the existing Stop control, then try again."
		case "execution_settling":
			text = "The previous execution timed out and is still settling. Try again shortly."
		case "execution_capacity_unavailable":
			text = "Worker capacity could not be confirmed. No new work was accepted. Try again after Worker capacity becomes available."
		case "execution_capacity_incompatible":
			text = "No active Worker pool supports this Computer. Ask an administrator to configure a compatible Worker pool, then try again."
		case "execution_capacity_exhausted":
			text = "Worker capacity is full. Release an unused Computer or ask an administrator to raise the Worker maximum, then try again."
		case "execution_limit":
			text = "Cannot start execution: the Environment execution quota is in use. Release an unused Computer or ask an administrator to raise the quota, then try again."
		case "response_conflict":
			text = "The affected scope changed. Use !sleep or !close again to review the current members."
		case "mute":
			text = "Thread input muted. Work already accepted and Agent replies continue. Use !unmute to resume automatic input, or mention the Agent to send just one message."
		case "unmute":
			text = "Thread input enabled. Future replies will reach the Agent without a mention. Muted messages will not be replayed."
		case "command_unbound":
			text = "Use thread commands in an existing Agent Session thread."

		case "identity_unlinked":
			link, err := firstUseURL(config, id, channel)
			if err != nil {
				return err
			}
			text = "Link your Slack account to Helmr to use this Agent. Your action has not been applied."
			if direct {
				text += " After linking, send a new message outside this thread. " + directMessageDescription
			}
			blocks = []any{
				map[string]any{"type": "section", "text": map[string]string{"type": "plain_text", "text": text}},
				map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "action_id": "link_account", "text": map[string]string{"type": "plain_text", "text": "Link your account"}, "url": link}}},
			}
		case "ask_already_answered":
			text = "This question has already been answered. Check the updated question for its recorded answer."
		case "ask_cancelled":
			text = "This question was withdrawn and no longer accepts answers."
		case "answer_invalid":
			text = "That answer does not match the question. Use the current question controls or inspect it in Console."
		case "gesture_expired", "stale_gesture":
			text = "This action is out of date. Send a new message or use the latest question controls to try again."
			if direct {
				text = "This action is out of date. Send a new message outside this thread or use the latest question controls to try again."
			}
		case "session_unavailable":
			text = "This Session is closed or unavailable. Mention the Agent in a new thread to start a new Session."
			if direct {
				text = "This Session is closed or unavailable. Send a new message outside this thread to start a new Session."
			}
		case "agent_unavailable":
			text = "That Agent is unavailable. Check its connection in Console, then try again."
		case "thread_generation_unavailable":
			text = "This thread belongs to an earlier connection. Start a new thread in a connected channel or use the Helmr CLI."
			if direct {
				text = "This thread is no longer available. Send a new message outside this thread."
			}
		case "binding_unavailable":
			text = "That Slack destination is no longer available. Start a new thread in a connected channel or use the Helmr CLI."
		case "content_invalid", "content_unsupported":
			text = "Helmr could not accept that input. Check the task's supported input in Console, then try again."
		}
		body := map[string]any{"channel": channel, "text": text}
		if lifecycleAction(code) && timestampPattern.MatchString(payload.Thread) {
			body["thread_ts"] = payload.Thread
		}
		if direct {
			body["mrkdwn"] = false
			body["parse"] = "none"
			body["unfurl_links"] = false
			body["unfurl_media"] = false
		}
		if len(blocks) > 0 {
			body["blocks"] = blocks
		}
		if !direct {
			body["user"] = actor
		}
		frozen, err := json.Marshal(body)
		if err != nil {
			return err
		}
		intent = &rejectionFeedback{installation: installation, credentialRevision: revision, payload: frozen, channel: channel, actor: actor, team: team}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return intent, found, nil
}

// ReconcileRejectedFeedback consumes terminal receipts and input-control
// confirmations, including receipts rejected
// at ingress. The only remote operation is private to the authenticated actor;
// Channel feedback is ephemeral; DM feedback is an ordinary private message.
// Both use the same at-most-once attempt policy, not a new delivery queue.
func ReconcileRejectedFeedback(ctx context.Context, pool db.TxBeginner, client *WebClient, config ProjectionConfig, limit int) (int, error) {
	if client == nil || !config.valid() || limit < 1 || limit > 64 {
		return 0, errGestureInvalid
	}
	for n := 0; n < limit; n++ {
		intent, found, err := takeRejectedFeedback(ctx, pool, config)
		if err != nil || !found {
			return n, err
		}
		if intent != nil {
			method := "chat.postEphemeral"
			if slackroute.IsDirectMessage(intent.channel) {
				if _, err := client.verifyConversation(ctx, intent.installation, intent.credentialRevision, intent.team, intent.channel, intent.actor); err != nil {
					continue
				}
				method = "chat.postMessage"
			}
			client.Call(ctx, intent.installation, intent.credentialRevision, method, intent.payload)
		}
	}
	return limit, nil
}
