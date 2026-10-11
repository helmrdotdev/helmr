package slack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func streamBody(t *testing.T, text string) []byte {
	t.Helper()
	raw, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	posts, err := RenderContent([]ContentSource{{Sequence: 1, Content: raw}}, false)
	if err != nil || len(posts) != 1 {
		t.Fatal(err)
	}
	body, err := json.Marshal(posts[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func (f statusFixture) streamPost(t *testing.T, seq int64, text string) uuid.UUID {
	t.Helper()
	post := f.post(t, seq, "stream-"+uuid.NewV7().String(), "lifecycle", nil)
	body := streamBody(t, text)
	digest := sha256.Sum256(body)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET payload=$2,payload_digest=$3,presentation_path='stream',recipient_team_id='team',recipient_user_id='human' WHERE id=$1`, post, body, digest[:])
	return post
}
func (f statusFixture) reviseStream(t *testing.T, post uuid.UUID, text string) {
	t.Helper()
	body := streamBody(t, text)
	digest := sha256.Sum256(body)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET payload=$2,payload_digest=$3,desired_revision=desired_revision+1,status=CASE WHEN status='posted' THEN 'pending' ELSE status END WHERE id=$1`, post, body, digest[:])
}
func TestSlackStreamStartAppendStopRepairsAggregateAndKeepsQuestionsIndependent(t *testing.T) {
	f := newStatusFixture(t)
	f.pendingQuestion(t)
	post := f.streamPost(t, 1, "first")
	peer := f.streamPost(t, 2, "peer")
	first := f.postClaim(t, post)
	if first.Method != "chat.startStream" || first.StatusEpoch == 0 {
		t.Fatal("stream did not own status", first)
	}
	f.due(t)
	f.noPostClaim(t, peer)
	question := f.post(t, 3, "question-status", "lifecycle", nil)
	independent := f.postClaim(t, question)
	if err := FinishPost(t.Context(), f.Pool, independent, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.780"}); err != nil {
		t.Fatal(err)
	}
	if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, peer)
	repair := f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, peer)
	repair = f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.reviseStream(t, post, "first second")
	f.due(t)
	appendClaim := f.postClaim(t, post)
	if appendClaim.Method != "chat.appendStream" || appendClaim.StatusEpoch != 0 || bytes.Contains(appendClaim.Payload, []byte("first")) || !bytes.Contains(appendClaim.Payload, []byte(" second")) {
		t.Fatal("append resent prefix", string(appendClaim.Payload))
	}
	if err := FinishPost(t.Context(), f.Pool, appendClaim, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET closed_at=clock_timestamp() WHERE id=$1`, post)
	f.due(t)
	stop := f.postClaim(t, post)
	if stop.Method != "chat.stopStream" || stop.StatusEpoch == 0 || !bytes.Contains(stop.Payload, []byte(`"session_status":"suspended"`)) || bytes.Contains(stop.Payload, []byte("chunks")) {
		t.Fatal("incorrect stop", string(stop.Payload))
	}
	if err := FinishPost(t.Context(), f.Pool, stop, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var stopped bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT stream_state='stopped' AND status='posted' AND confirmed_revision=2 AND confirmed_stream_text='first second' FROM slack_posts WHERE id=$1`, post).Scan(&stopped); err != nil || !stopped {
		t.Fatal("stream not settled", err)
	}
	f.due(t)
	repair = f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	if next := f.postClaim(t, peer); next.Method != "chat.startStream" {
		t.Fatal(next)
	}
}
func TestSlackStreamUnknownContentIsNotReplayedAndStopHasNoContent(t *testing.T) {
	f := newStatusFixture(t)
	unknown := f.streamPost(t, 1, "unknown")
	start := f.postClaim(t, unknown)
	if err := FinishPost(t.Context(), f.Pool, start, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, unknown)
	repair := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	known := f.streamPost(t, 2, "known")
	f.due(t)
	c := f.postClaim(t, known)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	repair = f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET closed_at=clock_timestamp() WHERE id=$1`, known)
	f.due(t)
	stop := f.postClaim(t, known)
	if err := FinishPost(t.Context(), f.Pool, stop, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	// The old stop can land later; current status is repaired independently. Its
	// next stop is content-free and recomputes the now-suspended aggregate.
	f.pendingQuestion(t)
	f.due(t)
	repair = f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	retry := f.postClaim(t, known)
	if retry.Method != "chat.stopStream" || !bytes.Contains(retry.Payload, []byte(`"session_status":"suspended"`)) || bytes.Contains(retry.Payload, []byte("known")) {
		t.Fatal("stop replayed content/stale status", string(retry.Payload))
	}
	if err := FinishPost(t.Context(), f.Pool, retry, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.noPostClaim(t, unknown)
}

func observedStream(t *testing.T, f statusFixture, post uuid.UUID, value, nativeState string) observedMessage {
	t.Helper()
	body := streamBody(t, value)
	header, err := streamHeader(post, body)
	if err != nil {
		t.Fatal(err)
	}
	var content struct {
		Blocks []any `json:"blocks"`
	}
	if err = json.Unmarshal(body, &content); err != nil {
		t.Fatal(err)
	}
	blocks, err := json.Marshal(append(header, content.Blocks...))
	if err != nil {
		t.Fatal(err)
	}
	var message observedMessage
	if err = f.Pool.QueryRow(t.Context(), `SELECT i.app_id,i.bot_user_id,c.slack_channel_id,t.thread_ts FROM slack_installations i JOIN slack_channels c ON c.installation_id=i.id JOIN slack_threads t ON t.channel_id=c.id WHERE t.id=$1`, f.thread).Scan(&message.AppID, &message.User, &message.Channel, &message.Thread); err != nil {
		t.Fatal(err)
	}
	message.Timestamp = "123.790"
	message.Blocks = blocks
	message.StreamingState = nativeState
	return message
}
func TestSlackStreamPositiveReconciliationNeverReplaysUnknownContent(t *testing.T) {
	f := newStatusFixture(t)
	post := f.streamPost(t, 1, "first <@human>\nline")
	c := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	good := observedStream(t, f, post, "first <@human>\nline", "in_progress")
	for _, field := range []string{"actor", "channel", "thread", "content", "state"} {
		bad := good
		switch field {
		case "actor":
			bad.User = "someone"
		case "channel":
			bad.Channel = "elsewhere"
		case "thread":
			bad.Thread = "999.999"
		case "content":
			bad.Blocks = bytes.ReplaceAll(good.Blocks, []byte("first"), []byte("other"))
		case "state":
			bad.StreamingState = ""
		}
		if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, bad); err != nil || ok {
			t.Fatalf("accepted %s mismatch: %v %v", field, ok, err)
		}
	}
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, good); err != nil || !ok {
		t.Fatal("exact positive start not confirmed", ok, err)
	}
	f.due(t)
	repair := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.reviseStream(t, post, "first <@human>\nline second")
	f.due(t)
	appendClaim := f.postClaim(t, post)
	if appendClaim.Method != "chat.appendStream" {
		t.Fatal(appendClaim.Method)
	}
	if err := FinishPost(t.Context(), f.Pool, appendClaim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	// A newer desired revision must not overwrite the frozen content being proved.
	f.reviseStream(t, post, "first <@human>\nline second third")
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, good); err != nil || ok {
		t.Fatal("old prefix accepted for unknown append", ok, err)
	}
	exact := observedStream(t, f, post, "first <@human>\nline second", "completed")
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, exact); err != nil || !ok {
		t.Fatal("exact append not confirmed", ok, err)
	}
	var partial bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND stream_state='stopped' AND confirmed_revision=2 AND desired_revision=3 AND error='stream_ended_before_delivery' FROM slack_posts WHERE id=$1`, post).Scan(&partial); err != nil || !partial {
		t.Fatal("partial completion not visible", err)
	}
	f.due(t)
	f.noPostClaim(t, post)
}

