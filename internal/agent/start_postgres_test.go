package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
)

func newAdmissionFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET seed=jsonb_build_object('profile',$3::text) WHERE environment_id=$1 AND id=$2;
 UPDATE template_definitions SET cpu_millis=1000,memory_bytes=536870912,max_image_age_ms=12345 WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE computers SET image_spec_id=$2,origin_deployment_id=$2,origin_template_id=origin_deployment_id,cpu_millis=1000,memory_bytes=536870912,storage_reservation_bytes=$4 WHERE environment_id=$1;`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, definition.ComputerSeedProfile, disk.SeedCapacity)
	return f
}
func (f fixture) startRequest(key string) StartRequest {
	return StartRequest{EnvironmentID: f.env, Agent: "agent", IdempotencyKey: key, Input: json.RawMessage(`[{"type":"text","text":"work 1"}]`)}
}

func TestStartFractionalCPUUsesPhysicalAdmissionBound(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE template_definitions SET cpu_millis=500 WHERE environment_id=$1;
 UPDATE environments SET max_cpu_millis=999 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("physical-cpu")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("admitted a whole vCPU below physical bound: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=1000 WHERE id=$1`, f.env)
	admitted, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("physical-cpu"))
	if err != nil {
		t.Fatal(err)
	}
	var declared int64
	if err := f.pool.QueryRow(t.Context(), `SELECT c.cpu_millis FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.id=$1`, admitted.SessionID).Scan(&declared); err != nil || declared != 500 {
		t.Fatalf("declared CPU changed: %d %v", declared, err)
	}
}

