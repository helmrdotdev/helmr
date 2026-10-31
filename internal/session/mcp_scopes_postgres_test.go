package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"github.com/helmrdotdev/helmr/internal/session"
	"sync"
	"testing"
	"time"
	"uuid"
)

func issueScope(t *testing.T, f fixture, raw string) session.Caller {
	t.Helper()
	caller := session.Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	var policy json.RawMessage
	if raw != "" {
		policy = json.RawMessage(raw)
	}
	scope, err := session.IssueMCPScope(t.Context(), f.pool, caller, policy)
	if err != nil {
		t.Fatal(err)
	}
	caller.MCPScopeID = scope.ID
	return caller
}
func TestRuntimeMCPScopeReservationConcurrencyAndQuota(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM session_mcp_scopes WHERE environment_id=$1`, f.env)
	// Finite requests resolve against the caller's pinned manifest, independently
	// of whether the maximum is symbolic unrestricted.
	raw := json.RawMessage(`{"allow":[{"operation":"spawn","agents":["agent"]}]}`)
	caller := session.Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	const workers = 12
	results := make(chan session.MCPScope, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { s, e := session.IssueMCPScope(t.Context(), f.pool, caller, raw); results <- s; errs <- e })
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var id uuid.UUID
	for s := range results {
		if id == uuid.Nil() {
			id = s.ID
		}
		if id != s.ID {
			t.Fatal("concurrent identical policy minted distinct scopes")
		}
	}
	var count, slot int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*),max(slot) FROM session_mcp_scopes WHERE environment_id=$1 AND session_id=$2`, f.env, f.session).Scan(&count, &slot); err != nil || count != 2 || slot != 1 {
		t.Fatalf("maximum reservation count=%d slot=%d err=%v", count, slot, err)
	}
	for _, op := range []string{"start", "enqueue", "send_turn", "interrupt_turn", "interrupt_session", "resume_session", "close_session", "cancel_session", "inspect_session", "inspect_turn", "list_computers"} {
		rules := `{"operation":"` + op + `","agents":["agent"]}`
		if op == "list_computers" {
			rules = `{"operation":"list_computers"}`
		}
		if _, err := session.IssueMCPScope(t.Context(), f.pool, caller, json.RawMessage(`{"allow":[`+rules+`]}`)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{"allow":[]}`, `{"allow":[{"operation":"spawn","agents":["agent"],"computer":"caller"}]}`} {
		if _, err := session.IssueMCPScope(t.Context(), f.pool, caller, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	distinctErrors := make(chan error, 2)
	for _, policy := range []string{`{"allow":[{"operation":"start","agents":["agent"],"computer":"caller"}]}`, `{"allow":[{"operation":"spawn","agents":["agent"]},{"operation":"inspect_turn","agents":["agent"]}]}`} {
		wg.Go(func() {
			_, err := session.IssueMCPScope(t.Context(), f.pool, caller, json.RawMessage(policy))
			distinctErrors <- err
		})
	}
	wg.Wait()
	close(distinctErrors)
	accepted, limited := 0, 0
	for err := range distinctErrors {
		if err == nil {
			accepted++
		} else if errors.Is(err, session.ErrRuntimeMCPScopeLimit) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || limited != 1 {
		t.Fatalf("concurrent final slot accepted=%d limited=%d", accepted, limited)
	}
	if _, err := session.IssueMCPScope(t.Context(), f.pool, caller, json.RawMessage(`{"allow":[{"operation":"spawn","agents":["agent"]},{"operation":"inspect_turn","agents":["agent"]},{"operation":"inspect_session","agents":["agent"]}]}`)); !errors.Is(err, session.ErrRuntimeMCPScopeLimit) {
		t.Fatalf("17th scope: %v", err)
	}
	if reused, err := session.IssueMCPScope(t.Context(), f.pool, caller, raw); err != nil || reused.ID != id {
		t.Fatalf("quota broke existing descriptor %v", err)
	}
}
func TestRuntimeMCPScopeCeilingAndTrustedAssociation(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM session_mcp_scopes WHERE environment_id=$1`, f.env)
	maximum, _ := mcppolicy.Normalize(json.RawMessage(`{"allow":[{"operation":"inspect_session","agents":["agent"]}]}`), []string{"agent"})
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_definitions SET runtime_mcp_policy=$3 WHERE environment_id=$1 AND agent_id=$2`, f.env, f.agent, maximum.Bytes())
	caller := issueScope(t, f, "")
	peer := f.requestedPeer(t)
	if _, err := session.RuntimeGetSession(t.Context(), f.pool, caller, peer.session); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.RuntimeListComputers(t.Context(), f.pool, caller, computer.ListPage{}); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("scope bypass: %v", err)
	}
	raw := json.RawMessage(`{"allow":[{"operation":"spawn","agents":["agent"]}]}`)
	if _, err := session.IssueMCPScope(t.Context(), f.pool, caller, raw); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("widen: %v", err)
	}
	if _, err := session.IssueMCPScope(t.Context(), f.pool, caller, json.RawMessage(`{"allow":[{"operation":"spawn","agents":["unknown"]}]}`)); !errors.Is(err, mcppolicy.ErrInvalid) {
		t.Fatalf("unknown pinned key: %v", err)
	}
	foreign := issueScope(t, peer, "")
	bad := caller
	bad.MCPScopeID = foreign.MCPScopeID
	for _, candidate := range []session.Caller{bad, func() session.Caller { c := caller; c.MCPScopeID = uuid.Nil(); return c }(), func() session.Caller { c := caller; c.Execution.ProcessEpoch++; return c }(), func() session.Caller { c := caller; c.Execution.AuthorityGeneration++; return c }()} {
		if _, err := session.RuntimeGetSession(t.Context(), f.pool, candidate, peer.session); err == nil {
			t.Fatal("invalid association or current authority accepted")
		}
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_mcp_scopes SET policy_digest=decode(repeat('00',32),'hex') WHERE environment_id=$1 AND id=$2`, f.env, caller.MCPScopeID)
	if _, err := session.RuntimeGetSession(t.Context(), f.pool, caller, peer.session); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("corrupt digest: %v", err)
	}
}

