package agent

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackHistoryRetentionWaitsForProjectionAndDelivery(t *testing.T) {
	for _, kind := range []string{"unprojected", "pending", "sending", "uncertain", "open-stream", "uncertain-stream", "newer-revision", "stopped-stream", "posted", "failed", "suppressed", "abandoned-post", "abandoned-uncertain-stream", "abandoned-open-stream", "abandoned-stopped-stream"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			_, channel := slackRouteFixture(t, f)
			bindFixtureSlackRoot(t, f, channel)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_retention_mode='duration',history_retention_seconds=60 WHERE environment_id=$1 AND id=$2`, f.env, f.session)
			turn := f.enqueue(t, "retention")
			if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			if _, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), turn.TurnID, uuid.NewV7(), json.RawMessage(`"retained source"`)); err != nil {
				t.Fatal(err)
			}
			if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
				t.Fatal(err)
			}
			fenceHistoryProcess(t, f)
			if kind != "unprojected" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_thread_sources p SET projected_event_seq=s.next_event_seq-1 FROM sessions s WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND s.id=$1`, f.session)
				body := []byte(`{"text":"retained source"}`)
				digest := sha256.Sum256(body)
				post := uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_posts(id,environment_id,session_id,thread_id,thread_source_id,seq,publication_key,role,turn_id,payload,payload_digest,presentation_path)
 SELECT $1,environment_id,session_id,thread_id,id,1,'content','intermediate',$3,$4,$5,'post' FROM slack_thread_sources WHERE session_id=$2`, post, f.session, turn.TurnID, body, digest[:])
				switch kind {
				case "abandoned-post", "abandoned-uncertain-stream", "abandoned-open-stream", "abandoned-stopped-stream":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status='failed',error='delivery_abandoned',closed_at=clock_timestamp(),delivery_disposed_at=clock_timestamp(),delivery_disposed_by=$2,inflight_method='chat.postMessage',inflight_payload=payload,inflight_digest=payload_digest,inflight_revision=1,inflight_attempt_id=$3 WHERE id=$1`, post, f.user, uuid.NewV7())
					if kind != "abandoned-post" {
						state := "uncertain"
						if kind == "abandoned-open-stream" {
							state = "open"
						}
						if kind == "abandoned-stopped-stream" {
							state = "stopped"
						}
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET presentation_path='stream',recipient_team_id='team',recipient_user_id='human',stream_state=$2,inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL WHERE id=$1`, post, state)
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
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET presentation_path='stream',recipient_team_id='team',recipient_user_id='human',stream_state=$2,confirmed_stream_text='retained confirmed text' WHERE id=$1`, post, state)
					}
				case "failed", "suppressed":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status=$2,error='delivery_unavailable' WHERE id=$1`, post, kind)
				}
			}
			request := uuid.NewV7()
			body := []byte(`{"message_ts":"123.456","text":"retained input"}`)
			digest := sha256.Sum256(body)
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_requests(id,installation_id,request_key,request_digest,source_occurred_at,slack_user_id,payload,expires_at,status,finished_at,user_id,thread_id,thread_source_id,environment_id,session_id,operation,turn_id)
 SELECT $1,c.installation_id,'retained-input',$2,clock_timestamp(),'human',$3,clock_timestamp()+interval '1 minute','accepted',clock_timestamp(),$4,p.thread_id,p.id,p.environment_id,p.session_id,'enqueue',$5 FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_channels c ON c.id=t.channel_id WHERE p.session_id=$6`, request, digest[:], body, f.user, turn.TurnID, f.session)
			reconcileHistory(t, f)
			released := kind == "abandoned-post" || kind == "abandoned-stopped-stream" || kind == "stopped-stream" || kind == "posted" || kind == "failed" || kind == "suppressed"
			if got := historyClock(t, f); (got != nil) != released {
				t.Fatalf("clock released=%v want=%v", got != nil, released)
			}
			// Recheck obligations at purge too: even an already-established elapsed clock
			// cannot release a newly observed pending mutation or its unread source.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_eligible_at=statement_timestamp()-interval '2 minutes',history_expires_at=EXTRACT(epoch FROM statement_timestamp())-60 WHERE id=$1`, f.session)
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
				var contentExpired bool
				if err := f.pool.QueryRow(t.Context(), `SELECT (payload IS NULL)=$2 AND (payload_expired_at IS NOT NULL)=$2 AND publication_key='content' AND octet_length(payload_digest)=32 AND (NOT $2 OR (confirmed_stream_text='' AND inflight_payload IS NULL AND inflight_attempt_id IS NULL)) FROM slack_posts WHERE session_id=$1`, f.session, released).Scan(&contentExpired); err != nil || !contentExpired {
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
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, TurnID: parent.TurnID, Execution: f.execution(), Host: f.host()}
	req := f.startRequest("child")
	req.ComputerID = f.computer
	child, err := Spawn(t.Context(), f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	c := f
	c.session = child.SessionID
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,1,$3,1,'ready')`, f.env, c.session, f.computer)
	if _, err = Dispatch(t.Context(), f.pool, c.execution()); err != nil {
		t.Fatal(err)
	}
	if _, err = ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	fenceHistoryProcess(t, f)
	drain := func(session uuid.UUID) {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_thread_sources src SET projected_event_seq=s.next_event_seq-1 FROM sessions s WHERE src.environment_id=s.environment_id AND src.session_id=s.id AND s.id=$1`, session)
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
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_retention_mode='duration',history_retention_seconds=60,history_eligible_at=clock_timestamp()-interval '2 minutes',history_expires_at=EXTRACT(epoch FROM clock_timestamp())-60 WHERE id=$1`, f.session)
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
