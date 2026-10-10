package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool                                                           *pgxpool.Pool
	env, session, computer, user, worker, agent, deployment, group uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := agenttest.New(t)
	return fixture{pool: f.Pool, env: f.Environment, session: f.Session, computer: f.Computer, user: f.User, worker: f.Worker, agent: f.Agent, deployment: f.Deployment, group: f.Group}
}
func (f fixture) caller() Caller { return Caller{Kind: "user", ID: f.user} }
func (f fixture) execution() Execution {
	return Execution{EnvironmentID: f.env, SessionID: f.session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
}
func (f fixture) enqueue(t *testing.T, key string) Admission {
	t.Helper()
	a, err := Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: key, Input: json.RawMessage(`[{"type":"text","text":"{\"input\":1}"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func (f fixture) finalize(t *testing.T, key string) (Admission, SaveRequest) {
	t.Helper()
	a := f.enqueue(t, key)
	id, err := Dispatch(t.Context(), f.pool, f.execution())
	if err != nil || id.TurnID != a.TurnID {
		t.Fatalf("dispatch %v %v", id, err)
	}
	if err = CloseProcessing(t.Context(), f.pool, f.execution(), id.TurnID); err != nil {
		t.Fatal(err)
	}
	s, err := RecordResult(t.Context(), f.pool, f.execution(), id.TurnID, json.RawMessage(`{"result":1}`), "native callbacks and operations joined")
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}
func (f fixture) capture(t *testing.T, s SaveRequest, root string) {
	t.Helper()

	if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: s.ID, LeaseEpoch: s.LeaseEpoch, DiskRoot: root, Evidence: "verified flush and ordered cut receipt"}); err != nil {
		t.Fatal(err)
	}
}
func (f fixture) peer(t *testing.T) fixture {
	t.Helper()
	p := f
	p.session = uuid.New()
	_, err := f.pool.Exec(t.Context(), `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) VALUES('until_environment_deletion',$1,$2,$3,$4,$5,$2,0);
      INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,1,$5,1,'ready');`, pgx.QueryExecModeSimpleProtocol, f.env, p.session, f.agent, f.deployment, f.computer)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnqueueConcurrentRetryAndRevocation(t *testing.T) {
	f := newFixture(t)
	const n = 12
	results := make(chan Admission, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			a, err := Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "same", Input: json.RawMessage(`[{"type":"text","text":"{\"input\":1}"}]`)})
			results <- a
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id uuid.UUID
	for a := range results {
		if id == uuid.Nil() {
			id = a.TurnID
		}
		if a.TurnID != id || a.Sequence != 1 {
			t.Fatalf("duplicate receipt: %+v", a)
		}
	}
	_, err := f.pool.Exec(t.Context(), `UPDATE sessions SET status='closed' WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	if err != nil {
		t.Fatal(err)
	}
	a := f.enqueue(t, "same")
	if a.TurnID != id {
		t.Fatal("retry changed after close")
	}
	_, err = Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "same", Input: json.RawMessage(`[{"type":"text","text":"{\"input\":2}"}]`)})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	_, err = f.pool.Exec(t.Context(), `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.user)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "same", Input: json.RawMessage(`[{"type":"text","text":"{\"input\":1}"}]`)})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
	var turns, events int
	if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM turns),(SELECT count(*) FROM session_events)`).Scan(&turns, &events); err != nil {
		t.Fatal(err)
	}
	if turns != 1 || events != 1 {
		t.Fatalf("turns=%d events=%d", turns, events)
	}
}

func TestDispatchSerializesFIFO(t *testing.T) {
	f := newFixture(t)
	first := f.enqueue(t, "first")
	f.enqueue(t, "second")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	ids := make(chan TurnDispatch, 2)
	for range 2 {
		wg.Go(func() { id, err := Dispatch(t.Context(), f.pool, f.execution()); ids <- id; errs <- err })
	}
	wg.Wait()
	close(errs)
	close(ids)
	successes, blocked := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrNotReady) {
			blocked++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 2 || blocked != 0 {
		t.Fatalf("success=%d blocked=%d", successes, blocked)
	}
	for id := range ids {
		if id.TurnID != first.TurnID || id.Status != "running" {
			t.Fatal("dispatched later turn")
		}
	}
}

