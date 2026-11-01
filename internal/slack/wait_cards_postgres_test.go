package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

type recordedCall struct {
	method string
	body   map[string]any
}

type recordingCaller struct{ calls []recordedCall }

func (r *recordingCaller) VerifyConversation(context.Context, uuid.UUID, int64, string, string, string) error {
	return nil
}

func (r *recordingCaller) Call(_ context.Context, _ uuid.UUID, _ int64, method string, payload []byte) DeliveryResult {
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	r.calls = append(r.calls, recordedCall{method, body})
	return DeliveryResult{Disposition: Acknowledged, Timestamp: "200.000001"}
}

func (r *recordingCaller) drain(t *testing.T, f statusFixture) []recordedCall {
	t.Helper()
	r.calls = nil
	if _, err := ReconcileWaitCards(t.Context(), f.Pool, r, 8); err != nil {
		t.Fatal(err)
	}
	return r.calls
}

func cardOf(t *testing.T, call recordedCall) (title, status string) {
	t.Helper()
	blocks, _ := call.body["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("card blocks: %+v", call.body)
	}
	card, _ := blocks[0].(map[string]any)
	if card["type"] != "task_card" {
		t.Fatalf("card: %+v", card)
	}
	return card["title"].(string), card["status"].(string)
}

func (f statusFixture) messageTimestamp(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var ts string
	if err := f.Pool.QueryRow(t.Context(), `SELECT convert_from(payload,'UTF8')::jsonb->>'message_ts' FROM slack_requests WHERE id=$1`, id).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestSlackDeferredMentionShowsItsWaitUntilAdmitted(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	id := f.waitingMessage(t, "waiting")
	root := f.messageTimestamp(t, id)
	limits, free := f.withoutExecution(t, "execution_capacity_exhausted")
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, limits, f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	caller := &recordingCaller{}
	calls := caller.drain(t, f)
	if len(calls) != 2 || calls[0].method != "agents.sessions.setStatus" || calls[0].body["status"] != "processing" || calls[0].body["thread_ts"] != root ||
		calls[1].method != "chat.postMessage" || calls[1].body["thread_ts"] != root {
		t.Fatalf("wait shown: %+v", calls)
	}
	if title, status := cardOf(t, calls[1]); title != "Waiting for Worker capacity" || status != "in_progress" {
		t.Fatalf("wait card: %s %s", title, status)
	}
	if calls := caller.drain(t, f); len(calls) != 0 {
		t.Fatalf("waiting card repeated: %+v", calls)
	}
	free()
	sessiontest.ObserveProvisioning(t, f.Fixture)
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	calls = caller.drain(t, f)
	if len(calls) != 1 || calls[0].method != "chat.update" || calls[0].body["ts"] != "200.000001" {
		t.Fatalf("admitted wait: %+v", calls)
	}
	if title, status := cardOf(t, calls[0]); title != "Ready to start" || status != "complete" {
		t.Fatalf("admitted card: %s %s", title, status)
	}
}

func TestSlackExpiredWaitEndsInErrorAndClearsItsStatus(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	id := f.waitingMessage(t, "expiring")
	root := f.messageTimestamp(t, id)
	limits, _ := f.withoutExecution(t, "execution_limit")
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, limits, f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	caller := &recordingCaller{}
	caller.drain(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	if err := ReconcileRequest(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, limits, f.Pool, nil, testProjectionConfig(), nil, id); err != nil {
		t.Fatal(err)
	}
	calls := caller.drain(t, f)
	if len(calls) != 2 || calls[0].method != "chat.update" || calls[1].method != "agents.sessions.setStatus" || calls[1].body["status"] != "active" || calls[1].body["thread_ts"] != root {
		t.Fatalf("expired wait: %+v", calls)
	}
	if title, status := cardOf(t, calls[0]); title != "Waiting for the execution limit" || status != "error" {
		t.Fatalf("expired card: %s %s", title, status)
	}
}

func TestSlackStopWithdrawsAWaitingReplyInItsThread(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.reply(t, "waiting-reply", false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET error='execution_capacity_exhausted',next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, id)
	caller := &recordingCaller{}
	caller.drain(t, f)
	f.assertAggregate(t, "processing")
	stop := f.nativeStop(t, "stop")
	// The idle Session has nothing else to stop, so it gets no Stop hold.
	f.receiptStatus(t, stop, "rejected")
	var withdrawn bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='stopped_waiting' FROM slack_requests WHERE id=$1`, id).Scan(&withdrawn); err != nil || !withdrawn {
		t.Fatalf("waiting reply not withdrawn: %v %v", withdrawn, err)
	}
	calls := caller.drain(t, f)
	if len(calls) != 1 || calls[0].method != "chat.update" {
		t.Fatalf("stopped wait: %+v", calls)
	}
	if title, status := cardOf(t, calls[0]); title != "Stopped" || status != "error" {
		t.Fatalf("stopped card: %s %s", title, status)
	}
	f.assertAggregate(t, "active")
	for {
		feedback, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
		if err != nil || !found {
			t.Fatalf("no feedback for the withdrawn reply: %v", err)
		}
		if feedback == nil || !strings.Contains(string(feedback.payload), "Stopped.") {
			continue
		}
		if feedback.method != "chat.postEphemeral" || !strings.Contains(string(feedback.payload), `"thread_ts":"123.456"`) {
			t.Fatalf("withdrawn feedback: %s %s", feedback.method, feedback.payload)
		}
		return
	}
}

func TestSlackSettlingOneWaitKeepsAnotherWaitWorking(t *testing.T) {
	f := newStatusFixture(t)
	first, second := f.newMessage(t, "first", "555.000001"), f.newMessage(t, "second", "555.000001")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET error='execution_limit',next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id IN ($1,$2)`, first, second)
	caller := &recordingCaller{}
	caller.drain(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp() WHERE id=$1`, first)
	calls := caller.drain(t, f)
	if len(calls) != 1 || calls[0].method != "chat.update" {
		t.Fatalf("one settled wait cleared the other's status: %+v", calls)
	}
	// A wait settled concurrently may still clear the shared status; the
	// remaining wait reasserts it within a minute.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET wait_status_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, second)
	calls = caller.drain(t, f)
	if len(calls) != 1 || calls[0].method != "agents.sessions.setStatus" || calls[0].body["status"] != "processing" || calls[0].body["thread_ts"] != "555.000001" {
		t.Fatalf("waiting status not reasserted: %+v", calls)
	}
}

func TestSlackWaitCardFollowsCurrentPublication(t *testing.T) {
	f := newStatusFixture(t)
	id, _ := f.message(t, "C1")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET error='execution_limit',next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1;
 UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE slack_installation_id=$2`, pgx.QueryExecModeSimpleProtocol, id, f.installation)
	if calls := (&recordingCaller{}).drain(t, f); len(calls) != 0 {
		t.Fatalf("revoked connection published a wait: %+v", calls)
	}
}

func TestSlackWaitSettlesOnlyAfterAnOutstandingRefresh(t *testing.T) {
	f := newStatusFixture(t)
	id := f.newMessage(t, "refreshing", "666.000001")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET error='execution_limit',next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, id)
	caller := &recordingCaller{}
	caller.drain(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET wait_status_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, id)
	refresh, found, err := takeWaitCard(t.Context(), f.Pool)
	if err != nil || !found || refresh == nil || !refresh.refreshing {
		t.Fatalf("refresh claim: %+v %v %v", refresh, found, err)
	}
	// The message expires while the refresh call is still outstanding.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp() WHERE id=$1`, id)
	if calls := caller.drain(t, f); len(calls) != 0 {
		t.Fatalf("settled under an outstanding refresh: %+v", calls)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET wait_card='waiting' WHERE id=$1 AND wait_card='posting'`, id)
	calls := caller.drain(t, f)
	if len(calls) != 2 || calls[0].method != "chat.update" || calls[1].body["status"] != "active" {
		t.Fatalf("settlement after refresh: %+v", calls)
	}
}

func TestSlackAbandonedWaitRefreshIsClaimedAgain(t *testing.T) {
	f := newStatusFixture(t)
	id := f.newMessage(t, "abandoned", "777.000001")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET error='execution_limit',next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, id)
	caller := &recordingCaller{}
	caller.drain(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET wait_status_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, id)
	// A worker claims the refresh and never returns.
	if refresh, found, err := takeWaitCard(t.Context(), f.Pool); err != nil || !found || refresh == nil || !refresh.refreshing {
		t.Fatalf("refresh claim: %+v %v %v", refresh, found, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET wait_status_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, id)
	calls := caller.drain(t, f)
	if len(calls) != 1 || calls[0].body["status"] != "processing" {
		t.Fatalf("abandoned refresh not retried: %+v", calls)
	}
}
