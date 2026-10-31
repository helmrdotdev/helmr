package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"slices"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func controlRequest(f fixture, kind, key string) session.SessionControlRequest {
	return session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: kind, IdempotencyKey: key}
}

func ownedFixture(t *testing.T, f fixture) fixture {
	t.Helper()
	p := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$1,root_session_id=$1,requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, p.session)
	return p
}

func TestSessionInterruptPreservesFinalizingTurnQueueAndPeer(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	active, _ := child.finalize(t, "active")
	queued := child.enqueue(t, "queued")
	peerActive := peer.enqueue(t, "peer")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, peer.execution()); err != nil {
		t.Fatal(err)
	}
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "stop"))
	if err != nil || r.HoldID == uuid.Nil() {
		t.Fatalf("interrupt: %+v %v", r, err)
	}
	for id, want := range map[uuid.UUID]string{active.TurnID: "completed", queued.TurnID: "queued", peerActive.TurnID: "running"} {
		var status string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&status); err != nil || status != want {
			t.Fatalf("turn %s: %s, want %s: %v", id, status, want, err)
		}
	}
	for id, want := range map[uuid.UUID]string{f.session: "ready", child.session: "ready", peer.session: "ready"} {
		var status string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE session_id=$1`, id).Scan(&status); err != nil || status != want {
			t.Fatalf("process %s: %s, want %s: %v", id, status, want, err)
		}
	}
	// A later control cannot retract an already completed result.
	if err := completedTurnForTest(t.Context(), f.pool, f.env, child.session, active.TurnID); err != nil {
		t.Fatalf("premature completion: %v", err)
	}
}

func TestSessionControlRetryAdditiveHoldAndExactResume(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	req := controlRequest(f, "interrupt", "same")
	const n = 8
	results := make(chan session.SessionControlReceipt, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
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
	second, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(child, "interrupt", "child"))
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(f, "resume", "release")
	resume.HoldID = first.HoldID
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	if r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req); err != nil || r != first {
		t.Fatalf("released retry: %+v %v", r, err)
	}
	req.InterruptQueued = true
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("changed interruption: %v", err)
	}
	resume.HoldID = second.HoldID
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); !errors.Is(err, session.ErrConflict) {
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
	if _, err := dispatchTurnForTest(t.Context(), f.pool, e); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("child hold dispatch rejection: %v", err)
	}
}

func TestSessionRuntimeControlsCannotEscapeOwnershipOrReleaseExternalHolds(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	caller := session.Caller{Kind: "session", MCPScopeID: f.mcpScope, ID: f.session, Execution: f.execution(), Host: f.host()}
	for _, target := range []fixture{f, peer} {
		if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, controlRequest(target, "cancel", "foreign")); !errors.Is(err, session.ErrDenied) {
			t.Fatalf("non-owned control: %v", err)
		}
	}
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, controlRequest(child, "interrupt", "programmatic"))
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(child, "resume", "programmatic-release")
	resume.HoldID = r.HoldID
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, resume); err != nil {
		t.Fatal(err)
	}
	// A parent releases a system hold on its child, never an interrupt a person
	// or API key placed there.
	for _, issuer := range []string{"user", "api_key", "system"} {
		hold := uuid.NewV7()
		kind, issuerID := "interrupt", any(uuid.NewV7())
		if issuer == "system" {
			kind, issuerID = "process_failed", nil
		}
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,issuer_id) VALUES($1,$2,$3,$4,$5,$6)`, f.env, hold, child.session, kind, issuer, issuerID)
		resume.IdempotencyKey, resume.HoldID = issuer, hold
		_, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, resume)
		if issuer == "system" {
			if err != nil {
				t.Fatalf("parent could not release child's process failure hold: %v", err)
			}
			continue
		}
		if !errors.Is(err, session.ErrDenied) {
			t.Fatalf("runtime released %s interrupt: %v", issuer, err)
		}
		if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
			t.Fatalf("authorized human release: %v", err)
		}
	}
}

