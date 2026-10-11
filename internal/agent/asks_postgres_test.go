package agent

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/conversation"
	"sync"
	"testing"
	"time"
	"uuid"
)

func askFixture(t *testing.T) (fixture, Admission, uuid.UUID, json.RawMessage) {
	t.Helper()
	f := newFixture(t)
	a := f.enqueue(t, "ask")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewV7()
	q := json.RawMessage(`{"prompt":[{"type":"text","text":"choose\u0000雪"}],"answer":{"type":"text"}}`)
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id, q); err != nil {
		t.Fatal(err)
	}
	return f, a, id, q
}
func TestAskDurableIdentityAndFinalization(t *testing.T) {
	f, a, id, q := askFixture(t)
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id, q); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id, json.RawMessage(`{"prompt":[],"answer":{"type":"text"}}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed question: %v", err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	if outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`null`), "joined"); err != nil || outcome != nil {
		t.Fatalf("pending question finalization did not wait: %s %v", outcome, err)
	}
	var waiting bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='running' AND result_digest IS NULL AND NOT EXISTS(SELECT 1 FROM computer_saves WHERE turn_id=$1) FROM turns WHERE id=$1`, a.TurnID).Scan(&waiting); err != nil || !waiting {
		t.Fatalf("question drainage prepared a result/save: %v %v", waiting, err)
	}
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), q); !errors.Is(err, ErrNotReady) {
		t.Fatalf("late ask: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE org_members SET role='viewer' WHERE user_id=$1`, f.user); err != nil {
		t.Fatal(err)
	}
	req := AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: "reply", Answer: json.RawMessage(`""`)}
	view, err := RespondAsk(t.Context(), f.pool, f.caller(), req)
	if err != nil || view.Status != "responded" || string(view.Answer) != `""` || view.RespondedByUserID == nil || *view.RespondedByUserID != f.user {
		t.Fatalf("viewer reply after close: %+v %v", view, err)
	}
	if err := RuntimeWithdrawAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id); err != nil {
		t.Fatal(err)
	}
	observed, err := RuntimeObserveAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id)
	if err != nil || observed.Status != "responded" {
		t.Fatalf("withdraw erased answer: %+v %v", observed, err)
	}
	if _, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`null`), "joined"); err != nil {
		t.Fatal(err)
	}
	// Receipts remain valid after finalization and content expiry.
	if _, err := f.pool.Exec(t.Context(), `UPDATE turn_asks SET question=NULL,answer=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatalf("expired reply retry: %v", err)
	}
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id, q); err != nil {
		t.Fatalf("expired question retry: %v", err)
	}
	req.Answer = json.RawMessage(`"changed"`)
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reply: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.user); err != nil {
		t.Fatal(err)
	}
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
}
func TestAskAnswerArbitrationAndCancellation(t *testing.T) {
	f, a, id, _ := askFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := RespondAsk(t.Context(), f.pool, f.caller(), AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: uuid.NewV7().String(), Answer: json.RawMessage(`"yes"`)})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	won := 0
	for err := range errs {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrAskAlreadyResponded) {
			t.Fatal(err)
		}
	}
	if won != 1 {
		t.Fatalf("winners %d", won)
	}
	pending := uuid.NewV7()
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, pending, json.RawMessage(`{"prompt":[],"answer":{"type":"text"}}`)); err != nil {
		t.Fatal(err)
	}
	req := AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: pending, ResponseID: "invalid", Answer: json.RawMessage(`null`)}
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); !errors.Is(err, conversation.ErrAnswerInvalid) {
		t.Fatal(err)
	}
	if err := RuntimeWithdrawAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, pending); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeWithdrawAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, pending); err != nil {
		t.Fatal(err)
	}
	req.Answer = json.RawMessage(`"yes"`)
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrAskCancelled) {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE kind='ask.cancelled'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cancel events %d %v", count, err)
	}
}
func TestAskTerminalCancellationAndBudget(t *testing.T) {
	f, a, id, q := askFixture(t)
	if _, err := f.pool.Exec(t.Context(), `UPDATE turns SET progress_bytes=8388608 WHERE id=$1`, a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id, q); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), q); !errors.Is(err, conversation.ErrLimit) {
		t.Fatalf("budget %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, a.TurnID); err != nil {
		t.Fatal(err)
	}
	req := AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: "late", Answer: json.RawMessage(`""`)}
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrNotReady) {
		t.Fatalf("deadline answer %v", err)
	}
	if err := expireSessionDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	view, err := GetAsk(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID, id)
	if err != nil || view.Status != "cancelled" {
		t.Fatalf("terminal ask %+v %v", view, err)
	}
	page, err := ListAsks(t.Context(), f.pool, f.caller(), TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 1}, a.TurnID)
	if err != nil || len(page.Asks) != 1 {
		t.Fatalf("page %+v %v", page, err)
	}
	if _, err := GetAsk(t.Context(), f.pool, f.caller(), f.env, uuid.NewV7(), a.TurnID, id); !errors.Is(err, ErrDenied) {
		t.Fatalf("containment %v", err)
	}
}

func askKey(t *testing.T, f fixture, role string, permissions []string) Caller {
	t.Helper()
	id := uuid.NewV7()
	_, err := f.pool.Exec(t.Context(), `INSERT INTO api_keys(id,org_id,project_id,environment_id,role,permissions,name,key_prefix,token_hash) SELECT $2,org_id,project_id,id,$3,$4,$2::uuid::text,'test',$5 FROM environments WHERE id=$1`, f.env, id, role, permissions, id[:])
	if err != nil {
		t.Fatal(err)
	}
	return Caller{Kind: "api_key", ID: id}
}
func TestAskKeyAuthorityAndAttribution(t *testing.T) {
	f, a, id, _ := askFixture(t)
	reader := askKey(t, f, "admin", []string{"sessions.read"})
	answerer := askKey(t, f, "admin", []string{"asks.respond"})
	developer := askKey(t, f, "developer", []string{"asks.respond"})
	cross := askKey(t, f, "admin", []string{"asks.respond"})
	otherEnv := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) SELECT 'until_environment_deletion',$2,org_id,project_id,'other','Other','#123456' FROM environments WHERE id=$1`, f.env, otherEnv); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE api_keys SET environment_id=$2 WHERE id=$1`, cross.ID, otherEnv); err != nil {
		t.Fatal(err)
	}

	req := AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: "response", Answer: json.RawMessage(`""`)}
	for _, caller := range []Caller{reader, developer, cross, {Kind: "session", ID: f.session}} {
		if _, err := RespondAsk(t.Context(), f.pool, caller, req); !errors.Is(err, ErrDenied) {
			t.Fatalf("ungranted answer %+v: %v", caller, err)
		}
	}
	if _, err := GetAsk(t.Context(), f.pool, answerer, f.env, f.session, a.TurnID, id); !errors.Is(err, ErrDenied) {
		t.Fatalf("answer implies read: %v", err)
	}
	if _, err := GetAsk(t.Context(), f.pool, reader, f.env, f.session, a.TurnID, id); err != nil {
		t.Fatal(err)
	}
	view, err := RespondAsk(t.Context(), f.pool, answerer, req)
	if err != nil || view.RespondedByAPIKeyID == nil || *view.RespondedByAPIKeyID != answerer.ID || view.RespondedByUserID != nil {
		t.Fatalf("key attribution %+v %v", view, err)
	}
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("responder identity omitted from retry: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, answerer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := RespondAsk(t.Context(), f.pool, answerer, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked key retry: %v", err)
	}
}
func TestAskKeyExpiresWhileWaitingForSession(t *testing.T) {
	f, a, id, _ := askFixture(t)
	key := askKey(t, f, "admin", []string{"asks.respond"})
	var expires time.Time
	if err := f.pool.QueryRow(t.Context(), `UPDATE api_keys SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1 RETURNING expires_at`, key.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := RespondAsk(t.Context(), f.pool, key, AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: "delayed", Answer: json.RawMessage(`""`)})
		done <- err
	}()
	// Confirm the operation passed its first authorization and is waiting on the
	// Session owner, then release only after database time passes the key expiry.
	for {
		var blocked bool
		if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM sessions%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(expires) {
			t.Fatal("answer did not reach Session lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(max(time.Until(expires.Add(25*time.Millisecond)), 0))
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrDenied) {
		t.Fatalf("expired key crossed owner wait: %v", err)
	}
	view, err := GetAsk(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID, id)
	if err != nil || view.Status != "pending" {
		t.Fatalf("expired answer changed state: %+v %v", view, err)
	}
}

func TestChildCannotAskHuman(t *testing.T) {
	root := newFixture(t)
	child := ownedFixture(t, root)
	turn := child.enqueue(t, "child")
	if _, err := Dispatch(t.Context(), child.pool, child.execution()); err != nil {
		t.Fatal(err)
	}
	question := json.RawMessage(`{"prompt":[{"type":"text","text":"Approve?"}],"answer":{"type":"text"}}`)
	if err := RuntimeAsk(t.Context(), child.pool, *child.host(), child.execution(), turn.TurnID, uuid.NewV7(), question); !errors.Is(err, ErrRootSessionRequired) {
		t.Fatalf("child human question: %v", err)
	}
	var unchanged bool
	if err := child.pool.QueryRow(t.Context(), `SELECT progress_bytes=0 AND NOT EXISTS(SELECT 1 FROM turn_asks WHERE turn_id=$1) AND NOT EXISTS(SELECT 1 FROM session_events WHERE turn_id=$1 AND kind='ask.created') FROM turns WHERE id=$1`, turn.TurnID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("child question effects: %v %v", unchanged, err)
	}
}
