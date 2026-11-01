package session_test

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"reflect"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
)

func TestEmptySessionCreationReceiptAndConcurrentFirstInputs(t *testing.T) {
	f := newAdmissionFixture(t)
	req := f.startRequest("empty-create")
	req.Input = nil
	key := "empty"
	req.SessionKey = &key
	first, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.TurnID != uuid.Nil() || first.Sequence != 0 {
		t.Fatalf("unexpected work: %+v", first)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_id IS NULL AND deployment_id=$3 AND creation_turn_id IS NULL AND idle_since IS NOT NULL AND NOT EXISTS(SELECT 1 FROM turns WHERE session_id=s.id) AND (SELECT count(*) FROM computers WHERE environment_id=$1)=1 FROM sessions s WHERE environment_id=$1 AND id=$2`, f.env, first.SessionID, f.deployment).Scan(&valid); err != nil || !valid {
		t.Fatalf("empty creation allocated work: %v %v", valid, err)
	}
	conflict := req
	conflict.IdempotencyKey = "occupied"
	if _, err = session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), conflict); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("occupied key: %v", err)
	}
	// A later promotion cannot alter the code or placement selected for this Session.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	const count = 6
	results := make(chan session.Admission, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			a, e := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: uuid.NewV7().String(), Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
			results <- a
			errs <- e
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
	seq := map[int64]bool{}
	for a := range results {
		if a.SessionID != first.SessionID || seq[a.Sequence] {
			t.Fatalf("nonserial admission: %+v", a)
		}
		seq[a.Sequence] = true
	}
	for i := int64(1); i <= count; i++ {
		if !seq[i] {
			t.Fatalf("missing sequence %d", i)
		}
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT s.computer_id IS NOT NULL AND s.creation_turn_id IS NULL AND s.deployment_id=$3 AND c.origin_deployment_id=$3 AND (SELECT count(*) FROM computers WHERE environment_id=$1)=2 AND (SELECT count(DISTINCT computer_id) FROM turns WHERE session_id=$2)=1 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.environment_id=$1 AND s.id=$2`, f.env, first.SessionID, f.deployment).Scan(&valid); err != nil || !valid {
		t.Fatalf("first-input allocation: %v %v", valid, err)
	}
	for _, kind := range []string{"interrupt", "cancel"} {
		if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: first.SessionID, Kind: kind, IdempotencyKey: kind}); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("mutable create receipt: %+v %+v %v", first, replay, err)
	}
}

func TestEmptySessionAccountingAndControls(t *testing.T) {
	for _, kind := range []string{"close", "cancel", "interrupt", "idle"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			// Occupy the outstanding Turn limit. Empty creation must still use just one rate token.
			_ = f.enqueue(t, "existing")
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_outstanding_admissions=1,admission_tokens=2,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.env)
			req := f.startRequest("empty")
			req.Input = nil
			a, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
			if err != nil || !reflect.DeepEqual(a, retry) {
				t.Fatalf("retry: %+v %v", retry, err)
			}
			_, err = session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: a.SessionID, IdempotencyKey: "blocked", Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
			if !errors.Is(err, session.ErrNotReady) {
				t.Fatalf("Turn limit: %v", err)
			}
			var valid bool
			if err := f.pool.QueryRow(t.Context(), `SELECT admission_tokens=1 AND (SELECT computer_id IS NULL FROM sessions WHERE id=$2) AND (SELECT count(*) FROM computers WHERE environment_id=$1)=1 FROM environments WHERE id=$1`, f.env, a.SessionID).Scan(&valid); err != nil || !valid {
				t.Fatalf("rejected input leaked resource or token: %v %v", valid, err)
			}
			if kind == "idle" {
				setIdleClose(t, f.pool, a.SessionID, 1)
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 hour' WHERE id=$1`, a.SessionID)
				if err := session.ReconcileSessionIdle(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, a.SessionID); err != nil {
					t.Fatal(err)
				}
				if err := session.ReconcileSessionClosure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, a.SessionID); err != nil {
					t.Fatal(err)
				}
			} else {
				ctl, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: a.SessionID, Kind: kind, IdempotencyKey: kind})
				if err != nil {
					t.Fatal(err)
				}
				if kind == "interrupt" {
					if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: a.SessionID, Kind: "resume", HoldID: ctl.HoldID, IdempotencyKey: "resume"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.pool.QueryRow(t.Context(), `SELECT computer_id IS NULL AND NOT EXISTS(SELECT 1 FROM session_processes WHERE session_id=s.id) AND NOT EXISTS(SELECT 1 FROM turns WHERE session_id=s.id) FROM sessions s WHERE id=$1`, a.SessionID).Scan(&valid); err != nil || !valid {
				t.Fatalf("empty control allocated work: %v %v", valid, err)
			}
		})
	}
}

