package slack

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/executionlimit"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestSlackReceiptWindowsKeepMessagesLongerThanGestures(t *testing.T) {
	f := newStatusFixture(t)
	window := func(key string) time.Duration {
		t.Helper()
		var seconds float64
		if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch FROM expires_at-source_occurred_at) FROM slack_requests WHERE installation_id=$1 AND request_key=$2`, f.installation, key).Scan(&seconds); err != nil {
			t.Fatal(err)
		}
		return time.Duration(seconds * float64(time.Second))
	}
	events := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	if response := serveSignedEvent(events, eventBody(t, "mention", map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> work"})); response.Code != 200 {
		t.Fatalf("mention receipt: %d", response.Code)
	}
	if got := window("mention"); got != messageAdmissionWindow {
		t.Fatalf("mention window %s", got)
	}
	f.nativeStop(t, "native-stop")
	if got := window("native-stop"); got != gestureExpiry {
		t.Fatalf("native Stop window %s", got)
	}
	interactions := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	turn, ask := f.pendingQuestion(t)
	claims := f.control("choose")
	claims.Target.Turn, claims.Target.Ask, claims.OptionID = turn, ask, "yes"
	claims.ExpiresAt = time.Now().Add(time.Hour).Unix()
	if response := serveSignedInteraction(interactions, interactionBody(t, interactions, claims, "human", slackNow())); response.Code != 200 {
		t.Fatalf("button receipt: %d", response.Code)
	}
	var key string
	if err := f.Pool.QueryRow(t.Context(), `SELECT request_key FROM slack_requests WHERE request_key NOT IN ('mention','native-stop')`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if got := window(key); got != gestureExpiry {
		t.Fatalf("button window %s", got)
	}
}

// waitingMessage receives a new channel mention with the production admission window.
func (f statusFixture) waitingMessage(t *testing.T, key string) uuid.UUID {
	t.Helper()
	payload, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Text: "<@bot> new task"})
	now := time.Now().UTC()
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: key, Actor: "human", OccurredAt: now, ExpiresAt: now.Add(messageAdmissionWindow), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}

// withoutExecution makes new execution wait for the given reason and returns
// the limits to admit with and a function that frees execution again.
func (f statusFixture) withoutExecution(t *testing.T, reason string) (executionlimit.Limits, func()) {
	t.Helper()
	limits := sessiontest.ExecutionLimits()
	switch reason {
	case "execution_limit":
		// The fixture's resident Computer already uses the only resident place.
		limits.MaxResidentComputers = 1
		return limits, func() {}
	case "execution_capacity_exhausted":
		var pools []byte
		if err := f.Pool.QueryRow(t.Context(), `SELECT capacity_pools FROM worker_groups WHERE id=$1`, f.Group).Scan(&pools); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET capacity_pools=jsonb_set(capacity_pools,'{0,max_additional_workers}','0') WHERE id=$1`, f.Group)
		return limits, func() {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET capacity_pools=$2 WHERE id=$1`, f.Group, pools)
		}
	}
	t.Fatalf("unknown reason %s", reason)
	return limits, nil
}

func TestSlackMessageWaitsForExecutionWithinItsWindow(t *testing.T) {
	for _, reason := range []string{"execution_limit", "execution_capacity_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.link(t)
			id := f.waitingMessage(t, "waiting")
			limits, free := f.withoutExecution(t, reason)
			var tokens string
			if err := f.Pool.QueryRow(t.Context(), `SELECT admission_tokens::text||'@'||admission_refilled_at::text FROM environments WHERE id=$1`, f.Environment).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, limits, f.Pool, nil, f.admissionClient(t), id); err != nil {
				t.Fatal(err)
			}
			var waiting bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='received' AND r.error=$2 AND r.finished_at IS NULL
 AND r.next_attempt_at BETWEEN clock_timestamp()+interval '50 seconds' AND clock_timestamp()+interval '1 minute'
 AND (SELECT count(*) FROM sessions WHERE environment_id=$3)=1 AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$3)
 AND (SELECT admission_tokens::text||'@'||admission_refilled_at::text FROM environments WHERE id=$3)=$4
 FROM slack_requests r WHERE r.id=$1`, id, reason, f.Environment, tokens).Scan(&waiting); err != nil || !waiting {
				var state string
				_ = f.Pool.QueryRow(t.Context(), `SELECT status||':'||coalesce(error,'')||':'||(next_attempt_at-clock_timestamp())::text FROM slack_requests WHERE id=$1`, id).Scan(&state)
				t.Fatal("message did not wait for execution without side effects", waiting, state, err)
			}
			if due, err := claimRequests(t.Context(), f.Pool, 64); err != nil || len(due) != 0 {
				t.Fatal("waiting message retried before a minute", due, err)
			}
			free()
			sessiontest.ObserveProvisioning(t, f.Fixture)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET next_attempt_at=clock_timestamp() WHERE id=$1`, id)
			if due, err := claimRequests(t.Context(), f.Pool, 64); err != nil || len(due) != 1 || due[0] != id {
				t.Fatal("waiting message not retried", due, err)
			}
			if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, nil, f.admissionClient(t), id); err != nil {
				t.Fatal(err)
			}
			var accepted bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='accepted' AND operation='start' AND error IS NULL FROM slack_requests WHERE id=$1`, id).Scan(&accepted); err != nil || !accepted {
				t.Fatal("message not admitted once execution freed", accepted, err)
			}
		})
	}
}

