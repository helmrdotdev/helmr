package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5"
)

type streamSnapshot struct {
	state, confirmed             string
	recipientTeam, recipientUser *string
	closed, suppressed           bool
	confirmedRevision            int64
}

type streamMutation struct {
	method   string
	payload  []byte
	text     string
	revision int64
}

func prepareStreamMutation(ctx context.Context, tx pgx.Tx, post, thread, environment uuid.UUID, channel string, root, message *string, body []byte, revision int64, s streamSnapshot) (streamMutation, error) {
	var result streamMutation
	if root == nil || s.recipientTeam == nil || s.recipientUser == nil {
		return result, errors.New("stream requires a thread and verified recipient")
	}
	if s.state == "uncertain" || s.state == "stopped" {
		return result, nil
	}
	desired, err := streamContent(body)
	if err != nil {
		return result, err
	}
	var source map[string]json.RawMessage
	if err = json.Unmarshal(body, &source); err != nil {
		return result, err
	}
	payload := map[string]any{"channel": channel}
	result.text = desired
	result.revision = revision
	if message == nil {
		if s.suppressed {
			return result, nil
		}
		result.method = "chat.startStream"
		payload["thread_ts"] = *root
		payload["recipient_team_id"] = *s.recipientTeam
		payload["recipient_user_id"] = *s.recipientUser
		for _, field := range []string{"username", "icon_url", "icon_emoji"} {
			if source[field] != nil {
				payload[field] = source[field]
			}
		}
		// This app-owned marker is a correlation hint, never authority. Positive
		// reconciliation also requires exact authored bytes, app/bot and destination.
		header, err := streamContext(post, body)
		if err != nil {
			return result, err
		}
		payload["chunks"] = []any{map[string]any{"type": "markdown_text", "text": desired}, map[string]any{"type": "blocks", "blocks": header}}
	} else if s.closed && (s.suppressed || desired == s.confirmed) {
		result.method = "chat.stopStream"
		result.text = s.confirmed
		result.revision = s.confirmedRevision
		status, err := aggregateStatus(ctx, tx, thread, environment)
		if err != nil {
			return result, err
		}
		payload["ts"] = *message
		payload["session_status"] = status
	} else if desired != s.confirmed && !s.suppressed {
		if !strings.HasPrefix(desired, s.confirmed) {
			return result, errors.New("stream content is not an append-only prefix")
		}
		result.method = "chat.appendStream"
		payload["ts"] = *message
		payload["chunks"] = []any{map[string]any{"type": "markdown_text", "text": strings.TrimPrefix(desired, s.confirmed)}}
	} else {
		return streamMutation{}, nil
	}
	result.payload, err = json.Marshal(payload)
	return result, err
}

func statusAffectingStream(method string) bool {
	return method == "chat.startStream" || method == "chat.stopStream"
}

// A status-changing stream call owes an absolute status repair until one is
// acknowledged. This durable flag survives a crashed stream owner while leases
// can expire normally. Proven-unsent calls cancel their own repair obligation.
// Independent questions/posts remain outside this status lane.
func finishStreamLane(ctx context.Context, tx pgx.Tx, claim PostClaim, result DeliveryResult) error {
	if !claim.OwnsStatus {
		return nil
	}
	repair := result.Disposition != NotIssued && result.Disposition != RateLimited && result.Disposition != Rejected
	_, err := tx.Exec(ctx, `UPDATE slack_threads SET status_confirmation='unknown',stream_status_repair=$3,
 inflight_method=NULL,inflight_attempt_id=NULL,claimed_until=NULL,
 next_attempt_at=clock_timestamp(),refresh_at=clock_timestamp(),delivery_error=CASE WHEN $3 THEN 'stream_status_unconfirmed' ELSE NULL END
 WHERE id=$1 AND inflight_attempt_id=$2`, claim.ThreadID, claim.AttemptID, repair)
	return err
}

func claimStreamLane(ctx context.Context, tx pgx.Tx, claim *PostClaim, environment uuid.UUID) error {
	status, err := aggregateStatus(ctx, tx, claim.ThreadID, environment)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE slack_threads SET desired_status=$2,
 status_confirmation='unknown',stream_status_repair=true,inflight_method=$3,inflight_attempt_id=$4,
 claimed_until=$5 WHERE id=$1`, claim.ThreadID, status, claim.Method, claim.AttemptID, claim.ExpiresAt)
	if err != nil {
		return err
	}
	claim.OwnsStatus = true
	return dbnotify.Notify(ctx, tx, "slack_delivery")
}

func streamContext(post uuid.UUID, body []byte) ([]any, error) {
	var source struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, err
	}
	presentation, err := frozenPresentationBlocks(source.Blocks)
	if err != nil {
		return nil, err
	}
	if len(presentation) == 0 {
		return nil, errors.New("stream response has no presentation context")
	}
	marker, ok := presentation[0].(map[string]json.RawMessage)
	if !ok {
		return nil, errors.New("invalid stream presentation context")
	}
	marker["block_id"], _ = json.Marshal("helmr_stream_" + post.String())
	return presentation, nil
}
