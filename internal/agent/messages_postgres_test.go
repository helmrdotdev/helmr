package agent

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestMessagesKeepExactDispositionAndDrain(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	req := EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "steer", Input: json.RawMessage(`[{"type":"text","text":"{\"text\":\"one\\u0000雪\"}"}]`)}
	canonicalInput, err := conversation.Input(req.Input)
	req.Input = canonicalInput
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SendTurn(t.Context(), f.pool, f.caller(), req, first.TurnID); !errors.Is(err, ErrMessageClosed) {
		t.Fatalf("unregistered: %v", err)
	}
	if err := RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	results := make(chan SendReceipt, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, err := Send(t.Context(), f.pool, f.caller(), req)
			if err != nil {
				t.Error(err)
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	var admitted SendReceipt
	for r := range results {
		if admitted.MessageID == uuid.Nil() {
			admitted = r
		}
		if r.MessageID != admitted.MessageID || r.TurnID != first.TurnID {
			t.Fatal("send retry changed disposition")
		}
	}
	req.RetryKey = "later"
	later, err := SendTurn(t.Context(), f.pool, f.caller(), req, first.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	message, err := ClaimRuntimeMessage(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, first.TurnID)
	if err != nil || message == nil || message.ID != admitted.MessageID || string(message.Input) != string(req.Input) {
		t.Fatalf("claim: %+v %v", message, err)
	}
	repeated, err := ClaimRuntimeMessage(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, first.TurnID)
	if err != nil || repeated == nil || repeated.ID != message.ID {
		t.Fatalf("claim retry: %+v %v", repeated, err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='rejected' AND rejection_reason='turn_closed' FROM turn_messages WHERE id=$1`, later.MessageID).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("unstarted message survived: %v %v", rejected, err)
	}
	if outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID, json.RawMessage(`null`), "joined"); err != nil || outcome != nil {
		t.Fatalf("started callback finalized: %s %v", outcome, err)
	}
	if err := CompleteRuntimeMessage(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, first.TurnID, message.ID, "message_rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID, json.RawMessage(`null`), "joined"); err != nil {
		t.Fatal(err)
	}
	req.RetryKey = "steer"
	retried, err := Send(t.Context(), f.pool, f.caller(), req)
	if err != nil || retried.MessageID != admitted.MessageID {
		t.Fatalf("rejected steering fell back: %+v %v", retried, err)
	}
	req.Input = json.RawMessage(`[{"type":"text","text":"false"}]`)
	if _, err := Send(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	req.RetryKey = "new"
	queued, err := Send(t.Context(), f.pool, f.caller(), req)
	if err != nil || queued.MessageID != uuid.Nil() || queued.TurnID == first.TurnID {
		t.Fatalf("new send didn't queue: %+v %v", queued, err)
	}
}
func TestMessagesCancellationAndExpiredRetry(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	req := EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "steer", Input: json.RawMessage(`[]`)}
	admitted, err := SendTurn(t.Context(), f.pool, f.caller(), req, first.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='rejected' AND rejection_reason='turn_terminated' FROM turn_messages WHERE id=$1`, admitted.MessageID).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("cancel didn't reject: %v %v", rejected, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turn_messages SET message=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, admitted.MessageID)
	repeated, err := SendTurn(t.Context(), f.pool, f.caller(), req, first.TurnID)
	if err != nil || repeated.MessageID != admitted.MessageID {
		t.Fatalf("expired retry: %+v %v", repeated, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.user)
	if _, err := SendTurn(t.Context(), f.pool, f.caller(), req, first.TurnID); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
}

func TestMessagesSteerClosingSessionWithoutEnqueueAuthority(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "close", RetryKey: "close"}); err != nil {
		t.Fatal(err)
	}
	runtime := Caller{Kind: "session", ID: f.session, TurnID: first.TurnID, Execution: f.execution(), Host: f.host()}
	for _, caller := range []Caller{f.caller()} {
		req := EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, Input: json.RawMessage(`[]`)}
		exact, err := SendTurn(t.Context(), f.pool, caller, req, first.TurnID)
		if err != nil || exact.MessageID == uuid.Nil() {
			t.Fatalf("closing exact %s: %+v %v", caller.Kind, exact, err)
		}
		routed, err := Send(t.Context(), f.pool, caller, req)
		if err != nil || routed.MessageID == uuid.Nil() {
			t.Fatalf("closing send %s: %+v %v", caller.Kind, routed, err)
		}
		if _, err := Enqueue(t.Context(), f.pool, caller, req); err == nil {
			t.Fatalf("closing enqueue allowed: %s", caller.Kind)
		}
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), first.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []Caller{f.caller(), runtime} {
		if _, err := Send(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, Input: json.RawMessage(`[]`)}); err == nil {
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
		if _, err := Dispatch(t.Context(), target.pool, target.execution()); err != nil {
			t.Fatal(err)
		}
		if err := RuntimeRegisterMessages(t.Context(), target.pool, *target.host(), target.execution(), admission.TurnID); err != nil {
			t.Fatal(err)
		}
		turns[target.session] = admission.TurnID
	}
	rootCaller := Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	childCaller := Caller{Kind: "session", ID: child.session, Execution: child.execution(), Host: child.host()}
	for _, test := range []struct {
		name    string
		caller  Caller
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
			req := EnqueueRequest{EnvironmentID: f.env, SessionID: test.target.session, RetryKey: test.name, Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}
			first, err := SendTurn(t.Context(), f.pool, test.caller, req, turns[test.target.session])
			if !test.allowed {
				if !errors.Is(err, ErrDenied) {
					t.Fatalf("non-owned send: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			replay, err := SendTurn(t.Context(), f.pool, test.caller, req, turns[test.target.session])
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
