package session_test

import (
	"crypto/sha256"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackHistoryRetentionWaitsForProjectionAndDelivery(t *testing.T) {
	for _, kind := range []string{"unprojected", "pending", "sending", "uncertain", "open-stream", "uncertain-stream", "newer-revision", "stopped-stream", "posted", "failed", "suppressed", "terminal-post", "terminal-uncertain-stream", "terminal-open-stream", "terminal-stopped-stream"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			_, channel := slackRouteFixture(t, f)
			bindFixtureSlackRoot(t, f, channel)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE projects SET history_retention_seconds=60 WHERE id=(SELECT project_id FROM environments WHERE id=$1) AND EXISTS(SELECT 1 FROM sessions WHERE id=$2)`, f.env, f.session)
			turn := f.enqueue(t, "retention")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			if _, err := session.RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), turn.TurnID, uuid.NewV7(), json.RawMessage(`"retained source"`)); err != nil {
				t.Fatal(err)
			}
			if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
				t.Fatal(err)
			}
			fenceHistoryProcess(t, f)
			if kind != "unprojected" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_thread_sources p SET projected_event_seq=s.next_event_seq-1 FROM sessions s WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND s.id=$1`, f.session)
				body := []byte(`{"text":"retained source"}`)
				digest := sha256.Sum256(body)
				post := uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_posts(id,environment_id,session_id,thread_id,thread_source_id,seq,post_key,role,turn_id,payload,payload_digest,presentation_path)
 SELECT $1,environment_id,session_id,thread_id,id,1,'content','intermediate',$3,$4,$5,'post' FROM slack_thread_sources WHERE session_id=$2`, post, f.session, turn.TurnID, body, digest[:])
				switch kind {
				case "terminal-post", "terminal-uncertain-stream", "terminal-open-stream", "terminal-stopped-stream":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status='failed',error='history_no_match',closed_at=clock_timestamp(),delivery_disposed_at=clock_timestamp(),delivery_disposed_through_seq=0,inflight_method='chat.postMessage',inflight_payload=payload,inflight_digest=payload_digest,inflight_revision=1,inflight_attempt_id=$2 WHERE id=$1`, post, uuid.NewV7())
					if kind != "terminal-post" {
						state := "uncertain"
						if kind == "terminal-open-stream" {
							state = "open"
						}
						if kind == "terminal-stopped-stream" {
							state = "stopped"
						}
						if state == "uncertain" {
							dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET presentation_path='stream',recipient_team_id='team',recipient_user_id='human',stream_status='uncertain',inflight_method='chat.startStream',inflight_stream_text='retained prefix' WHERE id=$1`, post)
						} else {
							dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET presentation_path='stream',recipient_team_id='team',recipient_user_id='human',stream_status=$2,inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL WHERE id=$1`, post, state)
						}
					}
				case "sending", "uncertain":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status=$2,inflight_method='chat.postMessage',inflight_payload=payload,inflight_digest=payload_digest,inflight_revision=1,inflight_attempt_id=$3,claimed_until=CASE WHEN $2='sending' THEN clock_timestamp()+interval '30 seconds' END WHERE id=$1`, post, kind, uuid.NewV7())
				case "posted", "stopped-stream", "newer-revision", "open-stream", "uncertain-stream":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status='posted',message_ts='123.456',posted_at=clock_timestamp(),confirmed_revision=1 WHERE id=$1`, post)
					if kind == "newer-revision" {
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET desired_revision=2 WHERE id=$1`, post)
					}
					if kind == "open-stream" || kind == "uncertain-stream" || kind == "stopped-stream" {
						state := "open"
						if kind == "stopped-stream" {
							state = "stopped"
						}
						if kind == "uncertain-stream" {
							state = "uncertain"
						}
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET presentation_path='stream',recipient_team_id='team',recipient_user_id='human',stream_status=$2,confirmed_stream_text='retained confirmed text' WHERE id=$1`, post, state)
					}
				case "failed", "suppressed":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status=$2,error='delivery_unavailable' WHERE id=$1`, post, kind)
				}
			}
			request := uuid.NewV7()
			body := []byte(`{"message_ts":"123.456","text":"retained input"}`)
			digest := sha256.Sum256(body)
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_requests(id,installation_id,request_key,request_digest,source_occurred_at,slack_user_id,payload,expires_at,status,finished_at,user_id,thread_id,thread_source_id,environment_id,session_id,operation,turn_id,source_link_id)
 SELECT $1,c.installation_id,'retained-input',$2,clock_timestamp(),'human',$3,clock_timestamp()+interval '1 minute','accepted',clock_timestamp(),$4,p.thread_id,p.id,p.environment_id,p.session_id,'enqueue',$5,gen_random_uuid() FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id WHERE p.session_id=$6`, request, digest[:], body, f.user, turn.TurnID, f.session)
			reconcileHistory(t, f)
			released := kind == "terminal-post" || kind == "terminal-stopped-stream" || kind == "terminal-open-stream" || kind == "terminal-uncertain-stream" || kind == "stopped-stream" || kind == "posted" || kind == "failed" || kind == "suppressed"
			if got := historyClock(t, f); (got != nil) != released {
				t.Fatalf("clock released=%v want=%v", got != nil, released)
			}
			// Recheck obligations at purge too: even an already-established elapsed clock
			// cannot release a newly observed pending mutation or its unread source.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_eligible_at=statement_timestamp()-interval '2 minutes' WHERE id=$1`, f.session)
			reconcileHistory(t, f)
			var purged bool
			if err := f.pool.QueryRow(t.Context(), `SELECT history_expired_at IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&purged); err != nil || purged != released {
				t.Fatalf("purge released=%v want=%v %v", purged, released, err)
			}
			var receiptRetained bool
			if err := f.pool.QueryRow(t.Context(), `SELECT (payload IS NULL)=$2 AND (payload_expired_at IS NOT NULL)=$2 AND request_digest=$3 AND status='accepted' AND turn_id=$4 FROM slack_requests WHERE id=$1`, request, released, digest[:], turn.TurnID).Scan(&receiptRetained); err != nil || !receiptRetained {
				t.Fatal("request expiry lost identity or ignored obligation", receiptRetained, err)
			}
			if kind != "unprojected" {
				if kind == "terminal-uncertain-stream" {
					var unknown bool
					if err := f.pool.QueryRow(t.Context(), `SELECT stream_status='uncertain' AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE session_id=$1`, f.session).Scan(&unknown); err != nil || !unknown {
						t.Fatal("retention fabricated remote stream closure", unknown, err)
					}
				}
				var contentExpired bool
				if err := f.pool.QueryRow(t.Context(), `SELECT (payload IS NULL)=$2 AND (payload_expired_at IS NOT NULL)=$2 AND post_key='content' AND octet_length(payload_digest)=32 AND (NOT $2 OR (confirmed_stream_text='' AND inflight_payload IS NULL AND inflight_attempt_id IS NULL)) FROM slack_posts WHERE session_id=$1`, f.session, released).Scan(&contentExpired); err != nil || !contentExpired {
					t.Fatal("post expiry lost identity or ignored delivery", contentExpired, err)
				}
			}

		})
	}
}

func TestSlackFrontHistoryWaitsForOwnedActivityToSettle(t *testing.T) {
	f := newAdmissionFixture(t)
	_, channel := slackRouteFixture(t, f)
	bindFixtureSlackRoot(t, f, channel)
	parent := f.enqueue(t, "parent")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := session.Caller{Kind: "session", MCPScopeID: f.mcpScope, ID: f.session, TurnID: parent.TurnID, Execution: f.execution(), Host: f.host()}
	req := f.startRequest("child")
	req.ComputerID = f.computer
	child, err := session.Spawn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	c := f
	c.session = child.SessionID
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status,attachment_sequence,control_sequence,control_attachment,control_kind,control_generation,control_acknowledged_at) VALUES($1,$2,1,$3,1,'ready',1,1,1,'resume',1,clock_timestamp())`, f.env, c.session, f.computer)
	if _, err = dispatchTurnForTest(t.Context(), f.pool, c.execution()); err != nil {
		t.Fatal(err)
	}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	fenceHistoryProcess(t, f)
	drain := func(sessionID uuid.UUID) {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_thread_sources src SET projected_event_seq=s.next_event_seq-1 FROM sessions s WHERE src.environment_id=s.environment_id AND src.session_id=s.id AND s.id=$1`, sessionID)
	}
	drain(f.session)
	drain(c.session)
	reconcileHistory(t, f)
	if historyClock(t, f) != nil {
		t.Fatal("front released while owned process can still update activity")
	}
	fenceHistoryProcess(t, c)
	// The physical stop can already be known while its event remains unread.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_thread_sources SET projected_event_seq=0 WHERE session_id=$1`, c.session)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE projects SET history_retention_seconds=60 WHERE id=(SELECT project_id FROM environments WHERE id=(SELECT environment_id FROM sessions WHERE id=$1))`, f.session)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_eligible_at=statement_timestamp()-interval '2 minutes' WHERE id=$1`, f.session)
	reconcileHistory(t, f)
	var expired bool
	if err = f.pool.QueryRow(t.Context(), `SELECT history_expired_at IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&expired); err != nil || expired {
		t.Fatal("unread child activity allowed front expiry", expired, err)
	}
	drain(c.session)
	reconcileHistory(t, f)
	if err = f.pool.QueryRow(t.Context(), `SELECT history_expired_at IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&expired); err != nil || !expired {
		t.Fatal("settled activity did not release front", expired, err)
	}
}
