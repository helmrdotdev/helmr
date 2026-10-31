package session_test

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

var holdPolicy = auditpolicy.Policy{RetentionSeconds: 3600}

// failProcessEpoch records one failure of the fixture Session's process at
// epoch, fences it and starts the next process, as reconstruction would.
func failProcessEpoch(t *testing.T, f fixture, epoch int64, reason string) {
	t.Helper()
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := session.LockSession(t.Context(), tx, f.env, f.session); err != nil {
			return err
		}
		return session.RecordSessionFailure(t.Context(), holdPolicy, tx, audit.Internal(audit.ControlPlane), session.Execution{EnvironmentID: f.env, SessionID: f.session, ProcessEpoch: epoch}, reason)
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1 AND epoch=$2;
 INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($3,$1,$2::bigint+1,$4,1,'ready')`, pgx.QueryExecModeSimpleProtocol, f.session, epoch, f.env, f.computer)
}

func processFailureHold(t *testing.T, f fixture) *uuid.UUID {
	t.Helper()
	var hold *uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT id FROM session_holds WHERE session_id=$1 AND kind='process_failed' AND released_at IS NULL)`, f.session).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	return hold
}

type processFailedEvent struct {
	ProcessEpoch int64      `json:"processEpoch"`
	Reason       string     `json:"reason"`
	HoldID       *uuid.UUID `json:"holdId"`
}

func processFailedEvents(t *testing.T, f fixture) []processFailedEvent {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT data FROM session_events WHERE session_id=$1 AND kind='session.process_failed' ORDER BY seq`, f.session)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	events := make([]processFailedEvent, len(raw))
	for i, data := range raw {
		if err := json.Unmarshal(data, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

func TestProcessFailuresHoldAfterConsecutiveThreshold(t *testing.T) {
	if session.ProcessFailureHoldThreshold != 3 {
		t.Fatalf("threshold %d", session.ProcessFailureHoldThreshold)
	}
	f := newFixture(t)
	queued := f.enqueue(t, "queued")
	failProcessEpoch(t, f, 1, "process_failed")
	failProcessEpoch(t, f, 2, "stop_deadline_elapsed")
	if hold := processFailureHold(t, f); hold != nil {
		t.Fatalf("held below threshold: %s", hold)
	}
	// Neither a recoverable Computer loss nor a revoked Secret counts.
	failProcessEpoch(t, f, 3, "computer_lost")
	failProcessEpoch(t, f, 4, "secret_unavailable")
	if hold := processFailureHold(t, f); hold != nil {
		t.Fatalf("uncounted failure held: %s", hold)
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, queued.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("queued Turn %s %v", status, err)
	}
	failProcessEpoch(t, f, 5, "execution_lost")
	hold := processFailureHold(t, f)
	if hold == nil {
		t.Fatal("third counted failure did not hold")
	}
	events := processFailedEvents(t, f)
	if len(events) != 3 || events[0].HoldID != nil || events[1].HoldID != nil || events[2].HoldID == nil || *events[2].HoldID != *hold ||
		events[0].Reason != "process_failed" || events[1].Reason != "stop_deadline_elapsed" || events[2].Reason != "execution_lost" || events[2].ProcessEpoch != 5 {
		t.Fatalf("failure events %+v", events)
	}
	var audited string
	if err := f.pool.QueryRow(t.Context(), `SELECT details->>'reason' FROM audit_events WHERE action='session.hold_created' AND session_id=$1`, f.session).Scan(&audited); err != nil || audited != "execution_lost" {
		t.Fatalf("hold audit reason %q %v", audited, err)
	}
	// Releasing the hold restarts the count.
	release := controlRequest(f, "resume", "release-failures")
	release.HoldID = *hold
	if _, err := session.ControlSession(t.Context(), holdPolicy, f.pool, f.caller(), release); err != nil {
		t.Fatal(err)
	}
	failProcessEpoch(t, f, 6, "process_failed")
	failProcessEpoch(t, f, 7, "process_failed")
	if hold := processFailureHold(t, f); hold != nil {
		t.Fatalf("release did not reset the count: %s", hold)
	}
	failProcessEpoch(t, f, 8, "process_failed")
	if processFailureHold(t, f) == nil {
		t.Fatal("count after release did not hold")
	}
}

func TestFinishedTurnResetsProcessFailureCount(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			failProcessEpoch(t, f, 1, "process_failed")
			failProcessEpoch(t, f, 2, "process_failed")
			// The harness finished a Turn on the next process, which later fails.
			finished := f.enqueue(t, "finished")
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status=$2,process_epoch=3,started_at=clock_timestamp(),terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp(),
 result=CASE WHEN $2='completed' THEN 'null'::jsonb END,result_recorded_at=CASE WHEN $2='completed' THEN clock_timestamp() END,result_digest=CASE WHEN $2='completed' THEN sha256('null') END,drain_evidence=CASE WHEN $2='completed' THEN 'fixture' END WHERE id=$1`, finished.TurnID, status)
			// The finishing process's own later failure is the first of three.
			failProcessEpoch(t, f, 3, "process_failed")
			failProcessEpoch(t, f, 4, "process_failed")
			if hold := processFailureHold(t, f); hold != nil {
				t.Fatalf("finished Turn did not reset the count: %s", hold)
			}
			failProcessEpoch(t, f, 5, "process_failed")
			hold := processFailureHold(t, f)
			if hold == nil {
				t.Fatal("three failures after the finished Turn did not hold")
			}
			events := processFailedEvents(t, f)
			if last := events[len(events)-1]; last.ProcessEpoch != 5 || last.HoldID == nil || *last.HoldID != *hold {
				t.Fatalf("hold not placed at epoch 5: %+v", events)
			}
		})
	}
}