func TestSlackStreamRejectionStopsWithoutRepublishingRejectedAppend(t *testing.T) {
	for _, code := range []string{"stopped_by_user", "message_not_in_streaming_state", "invalid_chunks"} {
		t.Run(code, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.streamPost(t, 1, "first")
			start := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, start, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			repair := f.claim(t, "active")
			if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			f.reviseStream(t, post, "first rejected")
			f.due(t)
			appendClaim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, appendClaim, DeliveryResult{Disposition: Rejected, Code: code}); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			if code == "invalid_chunks" {
				stop := f.postClaim(t, post)
				if stop.Method != "chat.stopStream" || bytes.Contains(stop.Payload, []byte("rejected")) {
					t.Fatal("rejected content retried", string(stop.Payload))
				}
				if err := FinishPost(t.Context(), f.Pool, stop, DeliveryResult{Disposition: Acknowledged}); err != nil {
					t.Fatal(err)
				}
			}
			var failed bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND stream_state='stopped' AND error IS NOT NULL AND confirmed_revision=1 AND confirmed_stream_text='first' FROM slack_posts WHERE id=$1`, post).Scan(&failed); err != nil || !failed {
				t.Fatal("rejected delivery lost", err)
			}
			f.due(t)
			f.noPostClaim(t, post)
		})
	}
}

func TestSlackStreamSuppressedRevisionCanReconcileAndStop(t *testing.T) {
	f := newStatusFixture(t)
	post := f.streamPost(t, 1, "published")
	c := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	f.reviseStream(t, post, "published suppressed")
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error { return suppressInstallationPosts(t.Context(), tx, f.installation) }); err != nil {
		t.Fatal(err)
	}
	m := observedStream(t, f, post, "published", "in_progress")
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || !ok {
		t.Fatal("cutoff confirmation", ok, err)
	}
	f.due(t)
	repair := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	stop := f.postClaim(t, post)
	if stop.Method != "chat.stopStream" || bytes.Contains(stop.Payload, []byte("suppressed")) {
		t.Fatal("cutoff republished", string(stop.Payload))
	}
	if err := FinishPost(t.Context(), f.Pool, stop, DeliveryResult{Disposition: Rejected, Code: "message_not_in_streaming_state"}); err != nil {
		t.Fatal(err)
	}
	var settled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='suppressed' AND stream_state='stopped' AND error='publication_authority_unavailable' AND confirmed_stream_text='published' FROM slack_posts WHERE id=$1`, post).Scan(&settled); err != nil || !settled {
		t.Fatal("cutoff stop", err)
	}
}