func TestCompletionRequiresOwnSaveAndMonotonicHead(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	first, save1 := f.finalize(t, "first")
	second, save2 := peer.finalize(t, "second")
	storage := newSaveStorageFixture(t, f)
	cut1, root1 := storage.cut(t, 2)
	cut2, root2 := storage.cut(t, 3)
	err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: save2.ID, LeaseEpoch: 1, DiskRoot: root2, Evidence: "verified flush and ordered cut receipt"})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("out of order capture: %v", err)
	}
	f.capture(t, save1, root1)
	f.capture(t, save2, root2)
	if err = storage.publish(t, save2.ID, cut2); err != nil {
		t.Fatal(err)
	}
	if err = Complete(t.Context(), f.pool, f.env, f.session, first.TurnID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wrong save completed first: %v", err)
	}
	if err = Complete(t.Context(), f.pool, f.env, peer.session, second.TurnID); err != nil {
		t.Fatal(err)
	}
	if err = storage.publish(t, save1.ID, cut1); err != nil {
		t.Fatal(err)
	}
	// Loss of the original writer does not erase already verified publication.
	_, err = f.pool.Exec(t.Context(), `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='VMM exited' WHERE environment_id=$1 AND computer_id=$2; UPDATE session_processes SET status='lost',fenced_at=clock_timestamp() WHERE environment_id=$1; INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$3,$4,'local','process lost');`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, uuid.New(), f.session)
	if err != nil {
		t.Fatal(err)
	}
	if err = Complete(t.Context(), f.pool, f.env, f.session, first.TurnID); err != nil {
		t.Fatal(err)
	}
	var head uuid.UUID
	var holds int
	if err = f.pool.QueryRow(t.Context(), `SELECT recovery_save_id,(SELECT count(*) FROM session_holds WHERE released_at IS NULL) FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer).Scan(&head, &holds); err != nil {
		t.Fatal(err)
	}
	if head != save2.ID || holds != 1 {
		t.Fatalf("head %v holds %d", head, holds)
	}
}

func TestIntegrityFaultBlocksCompletion(t *testing.T) {
	f := newFixture(t)
	a, s := f.finalize(t, "result")
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 4)
	f.capture(t, s, root)
	if err := storage.publish(t, s.ID, cut); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='writeback failure' WHERE environment_id=$1 AND id=$2`, f.env, f.computer); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("integrity result: %v", err)
	}
}

func TestCompletionRechecksDeadlineAfterOwnerLock(t *testing.T) {
	f := newFixture(t)
	a, s := f.finalize(t, "result")
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 5)
	f.capture(t, s, root)
	if err := storage.publish(t, s.ID, cut); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.env, f.computer); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- Complete(t.Context(), f.pool, f.env, f.session, a.TurnID) }()
	// Wait for a database lock waiter, rather than relying on goroutine scheduling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completion did not wait for owner lock")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = tx.Exec(t.Context(), `UPDATE turns SET deadline_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrTerminal) {
		t.Fatalf("deadline result: %v", err)
	}
}

func TestRuntimePeerAdmissionRequiresCurrentAuthority(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution()}
	req := EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "peer", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)}
	first, err := Enqueue(t.Context(), f.pool, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	stale := caller
	stale.Execution.LeaseEpoch = 2
	if _, err := Enqueue(t.Context(), f.pool, stale, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale ownership retry: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','operator interrupt')`, f.env, uuid.New(), f.session); err != nil {
		t.Fatal(err)
	}
	if receipt, err := Enqueue(t.Context(), f.pool, caller, req); err != nil || receipt.TurnID != first.TurnID {
		t.Fatalf("held receipt reconciliation: %+v %v", receipt, err)
	}
	req.RetryKey = "fresh-while-held"
	if _, err := Enqueue(t.Context(), f.pool, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("new held admission: %v", err)
	}
}