func TestSetupFailuresReconstructUntilThresholdHolds(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	for epoch := int64(1); epoch <= 3; epoch++ {
		e.ProcessEpoch = epoch
		if epoch > 1 {
			// A setup failure with no running Turn leaves the queue on a new process.
			r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, epoch)
			if err != nil || r.Epoch != epoch || r.Status != "starting" {
				t.Fatalf("epoch %d reconstruction: %+v %v", epoch, r, err)
			}
		}
		attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.ObserveSessionFailure(t.Context(), holdPolicy, f.pool, *f.host(), e, attachment.Sequence); err != nil {
			t.Fatal(err)
		}
		if err := session.ObserveSessionStopped(t.Context(), holdPolicy, f.pool, *f.host(), e, attachment.Sequence); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 4); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("held Session reconstructed: %v", err)
	}
	var hold uuid.UUID
	var failures int
	if err := f.pool.QueryRow(t.Context(), `SELECT h.id,(SELECT count(*) FROM session_processes WHERE session_id=$1 AND failure_reason='process_failed') FROM session_holds h WHERE h.session_id=$1 AND h.kind='process_failed' AND h.released_at IS NULL`, e.SessionID).Scan(&hold, &failures); err != nil || failures != 3 {
		t.Fatalf("held after setup failures: %d %v", failures, err)
	}
	var queued bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='queued' AND started_at IS NULL FROM turns WHERE session_id=$1`, e.SessionID).Scan(&queued); err != nil || !queued {
		t.Fatalf("failed setup consumed input: %v %v", queued, err)
	}
	if _, err := session.ControlSession(t.Context(), holdPolicy, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "resume", IdempotencyKey: "resume-setup", HoldID: hold}); err != nil {
		t.Fatal(err)
	}
	if r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 4); err != nil || r.Epoch != 4 || r.Status != "starting" {
		t.Fatalf("resumed setup: %+v %v", r, err)
	}
}

func TestSessionDeadlineStopsProcessAndQueueContinues(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	acknowledgeAdmissionControl(t, f, e, attachment.Sequence)
	active, err := dispatchTurnForTest(t.Context(), f.pool, e)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := session.Enqueue(t.Context(), holdPolicy, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: e.SessionID, IdempotencyKey: "queued", Input: json.RawMessage(`[{"type":"text","text":"next"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, active.TurnID)
	for range 2 {
		if err := session.ExpireSessionDeadline(t.Context(), holdPolicy, f.pool, f.env, e.SessionID); err != nil {
			t.Fatal(err)
		}
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT status='interrupted' FROM turns WHERE id=$2)
 AND (SELECT status='queued' FROM turns WHERE id=$3)
 AND NOT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1)
 AND (SELECT status='stopping' AND stop_deadline_at IS NOT NULL AND stop_turn_id IS NULL AND failure_reason IS NULL FROM session_processes WHERE session_id=$1 AND epoch=1)
 AND (SELECT count(*)=1 FROM session_events WHERE session_id=$1 AND turn_id=$2 AND kind='turn.interrupted' AND convert_from(data,'UTF8')::jsonb='{"reason":"turn_deadline_elapsed"}')
 AND NOT EXISTS(SELECT 1 FROM session_events WHERE session_id=$1 AND kind='session.process_failed')`, e.SessionID, active.TurnID, queued.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("deadline settlement: %v %v", valid, err)
	}
	// The harness converges to shutdown; the queued Turn runs on the next process.
	if err := session.ObserveSessionStopped(t.Context(), holdPolicy, f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	next := e
	next.ProcessEpoch = 2
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, e.SessionID).Scan(&next.AuthorityGeneration); err != nil {
		t.Fatal(err)
	}
	if r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); err != nil || r.Status != "starting" {
		t.Fatalf("queued Turn did not reach a new process: %+v %v", r, err)
	}
	attachment, err = session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), next, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	acknowledgeAdmissionControl(t, f, next, attachment.Sequence)
	if got, err := dispatchTurnForTest(t.Context(), f.pool, next); err != nil || got.TurnID != queued.TurnID {
		t.Fatalf("next Turn on epoch 2: %+v %v", got, err)
	}
	if events := processFailedEvents(t, fixture{pool: f.pool, session: e.SessionID}); len(events) != 0 {
		t.Fatalf("cooperative stop counted as failure: %+v", events)
	}
}

func TestWedgedProcessAfterDeadlineCountsAsFailure(t *testing.T) {
	f := newFixture(t)
	active := f.enqueue(t, "active")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, active.TurnID)
	if err := session.ExpireSessionDeadline(t.Context(), holdPolicy, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET stop_deadline_at=clock_timestamp()-interval '1 second' WHERE session_id=$1`, f.session)
	if err := session.ReconcileProcessStopDeadline(t.Context(), holdPolicy, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := f.pool.QueryRow(t.Context(), `SELECT failure_reason FROM session_processes WHERE session_id=$1 AND epoch=1`, f.session).Scan(&reason); err != nil || reason != "stop_deadline_elapsed" {
		t.Fatalf("wedged stop: %q %v", reason, err)
	}
	if events := processFailedEvents(t, f); len(events) != 1 || events[0].Reason != "stop_deadline_elapsed" || events[0].HoldID != nil || events[0].ProcessEpoch != 1 {
		t.Fatalf("wedged stop events %+v", events)
	}
	if hold := processFailureHold(t, f); hold != nil {
		t.Fatalf("first failure held: %s", hold)
	}
}

