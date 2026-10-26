package session_test

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestMessagesKeepExactDispositionAndDrain(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	req := session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "steer", Input: json.RawMessage(`[{"type":"text","text":"{\"text\":\"one\\u0000雪\"}"}]`)}
	canonicalInput, err := content.Input(req.Input)
	req.Input = canonicalInput
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req, first.TurnID); !errors.Is(err, session.ErrMessageClosed) {
		t.Fatalf("unregistered: %v", err)
	}
	if err := session.RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	results := make(chan session.SendReceipt, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, err := session.RouteSlackInput(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
			if err != nil {
				t.Error(err)
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	var admitted session.SendReceipt
	for r := range results {
		if admitted.MessageID == uuid.Nil() {
			admitted = r
		}
		if r.MessageID != admitted.MessageID || r.TurnID != first.TurnID {
			t.Fatal("send retry changed disposition")
		}
	}
	req.IdempotencyKey = "later"
	later, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req, first.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	message, err := session.ClaimRuntimeMessage(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, first.TurnID, nil)
	if err != nil || message == nil || message.ID != admitted.MessageID || string(message.Input) != string(req.Input) {
		t.Fatalf("claim: %+v %v", message, err)
	}
	repeated, err := session.ClaimRuntimeMessage(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, first.TurnID, nil)
	if err != nil || repeated == nil || repeated.ID != message.ID {
		t.Fatalf("claim retry: %+v %v", repeated, err)
	}
	if err := session.RuntimeCloseProcessing(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='rejected' AND rejection_reason='turn_closed' FROM turn_messages WHERE id=$1`, later.MessageID).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("unstarted message survived: %v %v", rejected, err)
	}
	if outcome, err := session.RuntimeFinalize(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), f.execution(), first.TurnID, json.RawMessage(`null`), "joined"); err != nil || outcome != nil {
		t.Fatalf("started callback finalized: %s %v", outcome, err)
	}
	if err := session.CompleteRuntimeMessage(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), attachment.Sequence, first.TurnID, message.ID, "message_rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.RuntimeFinalize(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), f.execution(), first.TurnID, json.RawMessage(`null`), "joined"); err != nil {
		t.Fatal(err)
	}
	req.IdempotencyKey = "steer"
	retried, err := session.RouteSlackInput(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || retried.MessageID != admitted.MessageID {
		t.Fatalf("rejected steering fell back: %+v %v", retried, err)
	}
	req.Input = json.RawMessage(`[{"type":"text","text":"false"}]`)
	if _, err := session.RouteSlackInput(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	req.IdempotencyKey = "new"
	queued, err := session.RouteSlackInput(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || queued.MessageID != uuid.Nil() || queued.TurnID == first.TurnID {
		t.Fatalf("new send didn't queue: %+v %v", queued, err)
	}
}
func TestMessagesCancellationAndExpiredRetry(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	req := session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "steer", Input: json.RawMessage(`[]`)}
	admitted, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req, first.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='rejected' AND rejection_reason='turn_terminated' FROM turn_messages WHERE id=$1`, admitted.MessageID).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("cancel didn't reject: %v %v", rejected, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turn_messages SET message=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, admitted.MessageID)
	repeated, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req, first.TurnID)
	if err != nil || repeated.MessageID != admitted.MessageID {
		t.Fatalf("expired retry: %+v %v", repeated, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE user_id=$1`, f.user)
	if _, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req, first.TurnID); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
}

func TestMessagesSteerClosingSessionWithoutEnqueueAuthority(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "close", IdempotencyKey: "close"}); err != nil {
		t.Fatal(err)
	}
	runtime := session.Caller{Kind: "session", ID: f.session, TurnID: first.TurnID, Execution: f.execution(), Host: f.host()}
	for _, caller := range []session.Caller{f.caller()} {
		req := session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, Input: json.RawMessage(`[]`)}
		exact, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, req, first.TurnID)
		if err != nil || exact.MessageID == uuid.Nil() {
			t.Fatalf("closing exact %s: %+v %v", caller.Kind, exact, err)
		}
		routed, err := session.RouteSlackInput(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req)
		if err != nil || routed.MessageID == uuid.Nil() {
			t.Fatalf("closing send %s: %+v %v", caller.Kind, routed, err)
		}
		if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req); err == nil {
			t.Fatalf("closing enqueue allowed: %s", caller.Kind)
		}
	}
	if err := session.RuntimeCloseProcessing(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []session.Caller{f.caller(), runtime} {
		if _, err := session.RouteSlackInput(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, Input: json.RawMessage(`[]`)}); err == nil {
			t.Fatalf("closing send fell back: %s", caller.Kind)
		}
	}
}

func TestRuntimeSendTurnRequiresOwnedTarget(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	sibling := ownedFixture(t, f)
	turns := make(map[uuid.UUID]uuid.UUID)
	for _, target := range []fixture{f, child, peer, sibling} {
		admission := target.enqueue(t, "active")
		if _, err := dispatchTurnForTest(t.Context(), target.pool, target.execution()); err != nil {
			t.Fatal(err)
		}
		if err := session.RuntimeRegisterMessages(t.Context(), target.pool, *target.host(), target.execution(), admission.TurnID); err != nil {
			t.Fatal(err)
		}
		turns[target.session] = admission.TurnID
	}
	rootCaller := session.Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	childCaller := session.Caller{Kind: "session", ID: child.session, Execution: child.execution(), Host: child.host()}
	for _, test := range []struct {
		name    string
		caller  session.Caller
		target  fixture
		allowed bool
	}{
		{"owned", rootCaller, child, true},
		{"self", rootCaller, f, false},
		{"independent", rootCaller, peer, false},
		{"parent", childCaller, f, false},
		{"sibling", childCaller, sibling, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := session.EnqueueRequest{EnvironmentID: f.env, SessionID: test.target.session, IdempotencyKey: test.name, Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}
			first, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, test.caller, req, turns[test.target.session])
			if !test.allowed {
				if !errors.Is(err, session.ErrDenied) {
					t.Fatalf("non-owned send: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			replay, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, test.caller, req, turns[test.target.session])
			if err != nil || replay != first {
				t.Fatalf("retry: %+v %v", replay, err)
			}
		})
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM turn_messages WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("message effects: %d %v", count, err)
	}
}