// emptySession creates a Session with no Computer, so its first enqueue
// creates one.
func emptySession(t *testing.T, f fixture, key string) uuid.UUID {
	t.Helper()
	req := f.startRequest(key)
	req.Input = nil
	created, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	return created.SessionID
}

// TestFirstEnqueueLocksImageSpecBeforeSessions: a holder of the image spec lock
// that next locks the Session, as preparation expiry does, must not deadlock
// with a first enqueue that is creating the Session's Computer.
func TestFirstEnqueueLocksImageSpecBeforeSessions(t *testing.T) {
	f := newAdmissionFixture(t)
	sessionID := emptySession(t, f, "lock-order")
	var spec uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT image_spec_id FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2`, f.env, f.deployment).Scan(&spec); err != nil {
		t.Fatal(err)
	}
	holder, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), holder, `SELECT id FROM image_specs WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.env, spec)
	done := make(chan error, 1)
	go func() {
		_, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: sessionID, IdempotencyKey: "first", Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("enqueue finished while the image spec was locked: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("enqueue never waited on the image spec")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := holder.Exec(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE NOWAIT`, f.env, sessionID); err != nil {
		t.Fatalf("enqueue held the Session while waiting on the image spec: %v", err)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var attached bool
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_id IS NOT NULL FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, sessionID).Scan(&attached); err != nil || !attached {
		t.Fatalf("first enqueue did not create the Computer: %v %v", attached, err)
	}
}

// TestFirstEnqueueRetriesExactlyAfterCapacityExhaustion: a first enqueue
// rejected for storage capacity leaves no Computer or Turn behind, and the same
// request admits once capacity returns.
func TestFirstEnqueueRetriesExactlyAfterCapacityExhaustion(t *testing.T) {
	f := newAdmissionFixture(t)
	sessionID := emptySession(t, f, "capacity")
	var before int
	var limit int64
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computers WHERE environment_id=$1),max_reserved_storage_bytes FROM environments WHERE id=$1`, f.env).Scan(&before, &limit); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_reserved_storage_bytes=$2 WHERE id=$1`, f.env, disk.SeedCapacity)
	req := session.EnqueueRequest{EnvironmentID: f.env, SessionID: sessionID, IdempotencyKey: "retry", Input: json.RawMessage(`[{"type":"text","text":"work"}]`)}
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); !errors.Is(err, computer.ErrNotReady) {
		t.Fatalf("capacity exhaustion: %v", err)
	}
	var clean bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computers WHERE environment_id=$1)=$3 AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=$2) AND (SELECT computer_id IS NULL FROM sessions WHERE environment_id=$1 AND id=$2)`, f.env, sessionID, before).Scan(&clean); err != nil || !clean {
		t.Fatalf("rejected enqueue left partial work: %v %v", clean, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_reserved_storage_bytes=$2 WHERE id=$1`, f.env, limit)
	first, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || again != first {
		t.Fatalf("exact retry: %+v %+v %v", first, again, err)
	}
	var computers int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computers WHERE environment_id=$1`, f.env).Scan(&computers); err != nil || computers != before+1 {
		t.Fatalf("computers %d, want %d: %v", computers, before+1, err)
	}
}