func TestSlackWaitingMessageExpiresWithItsReason(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	id := f.waitingMessage(t, "expiring")
	limits, _ := f.withoutExecution(t, "execution_limit")
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, limits, f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	if err := ReconcileRequest(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, limits, f.Pool, nil, testProjectionConfig(), nil, id); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='execution_limit' AND finished_at IS NOT NULL FROM slack_requests WHERE id=$1`, id).Scan(&rejected); err != nil || !rejected {
		var state string
		_ = f.Pool.QueryRow(t.Context(), `SELECT status||':'||coalesce(error,'') FROM slack_requests WHERE id=$1`, id).Scan(&state)
		t.Fatal("expired message lost its reason", rejected, state, err)
	}
	intent, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
	if err != nil || !found || intent == nil {
		t.Fatal("no feedback for the expired message", found, err)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(intent.payload, &body); err != nil || !strings.Contains(body.Text, "not accepted within an hour") || !strings.Contains(body.Text, "execution quota") {
		t.Fatalf("feedback does not explain the wait: %q %v", body.Text, err)
	}
}

func TestSlackButtonGestureStillExpiresAfterFiveMinutes(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	interactions := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	turn, ask := f.pendingQuestion(t)
	claims := f.control("choose")
	claims.Target.Turn, claims.Target.Ask, claims.OptionID = turn, ask, "yes"
	claims.ExpiresAt = time.Now().Add(time.Hour).Unix()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=clock_timestamp()-interval '1 hour',connected_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, f.installation)
	clicked := time.Now().Add(-gestureExpiry - time.Second)
	stale := fmt.Sprintf("%d.%06d", clicked.Unix(), clicked.Nanosecond()/1000)
	if response := serveSignedInteraction(interactions, interactionBody(t, interactions, claims, "human", stale)); response.Code != 200 {
		t.Fatalf("button receipt: %d", response.Code)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(status='rejected' AND error='gesture_expired') FROM slack_requests`).Scan(&rejected); err != nil || !rejected {
		var state string
		_ = f.Pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(status||':'||coalesce(error,''),','),'none') FROM slack_requests`).Scan(&state)
		t.Fatal("button older than five minutes was not expired", rejected, state, err)
	}
}

func TestSlackReplyWaitsWhileExecutionSettles(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	payload, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Thread: "123.456", Text: "<@bot> follow-up"})
	now := time.Now().UTC()
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "settling", Actor: "human", OccurredAt: now, ExpiresAt: now.Add(messageAdmissionWindow), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer)
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, nil, f.admissionClient(t), receipt.ID); err != nil {
		t.Fatal(err)
	}
	var waiting bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='received' AND error='execution_settling' AND next_attempt_at>clock_timestamp()+interval '50 seconds' FROM slack_requests WHERE id=$1`, receipt.ID).Scan(&waiting); err != nil || !waiting {
		var state string
		_ = f.Pool.QueryRow(t.Context(), `SELECT status||':'||coalesce(error,'') FROM slack_requests WHERE id=$1`, receipt.ID).Scan(&state)
		t.Fatal("settling execution did not defer the reply", waiting, state, err)
	}
}
