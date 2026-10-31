package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

func (f statusFixture) assertAggregate(t *testing.T, want string) {
	t.Helper()
	var got string
	err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		var err error
		got, err = aggregateStatus(t.Context(), tx, f.thread, f.Environment)
		return err
	})
	if err != nil || got != want {
		t.Fatalf("status %q want %q: %v", got, want, err)
	}
}

func TestSlackClosedStatusFollowsExternalCloseCancelAndIdleExpiry(t *testing.T) {
	for _, kind := range []string{"close", "cancel", "idle"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			f.assertAggregate(t, "active")
			if kind == "idle" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1;
 UPDATE agent_definitions d SET close_after_idle_ms=1 FROM sessions s WHERE s.id=$1 AND (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id);
 UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Session)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() {
					done <- session.RunSessionLifecycle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
				}()
				t.Cleanup(func() {
					cancel()
					if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				})
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				poll := time.NewTicker(10 * time.Millisecond)
				defer poll.Stop()
				for {
					var closed bool
					if err := f.Pool.QueryRow(t.Context(), `SELECT status='closed' FROM sessions WHERE id=$1`, f.Session).Scan(&closed); err != nil {
						t.Fatal(err)
					}
					if closed {
						break
					}
					select {
					case <-deadline.C:
						t.Fatal("idle expiry did not close Session")
					case <-poll.C:
					}
				}
			} else {
				if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: kind, IdempotencyKey: kind}); err != nil {
					t.Fatal(err)
				}
				if err := session.ReconcileSessionClosure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Session); err != nil {
					t.Fatal(err)
				}
			}
			// A terminal request is not evidence of physical cessation.
			f.assertAggregate(t, "processing")
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
			// Do not close the native session ahead of its last event/output projection.
			f.assertAggregate(t, "processing")
			projectAllActivity(t, f)
			f.assertAggregate(t, "closed")
			claim := f.claim(t, "closed")
			if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			later := f.threadMessage(t, "after-end", "more work", time.Now())
			f.admitThreadMessage(t, later)
			f.receiptStatus(t, later, "rejected")
			hint, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
			if err != nil || !found || hint == nil {
				t.Fatalf("terminal guidance missing: %v", err)
			}
			var body map[string]any
			if err = json.Unmarshal(hint.payload, &body); err != nil || body["text"] != "This Session is closed or unavailable. Mention the Agent in a new thread to start a new Session." {
				t.Fatalf("terminal guidance: %s %v", hint.payload, err)
			}
			var n int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("terminal reply created replacement: %d %v", n, err)
			}
		})
	}
}

func TestSlackClosedStatusWaitsForAllContributorsAndFinalDelivery(t *testing.T) {
	f := newStatusFixture(t)
	peer := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1;
 UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1;
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id,origin_api_key_id,origin_activation_id) SELECT environment_id,$2,agent_id,deployment_id,computer_id,$2,0,origin_user_id,origin_api_key_id,origin_activation_id FROM sessions WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Session, peer)
	projectAllActivity(t, f)
	// Another Session on the same Computer does not keep this Slack thread open.
	f.assertAggregate(t, "closed")
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($1,$2,$3,$4)`, uuid.NewV7(), f.thread, f.Environment, peer)
	f.assertAggregate(t, "active")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, peer)
	post := f.post(t, 1, "last-output", "lifecycle", nil)
	f.assertAggregate(t, "processing")
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	f.assertAggregate(t, "processing")
	// Exact delivery observation settles the outstanding post.
	if found, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !found {
		t.Fatalf("observe: %v %v", found, err)
	}
	f.assertAggregate(t, "closed")
}

func TestSlackClosedStatusWaitsForLaterPhysicalFenceProjection(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	stop := f.nativeStop(t, "stop")
	if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, stop); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='lost',failure_recorded_at=clock_timestamp(),failure_reason='process_failed' WHERE session_id=$1`, f.Session)
	projectAllActivity(t, f)
	var before int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT next_event_seq FROM sessions WHERE id=$1`, f.Session).Scan(&before); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
	f.assertAggregate(t, "processing")
	projectAllActivity(t, f)
	var post uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_posts WHERE thread_id=$1 AND post_key LIKE 'stopped:%'`, f.thread).Scan(&post); err != nil {
		t.Fatal(err)
	}
	f.assertAggregate(t, "processing")
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "456.789"}); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT next_event_seq FROM sessions WHERE id=$1`, f.Session).Scan(&after); err != nil || before != after {
		t.Fatalf("fence added event: %d -> %d: %v", before, after, err)
	}
	f.assertAggregate(t, "closed")
}
