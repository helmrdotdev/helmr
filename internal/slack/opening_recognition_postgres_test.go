package slack

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestOpeningRecognitionUsesActualSenderAndKeepsAbandonment(t *testing.T) {
	for _, kind := range []string{"normalized-body", "wrong-app", "wrong-bot", "wrong-channel", "wrong-workspace", "abandoned"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
			post := f.opening(t)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
				t.Fatal(err)
			}
			var org uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT organization_id FROM slack_installations WHERE id=$1`, f.installation).Scan(&org); err != nil {
				t.Fatal(err)
			}
			if kind == "abandoned" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='failed',delivery_disposed_at=clock_timestamp(),delivery_disposed_by=$2,closed_at=clock_timestamp(),error='delivery_abandoned' WHERE id=$1`, post, f.User)
			}
			metadata, _ := json.Marshal(map[string]any{"event_type": "helmr_post", "event_payload": map[string]any{"post_id": post.String(), "revision": 1}})
			message := observedMessage{Channel: "C1", Timestamp: "123.999", AppID: "app", User: "bot", Text: "Slack normalized this text", Metadata: metadata}
			team := "team"
			switch kind {
			case "wrong-app":
				message.AppID = "different-app"
			case "wrong-bot":
				message.User = "human"
			case "wrong-channel":
				message.Channel = "COTHER"
			case "wrong-workspace":
				team = "other-team"
			}
			matched, err := confirmOpeningMessage(t.Context(), f.Pool, org, team, message)
			want := kind == "normalized-body" || kind == "abandoned"
			if err != nil || matched != want {
				t.Fatal(matched, err)
			}
			var bound bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT thread_ts IS NOT NULL FROM slack_threads WHERE id=$1`, f.thread).Scan(&bound); err != nil || bound != want {
				t.Fatal(bound, err)
			}
			if kind == "abandoned" {
				var kept bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND delivery_disposed_at IS NOT NULL AND message_ts='123.999' FROM slack_posts WHERE id=$1`, post).Scan(&kept); err != nil || !kept {
					t.Fatal(kept, err)
				}
			}
			if kind == "abandoned" {
				later := f.post(t, 2, "later", "lifecycle", nil)
				f.due(t)
				f.noPostClaim(t, later)
				var suppressed bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT status='suppressed' FROM slack_posts WHERE id=$1`, later).Scan(&suppressed); err != nil || !suppressed {
					t.Fatal("late binding resumed delivery", suppressed, err)
				}
			}
			if want {
				again, err := confirmOpeningMessage(t.Context(), f.Pool, org, team, message)
				if err != nil || !again {
					t.Fatal("recognition was not repeatable", again, err)
				}
			}
		})
	}
}

func TestOpeningAbandonmentInvalidatesClaimBeforeHTTP(t *testing.T) {
	for _, kind := range []string{"post", "status"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			opening := f.opening(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='posted',posted_at=clock_timestamp(),message_ts='123.456',confirmed_revision=desired_revision WHERE id=$1`, opening)
			var authorized func() (bool, error)
			if kind == "post" {
				claim := f.postClaim(t, f.post(t, 2, "later", "lifecycle", nil))
				authorized = func() (bool, error) { return postClaimAuthorized(t.Context(), f.Pool, claim) }
			} else {
				claim := f.claim(t, "processing")
				authorized = func() (bool, error) { return statusClaimAuthorized(t.Context(), f.Pool, claim) }
			}
			if allowed, err := authorized(); err != nil || !allowed {
				t.Fatal(allowed, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='failed',delivery_disposed_at=clock_timestamp(),delivery_disposed_by=$2,closed_at=clock_timestamp(),error='delivery_abandoned' WHERE id=$1`, opening, f.User)
			if allowed, err := authorized(); err != nil || allowed {
				t.Fatal("abandoned opening retained HTTP authority", allowed, err)
			}
		})
	}
}