func TestSessionCancelSettlesQueueAndStopsOnlyOwnedProcesses(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	peer := f.peer(t)
	a := child.enqueue(t, "queued")
	closeReceipt, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatal(err)
	}
	if closeReceipt.Status != "closing" || closeReceipt.ID == uuid.Nil() {
		t.Fatalf("close receipt: %+v", closeReceipt)
	}
	if r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil || r.ID != closeReceipt.ID || r.Status != "closed" {
		t.Fatalf("close retry after escalation: %+v %v", r, err)
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, a.TurnID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("queued turn: %s %v", status, err)
	}
	for _, target := range []fixture{f, child, peer} {
		want := "closed"
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

func TestSessionHeldFinalizingSaveCompletesWithoutBlockingPeer(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	active, save := f.finalize(t, "active")
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "interrupt")); err != nil {
		t.Fatal(err)
	}
	// Stop delivery still needs a renewed envelope and replacement attachment.
	authority, err := session.RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil || authority.Generation != 2 {
		t.Fatalf("stopping control authority: %+v %v", authority, err)
	}
	if _, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution()); err != nil {
		t.Fatalf("stopping attachment: %v", err)
	}
	e := f.execution()
	e.AuthorityGeneration = 2
	if _, err := dispatchTurnForTest(t.Context(), f.pool, e); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("stopping process received business execution: %v", err)
	}
	f.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err := completedTurnForTest(t.Context(), f.pool, f.env, f.session, active.TurnID); err != nil {
		t.Fatalf("held finalizing Turn did not complete: %v", err)
	}
	peerTurn, peerSave := peer.finalize(t, "peer")
	peerCut, peerRoot := storage.cut(t, 3)
	f.capture(t, peerSave, peerRoot)
	if err := storage.publish(t, peerSave.ID, peerCut); err != nil {
		t.Fatal(err)
	}
	if err := completedTurnForTest(t.Context(), f.pool, f.env, peer.session, peerTurn.TurnID); err != nil {
		t.Fatalf("peer blocked by interrupted save: %v", err)
	}
}

func TestSessionCloseSealsOwnedTreeAndDrainsRunnableWork(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	parentTurn := f.enqueue(t, "parent")
	childTurn := child.enqueue(t, "child")
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil || r.Status != "closing" {
		t.Fatalf("close: %+v %v", r, err)
	}
	var lifecycle string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, child.session).Scan(&lifecycle); err != nil || lifecycle != "closing" {
		t.Fatalf("child not closing with its parent: %s %v", lifecycle, err)
	}
	for _, target := range []fixture{f, child} {
		if _, err := dispatchTurnForTest(t.Context(), f.pool, target.execution()); err != nil {
			t.Fatalf("accepted closing work could not dispatch: %v", err)
		}
	}
	// Model settlement without requiring the independent storage-publication
	// fixture. Closure must consume the same durable terminal state.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp() WHERE id=$1`, parentTurn.TurnID)
	if err := session.ReconcileSessionClosure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, f.session).Scan(&lifecycle); err != nil || lifecycle != "closing" {
		t.Fatalf("parent closed before its child: %s %v", lifecycle, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp() WHERE id=$1`, childTurn.TurnID)
	if err := session.ReconcileSessionClosure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, child.session); err != nil {
		t.Fatal(err)
	}
	var closed int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE status='closed' AND close_how='close'`).Scan(&closed); err != nil || closed != 2 {
		t.Fatalf("closed Sessions %d: %v", closed, err)
	}
}

