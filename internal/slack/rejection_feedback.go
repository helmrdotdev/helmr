package slack

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type rejectionFeedback struct {
	installation       uuid.UUID
	credentialRevision int64
	payload            []byte
}

// takeRejectedFeedback disposes terminal rejected input and its private hint
// before external I/O. Identity and digest remain for retry comparison.
// Ephemeral hints are best effort: a crash may lose one, but neither an unknown
// HTTP outcome nor a duplicate receipt may repeat it. No core owner is invented.
func takeRejectedFeedback(ctx context.Context, pool db.TxBeginner, config ProjectionConfig) (*rejectionFeedback, bool, error) {
	var intent *rejectionFeedback
	found := false
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var id, installation uuid.UUID
		var raw []byte
		var actor, code string
		var active bool
		var revision int64
		err := tx.QueryRow(ctx, `SELECT r.id,r.installation_id,r.payload,r.slack_user_id,COALESCE(r.error,''),i.credential_revision,
 i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND r.source_occurred_at>=i.authorized_at AND r.source_occurred_at>=i.connected_at
 FROM slack_requests r JOIN slack_installations i ON i.id=r.installation_id
 WHERE r.status='rejected' AND r.payload_expired_at IS NULL ORDER BY r.finished_at,r.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&id, &installation, &raw, &actor, &code, &revision, &active)
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
		if !active || len(raw) == 0 {
			return nil
		}
		var payload struct {
			Channel string           `json:"channel"`
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
			err = tx.QueryRow(ctx, `SELECT c.slack_channel_id FROM slack_threads t JOIN slack_channels c ON c.id=t.channel_id JOIN agent_publications pub ON pub.id=c.publication_id JOIN slack_requests r ON r.id=$1
 WHERE t.id=$2 AND c.installation_id=r.installation_id AND pub.revoked_at IS NULL AND r.source_occurred_at>=pub.created_at`, id, c.Target.Thread).Scan(&channel)
		} else {
			err = tx.QueryRow(ctx, `SELECT $2::text FROM agent_publications pub JOIN slack_requests r ON r.id=$1
 WHERE pub.slack_installation_id=r.installation_id AND pub.revoked_at IS NULL AND r.source_occurred_at>=pub.created_at`, id, payload.Channel).Scan(&channel)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !agent.ValidSlackChannelID(channel) {
			return nil
		}
		text := "Helmr could not accept that action. Check your access in Console, then try again."
		var blocks []any
		switch code {
		case "identity_unlinked":
			link, err := firstUseURL(config, id, channel)
			if err != nil {
				return err
			}
			text = "Link your Slack account to Helmr to use this Agent. Your action has not been applied."
			blocks = []any{
				map[string]any{"type": "section", "text": map[string]string{"type": "plain_text", "text": text}},
				map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "action_id": "link_account", "text": map[string]string{"type": "plain_text", "text": "Link your account"}, "url": link}}},
			}
		case "gesture_expired", "stale_gesture":
			text = "This action is out of date. Send a new message or use the latest question controls to try again."
		case "agent_unavailable":
			text = "That Agent is unavailable. Check its connection in Console, then try again."
		case "thread_generation_unavailable":
			text = "This thread belongs to an earlier connection. Start a new thread in a connected channel or use the Helmr CLI."
		case "binding_unavailable":
			text = "That Slack destination is no longer available. Start a new thread in a connected channel or use the Helmr CLI."
		case "content_invalid", "content_unsupported":
			text = "Helmr could not accept that input. Check the task's supported input in Console, then try again."
		}
		frozen, err := json.Marshal(struct {
			Channel string `json:"channel"`
			User    string `json:"user"`
			Text    string `json:"text"`
			Blocks  []any  `json:"blocks,omitempty"`
		}{channel, actor, text, blocks})
		if err != nil {
			return err
		}
		intent = &rejectionFeedback{installation: installation, credentialRevision: revision, payload: frozen}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return intent, found, nil
}

// ReconcileRejectedFeedback consumes terminal receipts including those rejected
// at ingress. The only remote operation is private to the authenticated actor;
// Slack does not guarantee ephemeral delivery or retain these notifications.
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
			client.Call(ctx, intent.installation, intent.credentialRevision, "chat.postEphemeral", intent.payload)
		}
	}
	return limit, nil
}
