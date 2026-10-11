package agent

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func controlRequest(f fixture, kind, key string) SessionControlRequest {
	return SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: kind, RetryKey: key}
}

func ownedFixture(t *testing.T, f fixture) fixture {
	t.Helper()
	p := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$1,root_session_id=$1,requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, p.session)
	return p
}

func TestSessionInterruptSettlesOwnedTreePreservesQueueAndPeer(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	active, _ := child.finalize(t, "active")
	queued := child.enqueue(t, "queued")
	peerActive := peer.enqueue(t, "peer")
	if _, err := Dispatch(t.Context(), f.pool, peer.execution()); err != nil {
		t.Fatal(err)
	}
	r, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "stop"))
	if err != nil || r.HoldID == uuid.Nil() {
		t.Fatalf("interrupt: %+v %v", r, err)
	}
	for id, want := range map[uuid.UUID]string{active.TurnID: "interrupted", queued.TurnID: "queued", peerActive.TurnID: "running"} {
		var status string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&status); err != nil || status != want {
			t.Fatalf("turn %s: %s, want %s: %v", id, status, want, err)
		}
	}
	for id, want := range map[uuid.UUID]string{f.session: "ready", child.session: "stopping", peer.session: "ready"} {
		var status string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE session_id=$1`, id).Scan(&status); err != nil || status != want {
			t.Fatalf("process %s: %s, want %s: %v", id, status, want, err)
		}
	}
	// A pending save is not allowed to resurrect a terminal interrupted result.
	if err := Complete(t.Context(), f.pool, f.env, child.session, active.TurnID); !errors.Is(err, ErrTerminal) {
		t.Fatalf("late completion: %v", err)
	}
}

func TestSessionControlRetryAdditiveHoldAndExactResume(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	req := controlRequest(f, "interrupt", "same")
	const n = 8
	results := make(chan SessionControlReceipt, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			r, err := ControlSession(t.Context(), f.pool, f.caller(), req)
			results <- r
			errs <- err
		})
	}
	wg.Wait()
	first := <-results
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for range n - 1 {
		if r := <-results; r != first {
			t.Fatalf("retry differs: %+v / %+v", first, r)
		}
	}
	second, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(child, "interrupt", "child"))
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(f, "resume", "release")
	resume.HoldID = first.HoldID
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	if r, err := ControlSession(t.Context(), f.pool, f.caller(), req); err != nil || r != first {
		t.Fatalf("released retry: %+v %v", r, err)
	}
	req.Reason = "different"
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed interruption: %v", err)
	}
	resume.HoldID = second.HoldID
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), resume); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed resume: %v", err)
	}
	var active int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("remaining holds %d: %v", active, err)
	}
	// The overlapping child hold still fences dispatch after ancestor release.
	child.enqueue(t, "queued")
	e := child.execution()
	e.AuthorityGeneration = 4
	if _, err := Dispatch(t.Context(), f.pool, e); !errors.Is(err, ErrNotReady) {
		t.Fatalf("child hold dispatch rejection: %v", err)
	}
}

func TestSessionRuntimeControlsCannotEscapeOwnershipOrReleaseExternalHolds(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	caller := Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	for _, target := range []fixture{f, peer} {
		if _, err := ControlSession(t.Context(), f.pool, caller, controlRequest(target, "cancel", "foreign")); !errors.Is(err, ErrDenied) {
			t.Fatalf("non-owned control: %v", err)
		}
	}
	r, err := ControlSession(t.Context(), f.pool, caller, controlRequest(child, "interrupt", "programmatic"))
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(child, "resume", "programmatic-release")
	resume.HoldID = r.HoldID
	if _, err := ControlSession(t.Context(), f.pool, caller, resume); err != nil {
		t.Fatal(err)
	}
	for _, issuer := range []string{"user", "system"} {
		hold := uuid.NewV7()
		var issuerID any
		if issuer == "user" {
			issuerID = f.user
		}
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,$2,$3,'local','requires external recovery',$4,$5)`, f.env, hold, child.session, issuer, issuerID)
		resume.RetryKey, resume.HoldID = issuer, hold
		if _, err := ControlSession(t.Context(), f.pool, caller, resume); !errors.Is(err, ErrDenied) {
			t.Fatalf("runtime released %s hold: %v", issuer, err)
		}
		if _, err := ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
			t.Fatalf("authorized human release: %v", err)
		}
	}
}