func TestSessionCloseCancelsHeldQueuedTurnsAndReleasesHolds(t *testing.T) {
	f := newFixture(t)
	queued := f.enqueue(t, "held")
	hold := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, hold, f.session)
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil || r.Status != "closed" {
		t.Fatalf("close of a held Session waited: %+v %v", r, err)
	}
	var turn string
	var released bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT status FROM turns WHERE id=$1),(SELECT released_at IS NOT NULL FROM session_holds WHERE id=$2)`, queued.TurnID, hold).Scan(&turn, &released); err != nil || turn != "cancelled" || !released {
		t.Fatalf("held close: turn=%s released=%v %v", turn, released, err)
	}
	if kinds := eventKinds(t, f, f.session); !containsInOrder(kinds, "session.close", "turn.cancelled", "session.closed") {
		t.Fatalf("close events: %v", kinds)
	}
}

func TestSessionCloseCancelsPendingQuestionsWhileRunningTurnFinishes(t *testing.T) {
	f, a, ask, q := askFixture(t)
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil || r.Status != "closing" {
		t.Fatalf("close: %+v %v", r, err)
	}
	var askStatus, turnStatus string
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT status FROM turn_asks WHERE id=$1),(SELECT status FROM turns WHERE id=$2)`, ask, a.TurnID).Scan(&askStatus, &turnStatus); err != nil || askStatus != "cancelled" || turnStatus != "running" {
		t.Fatalf("close question/turn: %s %s %v", askStatus, turnStatus, err)
	}
	// A question asked while the Session closes never waits for a person.
	late := uuid.NewV7()
	if err := session.RuntimeAsk(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), f.execution(), a.TurnID, late, q); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turn_asks WHERE id=$1`, late).Scan(&askStatus); err != nil || askStatus != "cancelled" {
		t.Fatalf("question during close: %s %v", askStatus, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp() WHERE id=$1`, a.TurnID)
	if err := session.ReconcileSessionClosure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var lifecycle string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, f.session).Scan(&lifecycle); err != nil || lifecycle != "closed" {
		t.Fatalf("drained Session: %s %v", lifecycle, err)
	}
}

func TestSessionCloseOfInterruptedParentClosesItsChildren(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	parentTurn := f.enqueue(t, "parent")
	childTurn := child.enqueue(t, "child")
	interrupt, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "stop"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil || r.Status != "closed" {
		t.Fatalf("close of an interrupted parent: %+v %v", r, err)
	}
	var parent, childStatus, parentTurnStatus, childTurnStatus string
	var released bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT status FROM sessions WHERE id=$1),(SELECT status FROM sessions WHERE id=$2),(SELECT status FROM turns WHERE id=$3),(SELECT status FROM turns WHERE id=$4),(SELECT released_at IS NOT NULL FROM session_holds WHERE id=$5)`, f.session, child.session, parentTurn.TurnID, childTurn.TurnID, interrupt.HoldID).Scan(&parent, &childStatus, &parentTurnStatus, &childTurnStatus, &released); err != nil ||
		parent != "closed" || childStatus != "closed" || parentTurnStatus != "cancelled" || childTurnStatus != "cancelled" || !released {
		t.Fatalf("interrupted tree close: %s %s %s %s released=%v %v", parent, childStatus, parentTurnStatus, childTurnStatus, released, err)
	}
	for _, id := range []uuid.UUID{f.session, child.session} {
		view, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, id)
		if err != nil || len(view.Holds) != 0 {
			t.Fatalf("closed Session %s reports holds: %+v %v", id, view.Holds, err)
		}
	}
}

func TestSessionCancelReleasesHoldsAndRecordsOnlyClosure(t *testing.T) {
	f := newFixture(t)
	interrupt, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "stop"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "cancel", "cancel"))
	if err != nil || r.Status != "closed" {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	var released bool
	var how, data string
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT released_at IS NOT NULL FROM session_holds WHERE id=$1),(SELECT close_how FROM sessions WHERE id=$2),(SELECT convert_from(data,'UTF8') FROM session_events WHERE session_id=$2 AND kind='session.closed')`, interrupt.HoldID, f.session).Scan(&released, &how, &data); err != nil || !released || how != "cancel" || data != `{"how":"cancel"}` {
		t.Fatalf("cancel closure: released=%v how=%s data=%s %v", released, how, data, err)
	}
	for _, kind := range eventKinds(t, f, f.session) {
		if kind == "session.cancel" || kind == "session.cancelled" {
			t.Fatalf("cancel recorded %s", kind)
		}
	}
	var audits []string
	rows, err := f.pool.Query(t.Context(), `SELECT action FROM audit_events WHERE session_id=$1 AND action LIKE 'session.%' ORDER BY occurred_at,id`, f.session)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, action)
	}
	if rows.Err() != nil || !containsInOrder(audits, "session.cancel_requested") || slices.Contains(audits, "session.cancelled") || !slices.Contains(audits, "session.closed") {
		t.Fatalf("cancel audit: %v %v", audits, rows.Err())
	}
}

