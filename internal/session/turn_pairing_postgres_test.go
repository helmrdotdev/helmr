package session_test

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/session"
)

// Turn and Ask operations are addressed through their parent Session and Turn.
// A child ID paired with a different readable parent in the same Environment
// is rejected, reads report it like a missing Turn, and nothing changes for
// the child's actual owner.
func TestTurnAndAskOperationsRequireTheirParents(t *testing.T) {
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	f := newFixture(t)
	peer := f.peer(t)
	turns := map[uuid.UUID]uuid.UUID{}
	for _, target := range []fixture{f, peer} {
		admission := target.enqueue(t, "active")
		if _, err := dispatchTurnForTest(t.Context(), target.pool, target.execution()); err != nil {
			t.Fatal(err)
		}
		if err := session.RuntimeRegisterMessages(t.Context(), target.pool, *target.host(), target.execution(), admission.TurnID); err != nil {
			t.Fatal(err)
		}
		turns[target.session] = admission.TurnID
	}
	owned, peerTurn := turns[f.session], turns[peer.session]
	queued := f.enqueue(t, "queued").TurnID
	ask := uuid.NewV7()
	if err := session.RuntimeAsk(t.Context(), policy, f.pool, *f.host(), f.execution(), owned, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"continue?"}],"type":"text"}`)); err != nil {
		t.Fatal(err)
	}
	// The caller can read and control the peer Session itself, so every
	// rejection below comes from the parent pairing rather than authority.
	if _, err := session.ListTurns(t.Context(), policy, f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: peer.session, Limit: 10}); err != nil {
		t.Fatalf("peer Session is not readable: %v", err)
	}
	// The peer Turn accepts messages, so a rejected mismatched send below
	// comes from the pairing check rather than the peer's state.
	if _, err := session.SendTurn(t.Context(), policy, f.pool, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, IdempotencyKey: "peer", Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, peerTurn); err != nil {
		t.Fatalf("send to peer Turn: %v", err)
	}

	answer := func(sessionID, turn uuid.UUID) error {
		_, err := session.AnswerAsk(t.Context(), policy, f.pool, f.caller(), session.AskAnswerRequest{EnvironmentID: f.env, SessionID: sessionID, TurnID: turn, AskID: ask, AnswerID: "mismatch", Answer: json.RawMessage(`"yes"`)})
		return err
	}
	getAsk := func(sessionID, turn uuid.UUID) error {
		_, err := session.GetAsk(t.Context(), policy, f.pool, f.caller(), f.env, sessionID, turn, ask)
		return err
	}
	for _, test := range []struct {
		name string
		call func() error
		want error
	}{
		{"get missing Turn", func() error {
			_, err := session.GetTurn(t.Context(), policy, f.pool, f.caller(), f.env, f.session, uuid.NewV7())
			return err
		}, session.ErrDenied},
		{"get Turn under another Session", func() error {
			_, err := session.GetTurn(t.Context(), policy, f.pool, f.caller(), f.env, peer.session, owned)
			return err
		}, session.ErrDenied},
		{"list Asks under another Session", func() error {
			_, err := session.ListAsks(t.Context(), policy, f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: peer.session, Limit: 10}, owned)
			return err
		}, session.ErrDenied},
		{"get Ask under another Session", func() error { return getAsk(peer.session, owned) }, session.ErrDenied},
		{"get Ask under another Turn of its Session", func() error { return getAsk(f.session, queued) }, session.ErrDenied},
		{"get Ask under another Session's Turn", func() error { return getAsk(peer.session, peerTurn) }, session.ErrDenied},
		{"answer Ask under another Session", func() error { return answer(peer.session, owned) }, session.ErrDenied},
		{"answer Ask under another Turn of its Session", func() error { return answer(f.session, queued) }, session.ErrDenied},
		{"answer Ask under another Session's Turn", func() error { return answer(peer.session, peerTurn) }, session.ErrDenied},
		{"interrupt Turn under another Session", func() error {
			_, err := session.ControlSession(t.Context(), policy, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: peer.session, TurnID: owned, Kind: "turn_interrupt", IdempotencyKey: "mismatch"})
			return err
		}, session.ErrDenied},
		{"send message to Turn under another Session", func() error {
			_, err := session.SendTurn(t.Context(), policy, f.pool, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, IdempotencyKey: "mismatch", Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, owned)
			return err
		}, session.ErrMessageClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	var running, pending bool
	var messages, controls int
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT status='running' FROM turns WHERE environment_id=$1 AND id=$2),
 (SELECT status='pending' AND answer_id IS NULL FROM turn_asks WHERE environment_id=$1 AND id=$3),
 (SELECT count(*) FROM turn_messages WHERE environment_id=$1 AND (session_id<>$4 OR turn_id<>$5)),
 (SELECT count(*) FROM session_controls WHERE environment_id=$1)`, f.env, owned, ask, peer.session, peerTurn).Scan(&running, &pending, &messages, &controls); err != nil {
		t.Fatal(err)
	}
	if !running || !pending || messages != 0 || controls != 0 {
		t.Fatalf("mismatched operations had effects: running=%v pending=%v messages=%d controls=%d", running, pending, messages, controls)
	}

	// The same IDs succeed under their actual parents.
	if view, err := session.GetTurn(t.Context(), policy, f.pool, f.caller(), f.env, f.session, owned); err != nil || view.ID != owned {
		t.Fatalf("get owned Turn: %+v %v", view, err)
	}
	if page, err := session.ListAsks(t.Context(), policy, f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 10}, owned); err != nil || len(page.Asks) != 1 || page.Asks[0].ID != ask {
		t.Fatalf("list owned Asks: %+v %v", page, err)
	}
	if err := getAsk(f.session, owned); err != nil {
		t.Fatalf("get owned Ask: %v", err)
	}
	if _, err := session.SendTurn(t.Context(), policy, f.pool, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "owned", Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, owned); err != nil {
		t.Fatalf("send to owned Turn: %v", err)
	}
	if err := answer(f.session, owned); err != nil {
		t.Fatalf("answer owned Ask: %v", err)
	}
	if _, err := session.ControlSession(t.Context(), policy, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: owned, Kind: "turn_interrupt", IdempotencyKey: "owned"}); err != nil {
		t.Fatalf("interrupt owned Turn: %v", err)
	}
}