func TestSlackDeletedStreamReleasesLaterResponseWithoutRetry(t *testing.T) {
	for _, method := range []string{"event", "append", "stop"} {
		t.Run(method, func(t *testing.T) {
			f := newStatusFixture(t)
			turn, _ := f.pendingQuestion(t)
			post := f.streamPost(t, 1, "first")
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET role='intermediate',turn_id=$2 WHERE id=$1`, post, turn)
			start := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, start, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			repair := f.claim(t, "suspended")
			if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			response := f.post(t, 2, "response", "response", &turn)
			f.due(t)
			f.noPostClaim(t, response)
			if method == "event" {
				handler := EventHandler{Database: f.Pool, AppID: "app"}
				if err := handler.observeDeletion(t.Context(), "team", "C1", "123.790"); err != nil {
					t.Fatal(err)
				}
			} else {
				if method == "append" {
					f.reviseStream(t, post, "first disappeared")
				} else {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET closed_at=clock_timestamp() WHERE id=$1`, post)
				}
				f.due(t)
				c := f.postClaim(t, post)
				if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Rejected, Code: "message_not_found"}); err != nil {
					t.Fatal(err)
				}
			}
			var failed bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND stream_state='stopped' AND error IN ('message_deleted','message_not_found') FROM slack_posts WHERE id=$1`, post).Scan(&failed); err != nil || !failed {
				t.Fatal("disappearance not terminal", err)
			}
			f.due(t)
			f.noPostClaim(t, post)
			if c := f.postClaim(t, response); c.Method != "chat.postMessage" {
				t.Fatal(c.Method)
			}
		})
	}
}

func TestSlackDeletedRootConvergesKnownStreamsWithContentFreeStop(t *testing.T) {
	for _, completion := range []string{"already-open", "late-ack", "positive-read"} {
		for _, newer := range []bool{false, true} {
			t.Run(completion+map[bool]string{false: "/same", true: "/newer"}[newer], func(t *testing.T) {
				f := newStatusFixture(t)
				post := f.streamPost(t, 1, "visible")
				start := f.postClaim(t, post)
				if completion == "already-open" {
					if err := FinishPost(t.Context(), f.Pool, start, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
						t.Fatal(err)
					}
				}
				if newer {
					f.reviseStream(t, post, "visible must not publish")
				}
				h := EventHandler{Database: f.Pool, AppID: "app"}
				if err := h.observeDeletion(t.Context(), "team", "C1", "123.456"); err != nil {
					t.Fatal(err)
				}
				switch completion {
				case "late-ack":
					if err := FinishPost(t.Context(), f.Pool, start, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
						t.Fatal(err)
					}
				case "positive-read":
					if err := FinishPost(t.Context(), f.Pool, start, DeliveryResult{Disposition: Uncertain}); err != nil {
						t.Fatal(err)
					}
					m := observedStream(t, f, post, "visible", "in_progress")
					if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || !ok {
						t.Fatal("positive evidence", ok, err)
					}
				}
				f.due(t)
				calls := 0
				client := deliveryTestClient(func(r *http.Request) string {
					calls++
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if r.URL.Path != "/api/chat.stopStream" || bytes.Contains(raw, []byte("chunks")) || bytes.Contains(raw, []byte("visible")) {
						t.Fatal("deleted-root content mutation", r.URL.Path, string(raw))
					}
					return `{"ok":true,"ts":"123.790","channel":"C1"}`
				})
				if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
					t.Fatal(err)
				}
				var settled bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT stream_state='stopped' AND status IN ('posted','suppressed','failed') AND inflight_attempt_id IS NULL AND (status<>'posted' OR desired_revision=confirmed_revision) FROM slack_posts WHERE id=$1`, post).Scan(&settled); err != nil || !settled || calls != 1 {
					t.Fatal("stream retirement outstanding", settled, calls, err)
				}
			})
		}
	}
}