func TestSessionCancelSettlesQueueAndStopsOnlyOwnedProcesses(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	a := child.enqueue(t, "queued")
	closeReceipt, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatal(err)
	}
	if r, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil || r != closeReceipt {
		t.Fatalf("close receipt changed after escalation: %+v %v", r, err)
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, a.TurnID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("queued turn: %s %v", status, err)
	}
	for _, target := range []fixture{f, child, peer} {
		want := "cancelled"
		if target.session == peer.session {
			want = "open"
		}
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, target.session).Scan(&status); err != nil || status != want {
			t.Fatalf("session %s: %s %v", target.session, status, err)
		}
		want = "stopping"
		if target.session == peer.session {
			want = "ready"
		}
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE session_id=$1`, target.session).Scan(&status); err != nil || status != want {
			t.Fatalf("process %s: %s %v", target.session, status, err)
		}
	}
}

func TestSessionInterruptedSaveReconcilesWithoutBlockingPeerOrChangingOutcome(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	active, save := f.finalize(t, "active")
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "interrupt")); err != nil {
		t.Fatal(err)
	}
	// Stop delivery still needs a renewed envelope and replacement attachment.
	authority, err := RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil || authority.Generation != 2 {
		t.Fatalf("stopping control authority: %+v %v", authority, err)
	}
	if _, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution()); err != nil {
		t.Fatalf("stopping attachment: %v", err)
	}
	e := f.execution()
	e.AuthorityGeneration = 2
	if _, err := Dispatch(t.Context(), f.pool, e); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopping process received business execution: %v", err)
	}
	f.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, active.TurnID); !errors.Is(err, ErrTerminal) {
		t.Fatalf("interrupted Turn resurrected: %v", err)
	}
	peerTurn, peerSave := peer.finalize(t, "peer")
	peerCut, peerRoot := storage.cut(t, 3)
	f.capture(t, peerSave, peerRoot)
	if err := storage.publish(t, peerSave.ID, peerCut); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, peer.session, peerTurn.TurnID); err != nil {
		t.Fatalf("peer blocked by interrupted save: %v", err)
	}
}

func TestSessionControlRejectsInvalidStoredText(t *testing.T) {
	f := newFixture(t)
	for _, reason := range []string{strings.Repeat("x", 4097), "bad\x00reason", string([]byte{0xff})} {
		req := controlRequest(f, "interrupt", "key")
		req.Reason = reason
		if _, err := ControlSession(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid reason: %v", err)
		}
	}
}

func TestSessionCloseDrainsParentBeforeSealingChildren(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	parentTurn := f.enqueue(t, "parent")
	childTurn := child.enqueue(t, "child")
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil {
		t.Fatal(err)
	}
	var lifecycle string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, child.session).Scan(&lifecycle); err != nil || lifecycle != "open" {
		t.Fatalf("child sealed before parent drained: %s %v", lifecycle, err)
	}
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatalf("accepted closing work could not dispatch: %v", err)
	}
	// Model settlement without requiring the independent storage-publication
	// fixture. Closure must consume the same durable terminal state.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp() WHERE id=$1`, parentTurn.TurnID)
	if err := ReconcileSessionClosure(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, child.session).Scan(&lifecycle); err != nil || lifecycle != "closing" {
		t.Fatalf("child not sealed: %s %v", lifecycle, err)
	}
	if _, err := Dispatch(t.Context(), f.pool, child.execution()); err != nil {
		t.Fatalf("accepted child work could not dispatch: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp() WHERE id=$1`, childTurn.TurnID)
	if err := ReconcileSessionClosure(t.Context(), f.pool, f.env, child.session); err != nil {
		t.Fatal(err)
	}
	var closed int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE status='closed'`).Scan(&closed); err != nil || closed != 2 {
		t.Fatalf("closed Sessions %d: %v", closed, err)
	}
}

func TestSessionControlAPIKeyRequiresExactPermissionAndHumanAttribution(t *testing.T) {
	f := newFixture(t)
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,role,permissions,name,key_prefix,token_hash)
 SELECT $1,org_id,project_id,id,'developer',ARRAY['sessions.send'],'test','test',$2 FROM environments WHERE id=$3`, key, []byte(key.String()), f.env)
	caller := Caller{Kind: "api_key", ID: key}
	req := controlRequest(f, "interrupt", "interrupt")
	if _, err := ControlSession(t.Context(), f.pool, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("input permission authorized interrupt: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET permissions=ARRAY['sessions.interrupt','sessions.resume'] WHERE id=$1`, key)
	r, err := ControlSession(t.Context(), f.pool, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(f, "resume", "resume")
	resume.HoldID = r.HoldID
	if _, err := ControlSession(t.Context(), f.pool, caller, resume); err != nil {
		t.Fatal(err)
	}
	human, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "human"))
	if err != nil {
		t.Fatal(err)
	}
	resume.RetryKey, resume.HoldID = "human", human.HoldID
	if _, err := ControlSession(t.Context(), f.pool, caller, resume); !errors.Is(err, ErrDenied) {
		t.Fatalf("key released human hold without attribution: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, key)
	if _, err := ControlSession(t.Context(), f.pool, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked key read historical acceptance: %v", err)
	}
}

func TestSessionLocalHoldReleaseDoesNotInvalidateHealthyChild(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	hold := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','setup failed')`, f.env, hold, f.session)
	resume := controlRequest(f, "resume", "release")
	resume.HoldID = hold
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, child.session).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("local recovery invalidated healthy child: %d %v", generation, err)
	}
	resume.RetryKey = "new-noop"
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.session).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("noop release changed authority: %d %v", generation, err)
	}
}
