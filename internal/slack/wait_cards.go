package slack

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// waitTitle names what a deferred message waits for.
func waitTitle(code string) string {
	switch code {
	case "execution_limit":
		return "Waiting for the execution limit"
	case "execution_settling":
		return "Waiting for the previous run to stop"
	default:
		return "Waiting for Worker capacity"
	}
}

func waitCard(title, state string) map[string]any {
	card := map[string]any{"type": "task_card", "task_id": "wait", "block_id": "wait:" + state + ":" + title, "title": title, "status": state}
	return map[string]any{"text": title, "blocks": []any{card}, "mrkdwn": false, "parse": "none", "unfurl_links": false, "unfurl_media": false}
}

type waitCardIntent struct {
	request, installation uuid.UUID
	revision              int64
	calls                 []waitCall
	posting               bool
}

type waitCall struct {
	method string
	body   map[string]any
}

// takeWaitCard claims one deferred message whose wait card should appear or
// settle. The card follows the message: shown while it waits for execution,
// then complete when admitted or an error when it expires or is stopped. Like
// rejection feedback it is best effort: the state advances before HTTP, so a
// crash loses at most that card update.
func takeWaitCard(ctx context.Context, pool db.TxBeginner) (*waitCardIntent, bool, error) {
	var intent *waitCardIntent
	found := false
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var id, installation uuid.UUID
		var raw []byte
		var status, code string
		var card, channel, root, ts *string
		var revision int64
		var active, bound bool
		err := tx.QueryRow(ctx, `SELECT r.id,r.installation_id,r.payload,r.status,COALESCE(r.error,''),r.wait_card,r.wait_channel,r.wait_thread_ts,r.wait_ts,i.credential_revision,
 i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND r.source_occurred_at>=i.authorized_at AND r.source_occurred_at>=i.connected_at,
 EXISTS(SELECT 1 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id WHERE c.installation_id=r.installation_id AND t.slack_channel_id=r.wait_channel AND t.thread_ts=r.wait_thread_ts AND t.deleted_at IS NULL)
 FROM slack_requests r JOIN slack_installations i ON i.id=r.installation_id
 WHERE (r.status='received' AND r.wait_card IS NULL AND r.error IN ('execution_capacity_exhausted','execution_limit','execution_settling') AND r.payload IS NOT NULL)
 OR (r.status<>'received' AND (r.wait_card='waiting' OR (r.wait_card='posting' AND r.finished_at<clock_timestamp()-interval '1 minute')))
 ORDER BY r.received_at,r.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&id, &installation, &raw, &status, &code, &card, &channel, &root, &ts, &revision, &active, &bound)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if card == nil {
			var message messageGesture
			if json.Unmarshal(raw, &message) != nil || !validConversationID(message.Channel) {
				_, err = tx.Exec(ctx, `UPDATE slack_requests SET wait_card='settled' WHERE id=$1`, id)
				return err
			}
			thread := message.Thread
			if thread == "" {
				thread = message.Timestamp
			}
			if _, err = tx.Exec(ctx, `UPDATE slack_requests SET wait_card='posting',wait_channel=$2,wait_thread_ts=$3 WHERE id=$1`, id, message.Channel, thread); err != nil {
				return err
			}
			if !active || !timestampPattern.MatchString(thread) {
				return nil
			}
			post := waitCard(waitTitle(code), "in_progress")
			post["channel"], post["thread_ts"] = message.Channel, thread
			intent = &waitCardIntent{request: id, installation: installation, revision: revision, posting: true, calls: []waitCall{
				{"agents.sessions.setStatus", map[string]any{"channel_id": message.Channel, "thread_ts": thread, "status": "processing"}},
				{"chat.postMessage", post},
			}}
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_requests SET wait_card='settled' WHERE id=$1`, id); err != nil {
			return err
		}
		if !active {
			return nil
		}
		intent = &waitCardIntent{request: id, installation: installation, revision: revision}
		if ts != nil {
			title, state := "Ready to start", "complete"
			if status == "rejected" {
				title, state = waitTitle(code), "error"
				if code == "stopped_waiting" {
					title = "Stopped"
				}
			}
			update := waitCard(title, state)
			update["channel"], update["ts"] = *channel, *ts
			intent.calls = append(intent.calls, waitCall{"chat.update", update})
		}
		// An admitted message continues in its Session thread, whose status lane
		// takes over. A refused one leaves only this Slack session to clear.
		if status == "rejected" && !bound {
			intent.calls = append(intent.calls, waitCall{"agents.sessions.setStatus", map[string]any{"channel_id": *channel, "thread_ts": *root, "status": "active"}})
		}
		return nil
	})
	return intent, found, err
}

// ReconcileWaitCards shows and settles wait cards for deferred messages.
func ReconcileWaitCards(ctx context.Context, pool db.TxBeginner, client Caller, limit int) (int, error) {
	if client == nil || limit < 1 || limit > 64 {
		return 0, errGestureInvalid
	}
	for n := range limit {
		intent, found, err := takeWaitCard(ctx, pool)
		if err != nil || !found {
			return n, err
		}
		if intent == nil {
			continue
		}
		var posted *string
		for _, call := range intent.calls {
			payload, err := json.Marshal(call.body)
			if err != nil {
				return n, err
			}
			result := client.Call(ctx, intent.installation, intent.revision, call.method, payload)
			if call.method == "chat.postMessage" && result.Disposition == Acknowledged {
				posted = &result.Timestamp
			}
		}
		if intent.posting {
			if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE slack_requests SET wait_card='waiting',wait_ts=$2 WHERE id=$1 AND wait_card='posting'`, intent.request, posted)
				return err
			}); err != nil {
				return n, err
			}
		}
	}
	return limit, nil
}