func TestSlackCrashedStreamReconciliationReleasesExactContentCopy(t *testing.T) {
	for _, newer := range []bool{false, true} {
		name := "own_lane"
		if newer {
			name = "newer_lane"
		}
		t.Run(name, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.streamPost(t, 1, "private stream text")
			first := f.postClaim(t, post)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1;
 UPDATE slack_threads SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, post, f.thread)
			f.noPostClaim(t, post)
			var repair StatusClaim
			if newer {
				f.due(t)
				repair = f.claim(t, "active")
			}
			message := observedStream(t, f, post, "private stream text", "completed")
			if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, message); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged, Timestamp: message.Timestamp}); err != nil {
				t.Fatal(err)
			}
			var safe bool
			if newer {
				if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_attempt_id=$2 AND inflight_method='agents.sessions.setStatus' FROM slack_threads WHERE id=$1`, f.thread, repair.AttemptID).Scan(&safe); err != nil || !safe {
					t.Fatal("old reconciliation cleared newer status owner", safe, err)
				}
				return
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_payload IS NULL AND inflight_attempt_id IS NULL AND stream_status_repair AND status_confirmation='unknown' FROM slack_threads WHERE id=$1`, f.thread).Scan(&safe); err != nil || !safe {
				t.Fatal("confirmed crash left content copy or lost repair", safe, err)
			}
			if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "retire"}); err != nil {
				t.Fatal(err)
			}
			// The fixture supplies physical fencing and consumed lifecycle events; the
			// authored stream above was reconciled through the real adapter path.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1;
 UPDATE sessions SET history_retention_mode='duration',history_retention_seconds=1,history_eligible_at=statement_timestamp()-interval '2 seconds',history_expires_at=EXTRACT(epoch FROM statement_timestamp())-1 WHERE id=$1;
 UPDATE slack_thread_sources p SET projected_event_seq=s.next_event_seq-1 FROM sessions s WHERE s.environment_id=p.environment_id AND s.id=p.session_id AND s.id=$1;
 UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.Session, f.publication)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- agent.RunSessionHistoryRetention(ctx, f.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}()
			expired := false
			for ctx.Err() == nil {
				if err := f.Pool.QueryRow(ctx, `SELECT history_expired_at IS NOT NULL FROM sessions WHERE id=$1`, f.Session).Scan(&expired); err != nil {
					cancel()
					<-done
					t.Fatal(err)
				}
				if expired {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if !expired {
				t.Fatal("retention failed to expire released owner")
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT p.payload IS NULL AND p.payload_expired_at IS NOT NULL AND p.confirmed_stream_text='' AND t.inflight_payload IS NULL FROM slack_posts p JOIN slack_thread_sources s ON s.id=p.thread_source_id JOIN slack_threads t ON t.id=s.thread_id WHERE p.id=$1`, post).Scan(&safe); err != nil || !safe {
				t.Fatal("expired stream left a content copy", safe, err)
			}
		})
	}
}