func TestDirectTurnAuthorityNeverBorrowsAnotherTurn(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	a := f.enqueue(t, "active")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution(), TurnID: a.TurnID}
	req := EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "peer", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)}
	if _, err := Enqueue(t.Context(), f.pool, caller, req); err != nil {
		t.Fatal(err)
	}
	if err := CloseProcessing(t.Context(), f.pool, f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	req.RetryKey = "after-return"
	if _, err := Enqueue(t.Context(), f.pool, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("closed direct authority: %v", err)
	}
	caller.TurnID = uuid.Nil()
	if _, err := Enqueue(t.Context(), f.pool, caller, req); err != nil {
		t.Fatalf("session-bound authority incorrectly borrowed active turn: %v", err)
	}
}

func TestDirectTurnAdmissionRechecksDeadlineAfterOwnerLock(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	a := f.enqueue(t, "active")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution(), TurnID: a.TurnID}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "deadline", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admission did not wait for owner lock")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = tx.Exec(t.Context(), `UPDATE turns SET deadline_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrDenied) {
		t.Fatalf("expired direct admission: %v", err)
	}
}

func TestOldTurnCannotBorrowReplacementProcess(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	a := f.enqueue(t, "active")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	_, err := f.pool.Exec(t.Context(), `UPDATE session_processes SET status='lost',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2;
      INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,2,$3,1,'ready');`, pgx.QueryExecModeSimpleProtocol, f.env, f.session, f.computer)
	if err != nil {
		t.Fatal(err)
	}
	execution := f.execution()
	execution.ProcessEpoch = 2
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: execution, TurnID: a.TurnID}
	_, err = Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "stale-turn", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("old Turn under replacement process: %v", err)
	}
}