func TestStartAtomicFreshAdmissionAndConcurrentRetry(t *testing.T) {
	f := newAdmissionFixture(t)
	const clients = 8
	results := make(chan Admission, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for range clients {
		wg.Go(func() {
			a, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("same"))
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
	var first Admission
	for a := range results {
		if first.TurnID == uuid.Nil() {
			first = a
		}
		if !reflect.DeepEqual(a, first) || !a.Created || a.Sequence != 1 {
			t.Fatalf("changed retry: %+v %+v", first, a)
		}
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.deployment_id=$3 AND s.root_session_id=s.id AND s.parent_session_id IS NULL AND s.requester_session_id IS NULL AND c.id<>$4 AND c.image_build_id IS NOT NULL AND c.image_id IS NULL AND c.preparation_max_age_ms=12345 AND c.storage_reservation_bytes=$5 AND (SELECT count(*) FROM turns WHERE environment_id=$1)=1 AND (SELECT count(*) FROM computers WHERE environment_id=$1)=2 AND (SELECT count(*) FROM session_events WHERE environment_id=$1)=1 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.environment_id=$1 AND s.id=$2`, f.env, first.SessionID, f.deployment, f.computer, disk.SeedCapacity).Scan(&valid); err != nil || !valid {
		t.Fatalf("incomplete atomic admission: %v %v", valid, err)
	}
	req := f.startRequest("same")
	req.Input = json.RawMessage(`[{"type":"text","text":"{\"work\":2}"}]`)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting retry: %v", err)
	}
	// Reconciliation still returns the recorded resolution after defaults disappear.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL,admission_tokens=0,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.env)
	if a, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("same")); err != nil || !reflect.DeepEqual(a, first) {
		t.Fatalf("retry resolved defaults: %+v %v", a, err)
	}
}

func TestCreateSessionKeyNeverContinuesExistingSession(t *testing.T) {
	f := newAdmissionFixture(t)
	key := "conversation"
	req := f.startRequest("first")
	req.SessionKey = &key
	req.ComputerID = f.computer
	first, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	for _, state := range []string{"open", "closed"} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status=$2 WHERE id=$1`, first.SessionID, state)
		changed := req
		changed.IdempotencyKey = state
		if _, err := Start(t.Context(), f.pool, nil, f.caller(), changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("occupied %s key: %v", state, err)
		}
		replay, err := Start(t.Context(), f.pool, nil, f.caller(), req)
		if err != nil || !reflect.DeepEqual(first, replay) {
			t.Fatalf("receipt after %s: %+v %v", state, replay, err)
		}
	}
}

func TestRuntimeCreationSelectsCurrentStartAndPinnedSpawn(t *testing.T) {
	f := newAdmissionFixture(t)
	admitted := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, TurnID: admitted.TurnID, Execution: f.execution(), Host: f.host()}
	promoted := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,'sha256:'||repeat('e',64));
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes) SELECT environment_id,$3,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2;
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) SELECT environment_id,agent_id,$3,template_id,has_init FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE environments SET current_deployment_id=$3 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, promoted)
	for _, method := range []string{"create", "spawn"} {
		req := f.startRequest(method)
		req.ComputerID = f.computer
		a, err := admitSession(t.Context(), f.pool, nil, caller, req, method)
		if err != nil {
			t.Fatal(err)
		}
		expected := f.deployment
		if method == "create" {
			expected = promoted
		}
		var valid bool
		if err := f.pool.QueryRow(t.Context(), `SELECT deployment_id=$3 AND requester_session_id=$4 AND origin_turn_id=$5 AND causal_depth=1 AND CASE WHEN $6='spawn' THEN parent_session_id=$4 AND root_session_id=$4 ELSE parent_session_id IS NULL AND root_session_id=id END FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, a.SessionID, expected, f.session, admitted.TurnID, method).Scan(&valid); err != nil || !valid {
			t.Fatalf("creation ownership: %v %v", valid, err)
		}
	}
	if err := closeProcessingForTest(t.Context(), f.pool, f.execution(), admitted.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(t.Context(), f.pool, nil, caller, f.startRequest("late")); !errors.Is(err, ErrDenied) {
		t.Fatalf("closed invoking Turn admitted work: %v", err)
	}
}

func TestStartBudgetFailureRollsBackPreparationAndComputer(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_tokens=0,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.env)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("limited")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("rate exhaustion: %v", err)
	}
	var clean bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computers WHERE environment_id=$1)=1 AND NOT EXISTS(SELECT 1 FROM image_builds WHERE environment_id=$1) AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1) AND (SELECT count(*) FROM sessions WHERE environment_id=$1)=1`, f.env).Scan(&clean); err != nil || !clean {
		t.Fatalf("partial admission: %v %v", clean, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_tokens=1,max_reserved_storage_bytes=$2 WHERE id=$1`, f.env, disk.SeedCapacity)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("disk-limited")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("disk exhaustion: %v", err)
	}
	req := f.startRequest("shared")
	req.ComputerID = f.computer
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), req); err != nil {
		t.Fatalf("explicit sharing consumed fresh disk reservation: %v", err)
	}
}

func TestCreateConcurrentDistinctRequestsConflictOnExplicitKey(t *testing.T) {
	f := newAdmissionFixture(t)
	key := "same-conversation"
	const clients = 6
	results := make(chan Admission, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			req := f.startRequest(fmt.Sprint(i))
			req.SessionKey = &key
			a, err := Start(t.Context(), f.pool, nil, f.caller(), req)
			results <- a
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	conflicts := 0
	for err := range errs {
		if errors.Is(err, ErrConflict) {
			conflicts++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	for a := range results {
		if a.Created {
			created++
			if a.Sequence != 1 || a.SessionID == uuid.Nil() {
				t.Fatalf("invalid creation: %+v", a)
			}
		}
	}
	if created != 1 || conflicts != clients-1 {
		t.Fatalf("created=%d conflicts=%d", created, conflicts)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computers WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 2 {
		t.Fatalf("key race leaked Computers: %d %v", count, err)
	}
}

func TestStartAndEnqueueShareOneTokenBudget(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_burst=2,admission_tokens=2,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.env)
	req := f.startRequest("first")
	req.ComputerID = f.computer
	first, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	enqueue := EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "second", Input: json.RawMessage(`[]`)}
	second, err := Enqueue(t.Context(), f.pool, nil, f.caller(), enqueue)
	if err != nil {
		t.Fatalf("initial Session consumed more than one token: %v", err)
	}
	if retry, err := Start(t.Context(), f.pool, nil, f.caller(), req); err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("start retry charged: %+v %v", retry, err)
	}
	if retry, err := Enqueue(t.Context(), f.pool, nil, f.caller(), enqueue); err != nil || retry != second {
		t.Fatalf("enqueue retry charged: %+v %v", retry, err)
	}
	req.IdempotencyKey = "third"
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, ErrNotReady) {
		t.Fatalf("start bypassed shared budget: %v", err)
	}
	enqueue.IdempotencyKey = "fourth"
	if _, err := Enqueue(t.Context(), f.pool, nil, f.caller(), enqueue); !errors.Is(err, ErrNotReady) {
		t.Fatalf("enqueue bypassed shared budget: %v", err)
	}
	var preserved bool
	if err := f.pool.QueryRow(t.Context(), `SELECT admission_tokens=0 AND admission_refilled_at>clock_timestamp()+interval '23 hours' FROM environments WHERE id=$1`, f.env).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("clock rollback replenished tokens: %v %v", preserved, err)
	}
}

func TestRuntimeCreationRechecksTurnAfterLockWait(t *testing.T) {
	f := newAdmissionFixture(t)
	a := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, TurnID: a.TurnID, Execution: f.execution(), Host: f.host()}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.env); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		req := f.startRequest("blocked")
		req.ComputerID = f.computer
		_, err := Start(ctx, f.pool, nil, caller, req)
		result <- err
	}()
	for {
		var waiting bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%current_deployment_id,max_outstanding_admissions%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case err := <-result:
			t.Fatalf("finished before lock: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	if err = closeProcessingForTest(ctx, f.pool, f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrDenied) {
		t.Fatalf("closed Turn crossed lock wait: %v", err)
	}
}

func TestStartAPIKeyCreationAndContinuationPermissions(t *testing.T) {
	f := newAdmissionFixture(t)
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,role,permissions,name,key_prefix,token_hash) SELECT $1,org_id,project_id,id,'developer',ARRAY['sessions.send'],'test','test',$2 FROM environments WHERE id=$3`, key, []byte(key.String()), f.env)
	caller := Caller{Kind: "api_key", ID: key}
	sessionKey := "conversation"
	req := f.startRequest("first")
	req.SessionKey = &sessionKey
	req.ComputerID = f.computer
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("send grant created work: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET permissions=ARRAY['sessions.create'] WHERE id=$1`, key)
	first, err := Start(t.Context(), f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	req.IdempotencyKey = "continue"
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate creation reused a Session: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET permissions=ARRAY['sessions.create','sessions.send'] WHERE id=$1`, key)
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("send permission bypassed key conflict: %v", err)
	}
	if _, err := Enqueue(t.Context(), f.pool, nil, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "send", Input: req.Input}); err != nil {
		t.Fatalf("explicit send failed: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, key)
	req.IdempotencyKey = "first"
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked key replayed receipt: %v", err)
	}
}

func TestStartPinsPublishedImageAndPreservesMaximumAge(t *testing.T) {
	f := newAdmissionFixture(t)
	p, key := preparationPublicationTestFor(t, preparationFixtureFor(t, f))
	root := p.captureCapacity(t, key, disk.SeedCapacity)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	a, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("image"))
	if err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT c.image_id IS NOT NULL AND c.preparation_max_age_ms=12345 AND c.storage_reservation_bytes=$3 AND c.initial_root_id IS NOT NULL AND octet_length(c.initial_root_digest)=32 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.environment_id=$1 AND s.id=$2`, f.env, a.SessionID, disk.SeedCapacity).Scan(&valid); err != nil || !valid {
		t.Fatalf("missing admission image or age: %v %v", valid, err)
	}
}

func TestCreateAllowsOmittedInputAndRejectsFaultedPlacement(t *testing.T) {
	f := newAdmissionFixture(t)
	req := f.startRequest("invalid")
	req.Input = nil
	if a, err := Start(t.Context(), f.pool, nil, f.caller(), req); err != nil || a.TurnID != uuid.Nil() {
		t.Fatalf("empty creation: %+v %v", a, err)
	}
	req = f.startRequest("faulted")
	req.ComputerID = f.computer
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='unrecoverable disk' WHERE environment_id=$1 AND id=$2`, f.env, f.computer)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, ErrNotReady) {
		t.Fatalf("faulted explicit placement: %v", err)
	}
}

func TestRuntimeStartPreservesCausalLimitAcrossIndependentRoots(t *testing.T) {
	f := newAdmissionFixture(t)
	a := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, TurnID: a.TurnID, Execution: f.execution(), Host: f.host()}
	// A caller at the configured ancestry bound cannot create a new independent
	// root to escape that bound. Its causal requester is retained independently of ownership.
	peer := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET requester_session_id=$3,causal_depth=1 WHERE environment_id=$1 AND id=$2;
 UPDATE environments SET max_causal_depth=1 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.session, peer.session)
	req := f.startRequest("too-deep")
	req.ComputerID = f.computer
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("independent start reset causal depth: %v", err)
	}
	req.EnvironmentID = uuid.NewV7()
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross Environment start allowed: %v", err)
	}
}

func TestStartRejectsUnsupportedDiskSizeWithoutReservations(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE template_definitions SET disk_bytes=107374182400 WHERE environment_id=$1`, f.env)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("large-disk")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsupported disk admitted: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computers WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("disk rejection leaked reservation: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE template_definitions SET disk_bytes=34359738368 WHERE environment_id=$1`, f.env)
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("supported-disk")); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCreationWithFreshComputer(t *testing.T) {
	f := newAdmissionFixture(t)
	first := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, TurnID: first.TurnID, Execution: f.execution(), Host: f.host()}
	for _, method := range []string{"create", "spawn"} {
		a, err := admitSession(t.Context(), f.pool, nil, caller, f.startRequest(method), method)
		if err != nil {
			t.Fatal(err)
		}
		var valid bool
		err = f.pool.QueryRow(t.Context(), `SELECT s.computer_id<>$3 AND s.deployment_id=$4 AND s.origin_turn_id=$5 AND c.image_build_id IS NOT NULL AND c.preparation_max_age_ms=12345 AND t.seq=1 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2`, f.env, a.SessionID, f.computer, f.deployment, first.TurnID).Scan(&valid)
		if err != nil || !valid {
			t.Fatalf("fresh runtime %s: %v %v", method, valid, err)
		}
	}
}

func TestOwnedCallerCannotCreateOrDiscoverNewWork(t *testing.T) {
	f := newAdmissionFixture(t)
	first := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	peer := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET requester_session_id=$3,parent_session_id=$3,root_session_id=$3,causal_depth=1 WHERE environment_id=$1 AND id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.session, peer.session)
	caller := Caller{Kind: "session", ID: f.session, TurnID: first.TurnID, Execution: f.execution(), Host: f.host()}
	req := f.startRequest("child")
	req.ComputerID = f.computer
	if _, err := Spawn(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrRootSessionRequired) {
		t.Fatalf("owned depth admitted: %v", err)
	}
	if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrRootSessionRequired) {
		t.Fatal(err)
	}
	for _, operation := range []string{"spawn", "start"} {
		if _, err := RuntimeListAgents(t.Context(), f.pool, caller, operation, ""); !errors.Is(err, ErrRootSessionRequired) {
			t.Fatalf("child discovered %s: %v", operation, err)
		}
	}
}

