package slack

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (f statusFixture) control(action string) controlEnvelope {
	actor := ""
	source := ""
	if action == "answer" {
		actor = "human"
		now := time.Now()
		source = fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
	}
	return controlEnvelope{Nonce: uuid.NewV7(), Action: action, Actor: actor, SourceTimestamp: source, ExpiresAt: time.Now().Add(time.Minute).Unix(), Target: controlTarget{Installation: f.installation, Thread: f.thread, Participant: f.participant, Environment: f.Environment, Session: f.Session}}
}
func (f statusFixture) controlReceipt(t *testing.T, c controlEnvelope, answer json.RawMessage) uuid.UUID {
	t.Helper()
	var form formState
	if c.Action == "answer" {
		field, err := json.Marshal(map[string]any{"type": "plain_text_input", "value": answer})
		if err != nil {
			t.Fatal(err)
		}
		form = formState{"text": {"text": field}}
	}
	payload, _ := json.Marshal(controlGesture{Control: c, Form: form})
	now := time.Now().UTC()
	if c.Action == "answer" {
		now, _ = slackMessageTime(c.SourceTimestamp)
	}
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "fixture:" + c.Nonce.String(), Actor: "human", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}
func (f statusFixture) pendingQuestion(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	admission, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "user", ID: f.User}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "ask-turn", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	execution := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	if _, err = dispatchRuntimeTurn(t.Context(), f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	ask := uuid.NewV7()
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	if err = session.RuntimeAsk(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, host, execution, admission.TurnID, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"Choose"}],"type":"text"}`)); err != nil {
		t.Fatal(err)
	}
	return admission.TurnID, ask
}

func TestSlackAnswerAllowsLinkedOperatorAndKeepsExactAskOnlyReceipt(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	turn, ask := f.pendingQuestion(t)
	f.SetPermissions(t, "agents.use", "sessions.send")
	c := f.control("answer")
	c.Target.Turn, c.Target.Ask = turn, ask
	first := f.controlReceipt(t, c, json.RawMessage(`"yes"`))
	for range 2 {
		if err := admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, first); err != nil {
			t.Fatal(err)
		}
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='accepted' AND r.operation='answer' AND r.ask_id=$2 AND r.turn_id IS NULL AND r.control_id IS NULL AND a.answered_by_user_id=$3 AND a.answer=$4 FROM slack_requests r JOIN turn_asks a ON a.environment_id=r.environment_id AND a.id=r.ask_id WHERE r.id=$1`, first, ask, f.User, []byte(`"yes"`)).Scan(&exact); err != nil || !exact {
		t.Fatalf("answer receipt mismatch: %v %v", exact, err)
	}
	c.Nonce = uuid.NewV7()
	second := f.controlReceipt(t, c, json.RawMessage(`"no"`))
	if err := admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, second); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='ask_already_answered' FROM slack_requests WHERE id=$1`, second).Scan(&exact); err != nil || !exact {
		t.Fatalf("competing answer replaced first: %v %v", exact, err)
	}
	f.SetPermissions(t, "sessions.read")
	stop := f.nativeStop(t, "viewer-stop")
	if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, stop); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='permission_denied' FROM slack_requests WHERE id=$1`, stop).Scan(&exact); err != nil || !exact {
		t.Fatalf("answer membership granted Stop authority: %v %v", exact, err)
	}
}

func TestSlackControlRejectsStaleBindingActorAndToken(t *testing.T) {
	for _, kind := range []string{"target", "actor", "expiry", "publication"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			c := f.control("answer")
			c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
			want := "target_unavailable"
			switch kind {
			case "target":
				c.Target.Thread = uuid.NewV7()
			case "actor":
				c.Actor = "human"
				c.Actor = "another-human"
				want = "control_actor_mismatch"
			case "expiry":
				c.ExpiresAt = time.Now().Add(-time.Minute).Unix()
				c.Action, c.Actor, c.SourceTimestamp, c.OptionID = "choose", "", "", "yes"
				want = "gesture_expired"
			case "publication":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
				want = "binding_unavailable"
			}
			id := f.controlReceipt(t, c, json.RawMessage(`"yes"`))
			if err := admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id); err != nil {
				t.Fatal(err)
			}
			var rejected bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error=$2 AND control_id IS NULL AND ask_id IS NULL FROM slack_requests WHERE id=$1`, id, want).Scan(&rejected); err != nil || !rejected {
				t.Fatalf("invalid control admitted: %v %v", rejected, err)
			}
		})
	}
}

func TestSlackControlRollsBackOperationWhenReceiptCannotCommit(t *testing.T) {
	for _, action := range []string{"stop", "answer"} {
		t.Run(action, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			c := f.control(action)
			if action == "answer" {
				c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
			}
			var id uuid.UUID
			if action == "stop" {
				id = f.nativeStop(t, "rollback")
			} else {
				id = f.controlReceipt(t, c, json.RawMessage(`"yes"`))
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_requests ADD CONSTRAINT fail_control_receipt CHECK(status<>'accepted')`)
			var err error
			if action == "stop" {
				err = admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id)
			} else {
				err = admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id)
			}
			if err == nil {
				t.Fatal("receipt failure missing")
			}
			var unchanged bool
			if action == "stop" {
				if err := f.Pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM session_controls WHERE environment_id=$1) AND NOT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1)`, f.Environment).Scan(&unchanged); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND answer IS NULL FROM turn_asks WHERE environment_id=$1 AND id=$2`, f.Environment, c.Target.Ask).Scan(&unchanged); err != nil {
					t.Fatal(err)
				}
			}
			if !unchanged {
				t.Fatal("operation escaped failed adapter receipt")
			}
		})
	}
}

func TestSlackStopExpiresDuringCoreLockWaitWithoutLeavingHold(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.nativeStop(t, "expires-in-lock")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err = blocker.Exec(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.Environment, f.Session); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err = blocker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id) }()
	deadline := time.Now().Add(5 * time.Second)
	var blocked bool
	for !blocked && time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if !blocked {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !blocked {
		t.Fatal("Stop did not wait on core lock")
	}
	var expired bool
	for !expired && time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT expires_at<=clock_timestamp() FROM slack_requests WHERE id=$1`, id).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if !expired {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !expired {
		t.Fatal("deadline did not elapse")
	}
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	var safe bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.status='rejected' AND r.error='gesture_expired' AND NOT EXISTS(SELECT 1 FROM session_controls WHERE environment_id=$2) AND NOT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$2) AND (SELECT authority_generation=1 FROM sessions WHERE environment_id=$2 AND id=$3) FROM slack_requests r WHERE r.id=$1`, id, f.Environment, f.Session).Scan(&safe); err != nil || !safe {
		t.Fatalf("expired Stop left effects: %v %v", safe, err)
	}
}

func TestSlackAnswerRejectsReceiptWithChangedOpeningSource(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	c := f.control("answer")
	c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
	id := f.controlReceipt(t, c, json.RawMessage(`"yes"`))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET source_occurred_at=source_occurred_at+interval '1 microsecond' WHERE id=$1`, id)
	if err := admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='control_source_mismatch' FROM slack_requests WHERE id=$1`, id).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("changed source accepted: %v %v", rejected, err)
	}
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' FROM turn_asks WHERE environment_id=$1 AND id=$2`, f.Environment, c.Target.Ask).Scan(&pending); err != nil || !pending {
		t.Fatalf("ask changed: %v %v", pending, err)
	}
}