func TestSessionCloseAndCancelOfClosedSessionRecordNothing(t *testing.T) {
	f := newFixture(t)
	closed, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close"))
	if err != nil || closed.Status != "closed" {
		t.Fatalf("close of an idle Session: %+v %v", closed, err)
	}
	before := countRows(t, f, f.session)
	for _, kind := range []string{"close", "cancel"} {
		r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, kind, "again-"+kind))
		if err != nil || r.Status != "closed" || r.ID != uuid.Nil() {
			t.Fatalf("%s of a closed Session: %+v %v", kind, r, err)
		}
	}
	if after := countRows(t, f, f.session); after != before {
		t.Fatalf("no-op controls wrote rows: %v -> %v", before, after)
	}
}

func TestSessionCloseOfClosingSessionRecordsNothing(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "draining")
	if r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil || r.Status != "closing" {
		t.Fatalf("close: %+v %v", r, err)
	}
	before := countRows(t, f, f.session)
	if r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "again")); err != nil || r.Status != "closing" || r.ID != uuid.Nil() {
		t.Fatalf("close of a closing Session: %+v %v", r, err)
	}
	if after := countRows(t, f, f.session); after != before {
		t.Fatalf("close of a closing Session wrote rows: %v -> %v", before, after)
	}
}

type recordedRows struct{ controls, events, audits int }

func countRows(t *testing.T, f fixture, id uuid.UUID) recordedRows {
	t.Helper()
	var r recordedRows
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_controls WHERE session_id=$1),(SELECT count(*) FROM session_events WHERE session_id=$1),(SELECT count(*) FROM audit_events WHERE session_id=$1)`, id).Scan(&r.controls, &r.events, &r.audits); err != nil {
		t.Fatal(err)
	}
	return r
}

func eventKinds(t *testing.T, f fixture, id uuid.UUID) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT kind FROM session_events WHERE session_id=$1 ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return kinds
}

// containsInOrder reports whether want appears in got as a subsequence.
func containsInOrder(got []string, want ...string) bool {
	i := 0
	for _, kind := range got {
		if i < len(want) && kind == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestSessionControlAPIKeyRequiresExactPermission(t *testing.T) {
	f := newFixture(t)
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash)
 SELECT $1,org_id,project_id,id,'test','test',$2 FROM environments WHERE id=$3`, key, []byte(key.String()), f.env)
	dbtest.SetAPIKeyRole(t, f.pool, key, "sessions.read")
	caller := session.Caller{Kind: "api_key", ID: key}
	req := controlRequest(f, "interrupt", "interrupt")
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("view permission authorized interrupt: %v", err)
	}
	dbtest.SetAPIKeyRole(t, f.pool, key, "sessions.interrupt")
	r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.SetAPIKeyRole(t, f.pool, key, "sessions.resume")
	resume := controlRequest(f, "resume", "resume")
	resume.HoldID = r.HoldID
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, resume); err != nil {
		t.Fatal(err)
	}
	human, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "human"))
	if err != nil {
		t.Fatal(err)
	}
	// API keys and users are alike: resume permission releases a user's interrupt.
	resume.IdempotencyKey, resume.HoldID = "human", human.HoldID
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, resume); err != nil {
		t.Fatalf("key with resume permission could not release a user interrupt: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, key)
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("revoked key read historical acceptance: %v", err)
	}
}

func TestSessionLocalHoldReleaseDoesNotInvalidateHealthyChild(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	hold := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, hold, f.session)
	resume := controlRequest(f, "resume", "release")
	resume.HoldID = hold
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, child.session).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("local recovery invalidated healthy child: %d %v", generation, err)
	}
	resume.IdempotencyKey = "new-noop"
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.session).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("noop release changed authority: %d %v", generation, err)
	}
}

func TestSessionInterruptOfClosingSessionIsRefused(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "draining")
	if r, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil || r.Status != "closing" {
		t.Fatalf("close: %+v %v", r, err)
	}
	before := countRows(t, f, f.session)
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "stop")); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("interrupt of a closing Session: %v", err)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1`, f.session).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("refused interrupt created holds: %d %v", holds, err)
	}
	if after := countRows(t, f, f.session); after != before {
		t.Fatalf("refused interrupt wrote rows: %v -> %v", before, after)
	}
}