func TestStartRejectsNonMember(t *testing.T) {
	f := newAdmissionFixture(t)
	user := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'nonmember')`, user)
	if _, err := Start(t.Context(), f.pool, nil, Caller{Kind: "user", ID: user}, f.startRequest("nonmember")); !errors.Is(err, ErrDenied) {
		t.Fatalf("nonmember admitted: %v", err)
	}
}

func TestSessionHistoryPolicyPinnedAtAdmission(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET history_retention_mode='duration',history_retention_seconds=3600 WHERE id=$1`, f.env)
	first, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("history-first"))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET history_retention_mode='until_environment_deletion',history_retention_seconds=NULL WHERE id=$1`, f.env)
	second, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("history-second"))
	if err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT a.history_retention_mode='duration' AND a.history_retention_seconds=3600 AND a.history_eligible_at IS NULL AND a.history_expires_at IS NULL AND b.history_retention_mode='until_environment_deletion' AND b.history_retention_seconds IS NULL FROM sessions a,sessions b WHERE a.environment_id=$1 AND a.id=$2 AND b.environment_id=$1 AND b.id=$3`, f.env, first.SessionID, second.SessionID).Scan(&valid); err != nil || !valid {
		t.Fatalf("admission policy snapshot: %v %v", valid, err)
	}
	replay, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("history-first"))
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("policy update changed retry: %+v %v", replay, err)
	}
}