func TestRuntimeMCPScopeReceiptPlacementAndControls(t *testing.T) {
	f := newAdmissionFixture(t)
	broad := issueScope(t, f, "")
	req := f.startRequest("scoped-new")
	created, err := session.Spawn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, broad, req)
	if err != nil {
		t.Fatal(err)
	}
	callerOnly := issueScope(t, f, `{"allow":[{"operation":"spawn","agents":["agent"],"computer":"caller"}]}`)
	if _, err := session.Spawn(t.Context(), auditpolicy.Policy{}, f.pool, nil, callerOnly, req); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("originally-new receipt laundered to caller placement: %v", err)
	}
	newOnly := issueScope(t, f, `{"allow":[{"operation":"spawn","agents":["agent"]}]}`)
	retry, err := session.Spawn(t.Context(), auditpolicy.Policy{}, f.pool, nil, newOnly, req)
	if err != nil || retry.SessionID != created.SessionID || retry.TurnID != created.TurnID {
		t.Fatalf("new placement retry %+v %v", retry, err)
	}
	if _, err := session.RuntimeGetSession(t.Context(), f.pool, newOnly, created.SessionID); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("creation gave observation implicitly: %v", err)
	}
	empty := issueScope(t, f, `{"allow":[]}`)
	if _, err := session.Spawn(t.Context(), auditpolicy.Policy{}, f.pool, nil, empty, req); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("empty descriptor recovered broader receipt: %v", err)
	}
	interrupt := issueScope(t, f, `{"allow":[{"operation":"interrupt_session","agents":["agent"]}]}`)
	controlReq := session.SessionControlRequest{EnvironmentID: f.env, SessionID: created.SessionID, Kind: "interrupt", IdempotencyKey: "interrupt-scoped"}
	hold, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, interrupt, controlReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{}, f.pool, empty, controlReq); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("control receipt laundered: %v", err)
	}
	resume := issueScope(t, f, `{"allow":[{"operation":"resume_session","agents":["agent"]}]}`)
	controlReq.Kind = "resume"
	controlReq.HoldID = hold.HoldID
	controlReq.IdempotencyKey = "resume-scoped"
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, resume, controlReq); err != nil {
		t.Fatal("explicit scoped resume of exact caller hold", err)
	}
}