// holdCase is one hold placed on an owned child for the release property.
type holdCase struct{ kind, issuer string }

func insertChildHold(t *testing.T, f, child fixture, c holdCase) uuid.UUID {
	t.Helper()
	hold := uuid.NewV7()
	issuerKind, issuerID := c.issuer, any(nil)
	switch c.issuer {
	case "session":
		issuerID = f.session
	case "other_session":
		// Another Session, such as an intermediate owner, placed this interrupt.
		issuerKind, issuerID = "session", uuid.NewV7()
	case "user", "api_key":
		issuerID = uuid.NewV7()
	}
	if c.kind == "recovery_gap" {
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,recovery_computer_id,recovery_lease_epoch,recovery_gap) VALUES($1,$2,$3,'recovery_gap','system',$4,1,jsonb_build_object('recoverySaveId',null,'restoredAt',null,'affectedTurnIds','[]'::jsonb))`, f.env, hold, child.session, child.computer)
		return hold
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,issuer_id) VALUES($1,$2,$3,$4,$5,$6::uuid)`, f.env, hold, child.session, c.kind, issuerKind, issuerID)
	return hold
}

// TestHoldCanReleaseMatchesControlSession checks that every hold's
// can_release equals what ControlSession accepts, over kinds and callers.
func TestHoldCanReleaseMatchesControlSession(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	readerKey, releaserKey := uuid.NewV7(), uuid.NewV7()
	for _, key := range []uuid.UUID{readerKey, releaserKey} {
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash)
 SELECT $1,org_id,project_id,id,'test-'||$1::uuid::text,'test',$2 FROM environments WHERE id=$3`, key, []byte(key.String()), f.env)
	}
	dbtest.SetAPIKeyRole(t, f.pool, readerKey, "sessions.read")
	dbtest.SetAPIKeyRole(t, f.pool, releaserKey, "sessions.read", "sessions.resume")
	callers := map[string]session.Caller{
		"user":          f.caller(),
		"reader_key":    {Kind: "api_key", ID: readerKey},
		"releaser_key":  {Kind: "api_key", ID: releaserKey},
		"parent":        {Kind: "session", MCPScopeID: f.mcpScope, ID: f.session, Execution: f.execution(), Host: f.host()},
		"child_runtime": {Kind: "session", MCPScopeID: child.mcpScope, ID: child.session, Execution: child.execution(), Host: child.host()},
	}
	cases := []holdCase{{"interrupt", "user"}, {"interrupt", "api_key"}, {"interrupt", "session"}, {"interrupt", "other_session"}, {"process_failed", "system"}, {"recovery_gap", "system"}, {"storage_pressure", "system"}}
	accepted := map[string]bool{}
	for _, c := range cases {
		hold := insertChildHold(t, f, child, c)
		for name, caller := range callers {
			var view session.SessionView
			var err error
			if caller.Kind == "session" && caller.ID == f.session {
				view, err = session.RuntimeGetSession(t.Context(), f.pool, caller, child.session)
			} else if caller.Kind == "session" {
				// A Session cannot inspect itself through runtime reads; the
				// owner's view still reports its own release rule below.
				view, err = session.GetSession(t.Context(), holdPolicy, f.pool, f.caller(), f.env, child.session)
			} else {
				view, err = session.GetSession(t.Context(), holdPolicy, f.pool, caller, f.env, child.session)
			}
			if err != nil {
				t.Fatalf("%s/%s view: %v", c.kind, name, err)
			}
			var found *session.HoldView
			for i := range view.Holds {
				if view.Holds[i].ID == hold {
					found = &view.Holds[i]
				}
			}
			if found == nil {
				t.Fatalf("%s/%s hold missing: %+v", c.kind, name, view.Holds)
			}
			// Attempt the release and roll it back so every caller sees the hold.
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			req := controlRequest(child, "resume", "property-"+name)
			req.HoldID = hold
			_, controlErr := session.ControlSession(t.Context(), holdPolicy, tx, caller, req)
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if controlErr != nil && !errors.Is(controlErr, session.ErrDenied) && !errors.Is(controlErr, session.ErrNotReady) {
				t.Fatalf("%s/%s release: %v", c.kind, name, controlErr)
			}
			if name == "child_runtime" {
				// Holds on the caller itself are not reported by a runtime read.
				if controlErr == nil {
					t.Fatalf("%s/%s released its own hold", c.kind, name)
				}
				continue
			}
			if found.CanRelease != (controlErr == nil) {
				t.Fatalf("%s/%s can_release=%t but ControlSession returned %v", c.kind, c.issuer+"/"+name, found.CanRelease, controlErr)
			}
			accepted[c.kind+"/"+c.issuer+"/"+name] = controlErr == nil
		}
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE id=$1`, hold)
	}
	for key, want := range map[string]bool{
		"interrupt/user/user": true, "interrupt/user/releaser_key": true, "interrupt/user/reader_key": false, "interrupt/user/parent": false,
		"interrupt/api_key/parent": false, "interrupt/session/parent": true, "interrupt/other_session/parent": false, "interrupt/other_session/user": true, "process_failed/system/parent": true, "recovery_gap/system/parent": true,
		"storage_pressure/system/user": false, "storage_pressure/system/parent": false, "storage_pressure/system/releaser_key": false,
	} {
		if accepted[key] != want {
			t.Fatalf("%s accepted=%t, want %t", key, accepted[key], want)
		}
	}

	// A closing parent still running a Turn can read its child, but cannot
	// enqueue, so it can no longer release the child's holds.
	running := f.enqueue(t, "closing-parent")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	closing := callers["parent"]
	closing.TurnID = running.TurnID
	hold := insertChildHold(t, f, child, holdCase{"process_failed", "system"})
	for _, state := range []string{"open", "closing"} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status=$2 WHERE id=$1`, f.session, state)
		view, err := session.RuntimeGetSession(t.Context(), f.pool, closing, child.session)
		if err != nil || len(view.Holds) != 1 || view.Holds[0].ID != hold {
			t.Fatalf("%s parent view: %+v %v", state, view.Holds, err)
		}
		tx, err := f.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		req := controlRequest(child, "resume", "closing-"+state)
		req.HoldID = hold
		_, controlErr := session.ControlSession(t.Context(), holdPolicy, tx, closing, req)
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if view.Holds[0].CanRelease != (controlErr == nil) || view.Holds[0].CanRelease != (state == "open") {
			t.Fatalf("%s parent can_release=%t but ControlSession returned %v", state, view.Holds[0].CanRelease, controlErr)
		}
	}
}

func recoveryGapEvents(t *testing.T, f fixture, sessionID uuid.UUID) []session.RecoveryGap {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT data FROM session_events WHERE session_id=$1 AND kind='session.recovery_gap' ORDER BY seq`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	gaps := make([]session.RecoveryGap, len(raw))
	for i, data := range raw {
		if err := json.Unmarshal(data, &gaps[i]); err != nil {
			t.Fatal(err)
		}
	}
	return gaps
}