func TestMCPStartAndSpawnRetainSessionOriginWithoutInventingTurn(t *testing.T) {
	for _, starting := range []bool{false, true} {
		t.Run(fmt.Sprintf("starting=%v", starting), func(t *testing.T) {
			f := newAdmissionFixture(t)
			// An existing Turn must never be substituted as MCP provenance.
			f.enqueue(t, "unrelated-pending-turn")
			if starting {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.session)
			}
			caller := Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
			for _, method := range []string{"create", "spawn"} {
				req := f.startRequest(method)
				req.ComputerID = f.computer
				receipt, err := admitSession(t.Context(), f.pool, nil, caller, req, method)
				if err != nil {
					t.Fatal(err)
				}
				var valid bool
				if err := f.pool.QueryRow(t.Context(), `SELECT s.deployment_id=$3 AND s.requester_session_id=$4 AND s.origin_turn_id IS NULL AND s.causal_depth=1
                    AND t.caller_kind='session' AND t.caller_id=$4 AND t.origin_turn_id IS NULL
                    AND CASE WHEN $6='spawn' THEN s.parent_session_id=$4 AND s.root_session_id=$4 ELSE s.parent_session_id IS NULL AND s.root_session_id=s.id END
                    FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND t.id=$5`, f.env, receipt.SessionID, f.deployment, f.session, receipt.TurnID, method).Scan(&valid); err != nil || !valid {
					t.Fatalf("Session origin %v %v", valid, err)
				}
				again, err := admitSession(t.Context(), f.pool, nil, caller, req, method)
				if err != nil || !reflect.DeepEqual(again, receipt) {
					t.Fatalf("retry %+v %v", again, err)
				}
				view, err := RuntimeObserveTurn(t.Context(), f.pool, caller, receipt.SessionID, receipt.TurnID)
				if err != nil || view.Status != "queued" {
					t.Fatalf("outcome scope %+v %v", view, err)
				}
			}
			req := f.startRequest("")
			req.ComputerID = f.computer
			if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("missing key %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.session)
			req.IdempotencyKey = "stale-new-admission"
			if _, err := Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrDenied) {
				t.Fatalf("stale caller %v", err)
			}
		})
	}
}
