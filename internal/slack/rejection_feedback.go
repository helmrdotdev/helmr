package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

type rejectionFeedback struct {
	installation                 uuid.UUID
	credentialRevision           int64
	payload                      []byte
	method, channel, actor, team string
}

// sessionWideRefusal reports refusals caused by the Session or its execution
// rather than by the person. They disclose nothing about the person's access,
// so a channel thread keeps them for everyone who later reads it. An ended or
// superseded thread refuses every later reply, so only a message addressed to
// the Agent gets a visible answer there.
func sessionWideRefusal(code string, mentioned bool) bool {
	switch code {
	case "execution_settling", "execution_capacity_incompatible", "execution_capacity_exhausted", "execution_limit":
		return true
	case "session_unavailable", "thread_superseded":
		return mentioned
	}
	return false
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
		var actor, code, team, bot string
		var active bool
		var revision int64
		var waitThread *string
		err := tx.QueryRow(ctx, `SELECT r.id,r.installation_id,r.payload,r.slack_user_id,COALESCE(r.error,r.operation,''),i.credential_revision,i.team_id,i.bot_user_id,CASE WHEN r.wait_ts IS NOT NULL THEN r.wait_thread_ts END,
 i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND r.source_occurred_at>=i.authorized_at AND r.source_occurred_at>=i.connected_at
 FROM slack_requests r JOIN slack_installations i ON i.id=r.installation_id
 WHERE (r.status='rejected' OR (r.status='accepted' AND r.operation IN ('mute','unmute','sleep','wake','close','help','hibernate','confirm_close'))) AND r.payload_expired_at IS NULL ORDER BY r.finished_at,r.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&id, &installation, &raw, &actor, &code, &revision, &team, &bot, &waitThread, &active)
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
			Channel    string           `json:"channel"`
			Text       string           `json:"text"`
			NativeStop bool             `json:"native_stop"`
			Message    string           `json:"message_ts"`
			Thread     string           `json:"thread_ts"`
			Control    *controlEnvelope `json:"control"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			return nil
		}
		// A posted wait card opened a thread under a new message.
		if payload.Thread == "" && waitThread != nil {
			payload.Thread = *waitThread
		}
		channel := ""
		if payload.Control != nil {
			c := payload.Control
			if !validControl(*c) || c.Target.Installation != installation {
				return nil
			}
			var root *string
			err = tx.QueryRow(ctx, `SELECT c.slack_channel_id,t.thread_ts FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN slack_connections pub ON pub.id=c.connection_id JOIN slack_requests r ON r.id=$1
 WHERE t.id=$2 AND c.installation_id=r.installation_id AND pub.revoked_at IS NULL AND r.source_occurred_at>=pub.created_at AND (left(c.slack_channel_id,1)<>'D' OR (t.recipient_user_id=r.slack_user_id AND t.recipient_team_id=c.team_id))`, id, c.Target.Thread).Scan(&channel, &root)
			if root != nil {
				payload.Thread = *root
			}
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
		if !direct && payload.Control == nil && !payload.NativeStop && (code == "identity_unlinked" || code == "permission_denied") && stripRoutingMention(payload.Text, bot) == payload.Text && threadInputCommand(payload.Text, bot) == "" {
			return nil
		}

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
		// A message, unlike a control or lifecycle command, waits for execution
		// through its admission window before it is rejected.
		waited := payload.Control == nil && !payload.NativeStop && threadInputCommand(payload.Text, bot) == ""
		var blocks []any
		switch code {
		case "permission_denied":
			text = "You do not have access to this Agent or action. Ask an administrator to check your Agent access in Helmr Console."
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
			human, denied, err := linkedSessionHuman(ctx, tx, team, actor, target.Installation, target.Environment, target.Session, source)
			if err != nil {
				return err
			}
			if denied != "" {
				return nil
			}
			if err := session.AuthorizeSessionOperation(ctx, tx, session.UserCaller(human), target.Environment, target.Session, auth.PermissionSessionsRead); err != nil {
				if errors.Is(err, session.ErrDenied) {
					return nil
				}
				return err
			}
			text, blocks, err = lifecyclePresentation(ctx, tx, config, target, code, actor)
			if errors.Is(err, session.ErrDenied) || errors.Is(err, access.ErrDenied) {
				return nil
			}
			if err != nil {
				return err
			}
			payload.Thread = root
		case "wake":
			text = "Waking the Computer. Nothing was sent to the Agent."
		case "hibernate":
			text = "Hibernating the Computer. Background work pauses once its state is saved."
		case "confirm_close":
			text = "Closing this Session. Running work finishes; queued work and open questions are cancelled."
		case "computer_busy":
			text = "The Computer cannot hibernate while work is running. Wait for it to finish or stop it, then try again."
		case "execution_settling":
			text = "The previous run is still stopping. Try again in a moment."
			if waited {
				text = "Your message was not sent to the Agent: the previous run was still stopping after an hour. Send it again, or ask an administrator to check service health."
			}
		case "execution_capacity_incompatible":
			text = "No Worker pool can run this Computer. Ask an administrator to set up a compatible Worker pool, then try again."
		case "execution_capacity_exhausted":
			text = "Worker capacity is full. Try again later, or ask an administrator to raise the Worker maximum."
			if waited {
				text = "Your message was not sent to the Agent: Worker capacity stayed full for an hour. Send it again later, or ask an administrator to raise the Worker maximum."
			}
		case "execution_limit":
			text = "This Environment is at its execution limit. Try again later, or ask an administrator to raise the limit."
			if waited {
				text = "Your message was not sent to the Agent: this Environment stayed at its execution limit for an hour. Send it again later, or ask an administrator to raise the limit."
			}
		case "response_conflict":
			text = "This changed while you were confirming it. Use !sleep or !close again."
		case "mute":
			text = "Thread input muted. Replies here no longer reach the Agent; mention it to send one message, or use !unmute."
		case "unmute":
			text = "Thread input on. Replies here reach the Agent again; muted messages are not replayed."
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
		case "gesture_expired":
			text = "This was not applied in time. Use the latest controls or send a new message. If this keeps happening, ask an administrator to check service health."
			if direct {
				text = "This was not applied in time. Use the latest controls or send a new message outside this thread. If this keeps happening, ask an administrator to check service health."
			}
		case "stale_gesture":
			text = "This action is out of date. Send a new message or use the latest question controls to try again."
			if direct {
				text = "This action is out of date. Send a new message outside this thread or use the latest question controls to try again."
			}
		case "stopped_waiting":
			text = "Stopped. Your message was not sent to the Agent."
		case "session_closing":
			text = "This Session is closing, so there is nothing to stop. Running work finishes; queued work and open questions are cancelled."
		case "session_unavailable":
			text = "This Session has ended. Mention the Agent in a new thread to start another."
			if direct {
				text = "This Session has ended. Send a new message outside this thread to start another."
			}
		case "agent_unavailable":
			text = "That Agent is unavailable. Check its connection in Console, then try again."
		case "thread_generation_unavailable":
			text = "This thread belongs to an earlier connection. Start a new thread in a connected channel or use the Helmr CLI."
			if direct {
				text = "This thread is no longer available. Send a new message outside this thread."
			}
		case "thread_superseded":
			text = "This thread no longer continues its Session. Follow the Session in Console, or mention the Agent in a new thread to start a new Session."
			if direct {
				text = "This thread no longer continues its Session. Follow the Session in Console, or send a new message outside this thread to start a new Session."
			}
		case "binding_unavailable":
			text = "That Slack destination is no longer available. Start a new thread in a connected channel or use the Helmr CLI."
		case "content_invalid", "content_unsupported":
			text = "Helmr could not accept that input. Check the task's supported input in Console, then try again."
		}
		// Feedback answers in the conversation where the input was made. A
		// private reply can join only an existing thread; a visible one may
		// thread under the refused message itself.
		body := map[string]any{"channel": channel, "text": text}
		method := "chat.postEphemeral"
		thread := payload.Thread
		if direct || sessionWideRefusal(code, payload.Control == nil && stripRoutingMention(payload.Text, bot) != payload.Text) {
			method = "chat.postMessage"
			if thread == "" {
				thread = payload.Message
			}
			body["mrkdwn"] = false
			body["parse"] = "none"
			body["unfurl_links"] = false
			body["unfurl_media"] = false
		} else {
			body["user"] = actor
		}
		if timestampPattern.MatchString(thread) {
			body["thread_ts"] = thread
		}
		if len(blocks) > 0 {
			body["blocks"] = blocks
		}
		frozen, err := json.Marshal(body)
		if err != nil {
			return err
		}
		intent = &rejectionFeedback{installation: installation, credentialRevision: revision, payload: frozen, method: method, channel: channel, actor: actor, team: team}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return intent, found, nil
}

// ReconcileRejectedFeedback consumes terminal receipts and input-control
// confirmations, including receipts rejected at ingress. Feedback about the
// person is ephemeral in channels; Session-wide refusals are visible thread
// replies; DM feedback is an ordinary private message. All use the same
// at-most-once attempt policy, not a new delivery queue.
func ReconcileRejectedFeedback(ctx context.Context, pool db.TxBeginner, client *WebClient, config ProjectionConfig, limit int) (int, error) {
	if client == nil || !config.valid() || limit < 1 || limit > 64 {
		return 0, errGestureInvalid
	}
	for n := range limit {
		intent, found, err := takeRejectedFeedback(ctx, pool, config)
		if err != nil || !found {
			return n, err
		}
		if intent != nil {
			if slackroute.IsDirectMessage(intent.channel) {
				if _, err := client.verifyConversation(ctx, intent.installation, intent.credentialRevision, intent.team, intent.channel, intent.actor); err != nil {
					continue
				}
			}
			client.Call(ctx, intent.installation, intent.credentialRevision, intent.method, intent.payload)
		}
	}
	return limit, nil
}