func TestResultReceiptAndGenerationRevocation(t *testing.T) {
	f := newFixture(t)
	a, s := f.finalize(t, "active")
	again, err := RecordResult(t.Context(), f.pool, f.execution(), a.TurnID, json.RawMessage(`{"result":1}`), "verified drain")
	if err != nil || again.ID != s.ID {
		t.Fatalf("save receipt %v %v", again, err)
	}
	conflict, err := RecordResult(t.Context(), f.pool, f.execution(), a.TurnID, json.RawMessage(`{"result":2}`), "verified drain")
	if !errors.Is(err, ErrConflict) || conflict.ID != uuid.Nil() {
		t.Fatalf("save conflict %v %v", conflict, err)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE sessions SET authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=$2`, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if _, err = Dispatch(t.Context(), f.pool, f.execution()); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked dispatch: %v", err)
	}
	if _, err = RecordResult(t.Context(), f.pool, f.execution(), a.TurnID, json.RawMessage(`{"result":1}`), "verified drain"); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked result: %v", err)
	}
}

func TestSaveRequiresRecordedResult(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "queued")
	_, err := f.pool.Exec(t.Context(), `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id) VALUES($1,$2,$3,1,1,$4)`, f.env, uuid.New(), f.computer, a.TurnID)
	if err == nil {
		t.Fatal("save accepted before result boundary")
	}
	_, err = RecordResult(t.Context(), f.pool, f.execution(), a.TurnID, json.RawMessage(`1`), "drained")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("queued result: %v", err)
	}
}

func TestRuntimePeerInputQueuesWithoutReleasingTargetHold(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t) // Independent target with no origin relationship to caller.
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','human interrupt')`, f.env, uuid.New(), peer.session); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution()}
	req := EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "held-target", Input: json.RawMessage(`[{"type":"text","text":"follow-up"}]`)}
	admitted, err := Enqueue(t.Context(), f.pool, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Dispatch(t.Context(), f.pool, peer.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("held target dispatch: %v", err)
	}
	var status, kind string
	var callerID uuid.UUID
	var held bool
	if err = f.pool.QueryRow(t.Context(), `SELECT status,caller_kind,caller_id,EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND released_at IS NULL) FROM turns WHERE environment_id=$1 AND id=$3`, f.env, peer.session, admitted.TurnID).Scan(&status, &kind, &callerID, &held); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || !held || kind != "session" || callerID != f.session {
		t.Fatalf("admission changed hold or attribution: %s %t %s %v", status, held, kind, callerID)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE sessions SET status='closing' WHERE environment_id=$1 AND id=$2`, f.env, peer.session); err != nil {
		t.Fatal(err)
	}
	req.RetryKey = "after-close"
	if _, err = Enqueue(t.Context(), f.pool, caller, req); !errors.Is(err, ErrNotReady) {
		t.Fatalf("closing target admission: %v", err)
	}
}

func TestRuntimeInputCannotCrossEnvironment(t *testing.T) {
	f := newFixture(t)
	otherEnv, otherSession := uuid.New(), uuid.New()
	_, err := f.pool.Exec(t.Context(), `
	INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) SELECT 'until_environment_deletion',$2,org_id,project_id,'other','Other','#112233' FROM environments WHERE id=$1;
	INSERT INTO deployments(environment_id,id,bundle_digest) SELECT $2,id,bundle_digest FROM deployments WHERE environment_id=$1;
	INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) SELECT $2,id,spec_digest,spec,seed FROM computer_preparation_specs WHERE environment_id=$1;
	INSERT INTO computer_definitions(environment_id,deployment_id,definition_key,preparation_spec_id,resources) SELECT $2,deployment_id,definition_key,preparation_spec_id,resources FROM computer_definitions WHERE environment_id=$1;
	INSERT INTO agents(environment_id,id,name) SELECT $2,id,name FROM agents WHERE environment_id=$1;
	INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers) SELECT $2,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers FROM agent_definitions WHERE environment_id=$1;
	INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) SELECT $2,id,$5,clock_timestamp()+interval '5 minutes' FROM computers WHERE environment_id=$1;
	INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) SELECT 'until_environment_deletion',$2,$3,agent_id,deployment_id,computer_id,$3,0 FROM sessions WHERE environment_id=$1 AND id=$4;`, pgx.QueryExecModeSimpleProtocol, f.env, otherEnv, otherSession, f.session, f.deployment)
	if err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution()}
	for _, env := range []uuid.UUID{f.env, otherEnv} {
		got, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: env, SessionID: otherSession, Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
		if !errors.Is(err, ErrDenied) || got.TurnID != uuid.Nil() {
			t.Fatalf("cross-environment admission: %v %v", got, err)
		}
	}
}

func (f fixture) host() *workergroup.HostPrincipal {
	return &workergroup.HostPrincipal{HostID: f.worker, GroupID: f.group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
}

func TestRuntimeAdmissionRechecksLeaseAfterLockWait(t *testing.T) {
	f := newFixture(t)
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(t.Context(), "SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE", f.env); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution()}
	done := make(chan error, 1)
	go func() {
		_, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "expired-while-waiting", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		err = f.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%max_outstanding_admissions%')").Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admission did not wait on Environment lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = f.pool.Exec(t.Context(), "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1", f.computer); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrDenied) {
		t.Fatalf("expired operation admitted: %v", err)
	}
}

func TestRuntimeBlockedAdmissionDoesNotBlockAuthorityRenewal(t *testing.T) {
	f := newFixture(t)
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(t.Context(), "SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE", f.env); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Enqueue(t.Context(), f.pool, Caller{Kind: "session", ID: f.session, Host: f.host(), Execution: f.execution()}, EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "blocked", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err = f.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%max_outstanding_admissions%')").Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admission did not block")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = RenewRuntimeAuthority(ctx, f.pool, *f.host(), f.execution()); err != nil {
		t.Fatalf("unrelated renewal blocked behind admission: %v", err)
	}
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