func TestRuntimeMCPScopeHeldDescriptorContinuity(t *testing.T) {
	f := newFixture(t)
	caller := issueScope(t, f, "")
	peer := f.requestedPeer(t)
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "interrupt", IdempotencyKey: "hold-source"}); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.session).Scan(&caller.Execution.AuthorityGeneration); err != nil {
		t.Fatal(err)
	}
	scope, err := session.IssueMCPScope(t.Context(), f.pool, caller, nil)
	if err != nil || scope.ID != caller.MCPScopeID {
		t.Fatalf("held descriptor lost continuity: %v", err)
	}
	if _, err := session.RuntimeGetSession(t.Context(), f.pool, caller, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("descriptor issuance released held effects: %v", err)
	}
}

func TestRuntimeMCPScopeReconstructionKeepsAdmittedWorkAndReceipts(t *testing.T) {
	f := newFixture(t)
	caller := issueScope(t, f, "")
	peer := f.requestedPeer(t)
	req := session.EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, IdempotencyKey: "before-reconstruction", Input: json.RawMessage(`[]`)}
	admitted, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=1`, f.env, f.session)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status,attachment_sequence,control_sequence,control_attachment,control_kind,control_generation,control_acknowledged_at) VALUES($1,$2,2,$3,1,'ready',1,1,1,'resume',2,clock_timestamp())`, f.env, f.session, f.computer)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=2 WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	fresh := caller
	fresh.Execution.ProcessEpoch = 2
	fresh.Execution.AuthorityGeneration = 2
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{}, f.pool, nil, fresh, req); !errors.Is(err, session.ErrRuntimeMCPDenied) {
		t.Fatalf("old reference authorized new process request: %v", err)
	}
	scope, err := session.IssueMCPScope(t.Context(), f.pool, fresh, nil)
	if err != nil || scope.ID == caller.MCPScopeID {
		t.Fatalf("new process scope %+v %v", scope, err)
	}
	fresh.MCPScopeID = scope.ID
	recovered, err := session.Enqueue(t.Context(), auditpolicy.Policy{}, f.pool, nil, fresh, req)
	if err != nil || recovered.TurnID != admitted.TurnID {
		t.Fatalf("fresh descriptor failed to recover original receipt %+v %v", recovered, err)
	}
	dispatched, err := dispatchTurnForTest(t.Context(), f.pool, peer.execution())
	if err != nil || dispatched.TurnID != admitted.TurnID {
		t.Fatalf("admitted work required original caller process %+v %v", dispatched, err)
	}
	var retained int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_mcp_scopes WHERE environment_id=$1 AND session_id=$2`, f.env, f.session).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("retained scope records %d %v", retained, err)
	}
}

func TestRuntimeMCPScopeIssuanceRechecksAuthorityAfterProcessWait(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM session_mcp_scopes WHERE environment_id=$1`, f.env)
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(t.Context(), `SELECT epoch FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=1 FOR UPDATE`, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	connection, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	caller := session.Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	done := make(chan error, 1)
	go func() {
		_, err := session.IssueMCPScope(t.Context(), connection, caller, json.RawMessage(`{"allow":[]}`))
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, connection.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scope issuer did not wait for process row")
		}
		time.Sleep(time.Millisecond)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("scope issued after physical authority expired during wait")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_mcp_scopes WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed issuance committed rows %d %v", count, err)
	}
}
