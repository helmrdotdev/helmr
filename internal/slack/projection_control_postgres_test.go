package slack

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"

	"github.com/helmrdotdev/helmr/internal/session"
)

func TestSlackNativeStopWaitsForCessationBeforeNoticeAndNewDispatch(t *testing.T) {
	for _, early := range []bool{false, true} {
		name := "after-cessation"
		if early {
			name = "before-cessation"
		}
		t.Run(name, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			f.outputWriter(t)("Original explanation.")
			e := activityExecution(f)
			host := activityHost(f)
			var attachment int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT attachment_sequence FROM session_processes WHERE session_id=$1`, f.Session).Scan(&attachment); err != nil {
				t.Fatal(err)
			}
			stopped := f.nativeStop(t, "stop")
			if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, stopped); err != nil {
				t.Fatal(err)
			}
			again := f.nativeStop(t, "stop-again")
			if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, again); err != nil {
				t.Fatal(err)
			}
			projectAllActivity(t, f)
			notices := func(want int) {
				t.Helper()
				var count int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND post_key LIKE 'stopped:%'`, f.thread).Scan(&count); err != nil || count != want {
					t.Fatalf("cessation notices=%d want=%d: %v", count, want, err)
				}
			}
			notices(0)
			incoming := func() {
				t.Helper()
				id := f.reply(t, "new-work", false)
				if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), id); err != nil {
					t.Fatal(err)
				}
			}
			if early {
				incoming()
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.Session).Scan(&e.AuthorityGeneration); err != nil {
				t.Fatal(err)
			}
			if _, err := dispatchRuntimeTurn(t.Context(), f.Pool, e); !errors.Is(err, session.ErrNotReady) {
				t.Fatalf("dispatch overlapped stopping work: %v", err)
			}
			delivery, err := session.PrepareSessionDelivery(t.Context(), f.Pool, host, e, attachment)
			if err != nil {
				t.Fatal(err)
			}
			if delivery.StopTurnID == nil {
				t.Fatal("native stop missing cessation obligation")
			}
			if err = session.AcknowledgeSessionDelivery(t.Context(), f.Pool, host, e, attachment, delivery.Sequence, delivery.Generation, delivery.Kind, "", delivery.StopSequence); err != nil {
				t.Fatal(err)
			}
			projectAllActivity(t, f)
			notices(1)
			projectAllActivity(t, f)
			notices(1)
			if !early {
				incoming()
				if err := f.Pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.Session).Scan(&e.AuthorityGeneration); err != nil {
					t.Fatal(err)
				}
				delivery, err = session.PrepareSessionDelivery(t.Context(), f.Pool, host, e, attachment)
				if err != nil {
					t.Fatal(err)
				}
				if err = session.AcknowledgeSessionDelivery(t.Context(), f.Pool, host, e, attachment, delivery.Sequence, delivery.Generation, delivery.Kind, "", delivery.StopSequence); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = dispatchRuntimeTurn(t.Context(), f.Pool, e); err != nil {
				t.Fatalf("new work did not resume: %v", err)
			}
			projectAllActivity(t, f)
			notices(1)
		})
	}
}

func TestSlackProjectionDiscoversLaterPhysicalFenceWithoutNewEvent(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	f.outputWriter(t)("Original explanation.")
	stopped := f.nativeStop(t, "stop")
	if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, stopped); err != nil {
		t.Fatal(err)
	}
	// This is the persisted shape of loss followed by later physical fencing:
	// the second observation does not need another execution failure event.
	if _, err := f.Pool.Exec(t.Context(), `UPDATE session_processes SET status='lost',failure_recorded_at=clock_timestamp() WHERE session_id=$1`, f.Session); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	var before int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT next_event_seq FROM sessions WHERE id=$1`, f.Session).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE session_processes SET fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.next_event_seq=$2 AND (SELECT count(*) FROM slack_posts WHERE thread_id=$3 AND post_key LIKE 'stopped:%')=1 FROM sessions s WHERE s.id=$1`, f.Session, before, f.thread).Scan(&correct); err != nil || !correct {
		t.Fatalf("physical fencing did not reconcile: %v %v", correct, err)
	}
}

func TestSlackProjectionDisposesControlsForInactiveSources(t *testing.T) {
	for _, inactive := range []string{"revoked", "deleted"} {
		t.Run(inactive, func(t *testing.T) {
			f := newStatusFixture(t)
			f.outputWriter(t)("Retained explanation.")
			projectAllActivity(t, f)
			var before int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(desired_revision),0) FROM slack_posts WHERE thread_id=$1`, f.thread).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if inactive == "revoked" {
				if _, err := f.Pool.Exec(t.Context(), `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.Pool.Exec(t.Context(), `UPDATE slack_threads SET deleted_at=clock_timestamp() WHERE id=$1`, f.thread); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.Pool.Exec(t.Context(), `UPDATE session_processes SET control_acknowledged_at=clock_timestamp() WHERE session_id=$1`, f.Session); err != nil {
				t.Fatal(err)
			}
			if progressed, err := ReconcileProjection(t.Context(), f.Pool, testProjectionConfig(), 1); err != nil || progressed != 1 {
				t.Fatalf("control disposition=%d: %v", progressed, err)
			}
			if progressed, err := ReconcileProjection(t.Context(), f.Pool, testProjectionConfig(), 1); err != nil || progressed != 0 {
				t.Fatalf("inactive source remained discoverable: %d %v", progressed, err)
			}
			var after int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(desired_revision),0) FROM slack_posts WHERE thread_id=$1`, f.thread).Scan(&after); err != nil || after != before {
				t.Fatalf("inactive route published: %d -> %d: %v", before, after, err)
			}
		})
	}
}

func TestSlackNativeStopBeforeStartupPostsCessationAndAdmitsNewMessage(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	turn, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "first", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.Session); err != nil {
		t.Fatal(err)
	}
	stopped := f.nativeStop(t, "immediate-stop")
	if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, stopped); err != nil {
		t.Fatal(err)
	}
	e, host := activityExecution(f), activityHost(f)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.Pool, host, e)
	if err != nil || !attachment.RetireAbsent {
		t.Fatalf("startup not retired: %+v %v", attachment, err)
	}
	projectAllActivity(t, f)
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND post_key LIKE 'stopped:%'`, f.thread).Scan(&count); err != nil || count != 0 {
		t.Fatalf("premature cessation: %d %v", count, err)
	}
	if err := session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, host, e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND post_key LIKE 'stopped:%'`, f.thread).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing cessation: %d %v", count, err)
	}
	incoming := f.reply(t, "after-stop", false)
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), incoming); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT old.status='interrupted' AND old.started_at IS NULL AND incoming.status='accepted' AND next.status='queued' AND s.status='open' AND NOT EXISTS(SELECT 1 FROM session_holds h WHERE h.session_id=s.id AND h.released_at IS NULL) FROM turns old JOIN sessions s ON s.id=old.session_id JOIN slack_requests incoming ON incoming.id=$2 JOIN turns next ON next.id=incoming.turn_id WHERE old.id=$1`, turn.TurnID, incoming).Scan(&correct); err != nil || !correct {
		t.Fatalf("stopped work replayed or continuation blocked: %v %v", correct, err)
	}
}