// RecordComputerRecoveryGap holds only Sessions whose completed Turns lost
// their saves; open peers and Sessions joining while that hold is unreleased
// are notified without a hold.
func TestRecoveryGapSettlementHoldsAffectedAndNotifiesOthers(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	checkpointImageExposure(t, f)
	lost, _ := f.finalize(t, "lost-result")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1 AND epoch=1`, f.computer)
	if err := dispatch.ExpireLease(t.Context(), holdPolicy, f.pool, f.env, f.computer, 1); err != nil {
		t.Fatal(err)
	}
	// A repeated settlement of the same loss records nothing new.
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := session.LockComputerMembers(t.Context(), tx, f.env, f.computer); err != nil {
			return err
		}
		return session.RecordComputerRecoveryGap(t.Context(), holdPolicy, tx, f.env, f.computer, 1)
	}); err != nil {
		t.Fatal(err)
	}
	held := recoveryGapEvents(t, f, f.session)
	if len(held) != 1 || held[0].HoldID == nil || len(held[0].AffectedTurnIDs) != 1 || held[0].AffectedTurnIDs[0] != lost.TurnID || held[0].ComputerID != f.computer || held[0].LeaseEpoch != 1 {
		t.Fatalf("affected Session gap %+v", held)
	}
	var hold uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND kind='recovery_gap' AND released_at IS NULL`, f.session).Scan(&hold); err != nil || hold != *held[0].HoldID {
		t.Fatalf("affected hold %s %v", hold, err)
	}
	notified := recoveryGapEvents(t, f, peer.session)
	if len(notified) != 1 || notified[0].HoldID != nil || len(notified[0].AffectedTurnIDs) != 0 || notified[0].ComputerID != f.computer {
		t.Fatalf("peer notice %+v", notified)
	}
	var peerHolds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1`, peer.session).Scan(&peerHolds); err != nil || peerHolds != 0 {
		t.Fatalf("peer held %d %v", peerHolds, err)
	}
	// A Session joining while the affected hold is unreleased is notified.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET seed=jsonb_build_object('profile',$2::text) WHERE environment_id=$1`, f.env, definition.ComputerSeedProfile)
	start := func(key string) uuid.UUID {
		t.Helper()
		admitted, err := session.Start(t.Context(), holdPolicy, f.pool, nil, f.caller(), session.StartRequest{EnvironmentID: f.env, Agent: "agent", ComputerID: f.computer, IdempotencyKey: key, Input: json.RawMessage(`[{"type":"text","text":"continue"}]`)})
		if err != nil {
			t.Fatal(err)
		}
		return admitted.SessionID
	}
	joined := start("joined-while-held")
	if gaps := recoveryGapEvents(t, f, joined); len(gaps) != 1 || gaps[0].HoldID != nil || len(gaps[0].AffectedTurnIDs) != 0 || gaps[0].ComputerID != f.computer || gaps[0].LeaseEpoch != 1 {
		t.Fatalf("joined notice %+v", gaps)
	}
	release := controlRequest(f, "resume", "acknowledge")
	release.HoldID = hold
	if _, err := session.ControlSession(t.Context(), holdPolicy, f.pool, f.caller(), release); err != nil {
		t.Fatal(err)
	}
	// Once acknowledged, later Sessions are not notified again.
	if gaps := recoveryGapEvents(t, f, start("joined-after-release")); len(gaps) != 0 {
		t.Fatalf("notice after release %+v", gaps)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE kind='recovery_gap' AND released_at IS NULL`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("joined Sessions held %d %v", holds, err)
	}
}
