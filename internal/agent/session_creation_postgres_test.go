package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestEmptySessionCreationReceiptAndConcurrentFirstInputs(t *testing.T) {
	f := newAdmissionFixture(t)
	req := f.startRequest("empty-create")
	req.Input = nil
	key := "empty"
	req.SessionKey = &key
	first, err := Start(t.Context(), f.pool, nil, f.caller(), req)
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
	conflict.RetryKey = "occupied"
	if _, err = Start(t.Context(), f.pool, nil, f.caller(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("occupied key: %v", err)
	}
	// A later promotion cannot alter the code or placement selected for this Session.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	const count = 6
	results := make(chan Admission, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			a, e := Enqueue(t.Context(), f.pool, nil, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, RetryKey: uuid.NewV7().String(), Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
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
		if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: first.SessionID, Kind: kind, RetryKey: kind}); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := Start(t.Context(), f.pool, nil, f.caller(), req)
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
			a, err := Start(t.Context(), f.pool, nil, f.caller(), req)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := Start(t.Context(), f.pool, nil, f.caller(), req)
			if err != nil || !reflect.DeepEqual(a, retry) {
				t.Fatalf("retry: %+v %v", retry, err)
			}
			_, err = Enqueue(t.Context(), f.pool, nil, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: a.SessionID, RetryKey: "blocked", Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
			if !errors.Is(err, ErrNotReady) {
				t.Fatalf("Turn limit: %v", err)
			}
			var valid bool
			if err := f.pool.QueryRow(t.Context(), `SELECT admission_tokens=1 AND (SELECT computer_id IS NULL FROM sessions WHERE id=$2) AND (SELECT count(*) FROM computers WHERE environment_id=$1)=1 FROM environments WHERE id=$1`, f.env, a.SessionID).Scan(&valid); err != nil || !valid {
				t.Fatalf("rejected input leaked resource or token: %v %v", valid, err)
			}
			if kind == "idle" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 hour',close_after_idle_ms=1 WHERE id=$1`, a.SessionID)
				if err := reconcileSessionIdle(t.Context(), f.pool, f.env, a.SessionID); err != nil {
					t.Fatal(err)
				}
				if err := ReconcileSessionClosure(t.Context(), f.pool, f.env, a.SessionID); err != nil {
					t.Fatal(err)
				}
			} else {
				ctl, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: a.SessionID, Kind: kind, RetryKey: kind})
				if err != nil {
					t.Fatal(err)
				}
				if kind == "interrupt" {
					if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: a.SessionID, Kind: "resume", HoldID: ctl.HoldID, RetryKey: "resume"}); err != nil {
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
