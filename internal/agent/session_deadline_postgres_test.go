package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSessionDeadlineHoldsOnlyItsSessionAndPreservesSave(t *testing.T) {
	for _, finalizing := range []bool{false, true} {
		name := "running"
		if finalizing {
			name = "finalizing"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			child := ownedFixture(t, f)
			peer := f.peer(t)
			var active Admission
			var save SaveRequest
			if finalizing {
				active, save = f.finalize(t, "active")
			} else {
				active = f.enqueue(t, "active")
				if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
					t.Fatal(err)
				}
			}
			queued := f.enqueue(t, "queued")
			childTurn := child.enqueue(t, "child")
			peerTurn := peer.enqueue(t, "peer")
			for _, p := range []fixture{child, peer} {
				if _, err := Dispatch(t.Context(), f.pool, p.execution()); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, active.TurnID)
			for range 2 {
				if err := expireSessionDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
					t.Fatal(err)
				}
			}
			for id, want := range map[uuid.UUID]string{active.TurnID: "interrupted", queued.TurnID: "queued", childTurn.TurnID: "running", peerTurn.TurnID: "running"} {
				var state string
				if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&state); err != nil || state != want {
					t.Fatalf("Turn %s: %s %v", id, state, err)
				}
			}
			var hold uuid.UUID
			if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND scope='local' AND issuer_kind='system'`, f.session).Scan(&hold); err != nil {
				t.Fatal(err)
			}
			var holds int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 1 {
				t.Fatalf("holds %d: %v", holds, err)
			}
			var pending bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status='stopping' AND fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&pending); err != nil || !pending {
				t.Fatalf("deadline claimed physical stop: %v %v", pending, err)
			}
			if finalizing {
				var retained bool
				if err := f.pool.QueryRow(t.Context(), `SELECT result->>'result'='1' AND result_digest IS NOT NULL AND result_recorded_at IS NOT NULL FROM turns WHERE id=$1`, active.TurnID).Scan(&retained); err != nil || !retained {
					t.Fatalf("recorded result lost: %v %v", retained, err)
				}
				var state string
				if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, save.ID).Scan(&state); err != nil || state != "requested" {
					t.Fatalf("ordered save changed: %s %v", state, err)
				}
			}
			release := controlRequest(f, "resume", "release-deadline")
			release.HoldID = hold
			if _, err := ControlSession(t.Context(), f.pool, f.caller(), release); err != nil {
				t.Fatal(err)
			}
			if err := expireSessionDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("deadline replayed hold: %d %v", holds, err)
			}
		})
	}
}

func TestSessionDeadlineDoesNotChangeCompletedOutcome(t *testing.T) {
	f := newFixture(t)
	active, save := f.finalize(t, "completed")
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	f.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, active.TurnID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, active.TurnID)
	if err := expireSessionDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='completed' AND NOT EXISTS(SELECT 1 FROM session_holds) FROM turns WHERE id=$1`, active.TurnID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("completed outcome changed: %v %v", unchanged, err)
	}
}

func TestSessionLifecycleWorkerExpiresWithoutGuest(t *testing.T) {
	f := newFixture(t)
	active := f.enqueue(t, "running")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, active.TurnID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunSessionLifecycle(ctx, f.pool, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	limit := time.Now().Add(5 * time.Second)
	for {
		var interrupted bool
		if err := f.pool.QueryRow(t.Context(), `SELECT status='interrupted' FROM turns WHERE id=$1`, active.TurnID).Scan(&interrupted); err != nil {
			t.Fatal(err)
		}
		if interrupted {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("worker did not expire Turn")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("worker exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not join")
	}
}

func TestSessionLifecycleClosesAfterRecordedResultSettles(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	active, save := f.finalize(t, "result")
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil {
		t.Fatal(err)
	}
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	f.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, active.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []uuid.UUID{f.session, child.session} {
		var closed bool
		if err := f.pool.QueryRow(t.Context(), `SELECT status='closed' FROM sessions WHERE id=$1`, s).Scan(&closed); err != nil || !closed {
			t.Fatalf("Session %s not closed: %v %v", s, closed, err)
		}
	}
}

func waitSessionLifecycleLock(t *testing.T, f fixture) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(until) {
			t.Fatal("lifecycle operation did not wait for lock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionDeadlineRechecksTimeAfterOwnerLock(t *testing.T) {
	f := newFixture(t)
	active := f.enqueue(t, "deadline")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, active.TurnID)
	if err := expireSessionDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var running bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='running' AND NOT EXISTS(SELECT 1 FROM session_holds) FROM turns WHERE id=$1`, active.TurnID).Scan(&running); err != nil || !running {
		t.Fatalf("future deadline expired: %v %v", running, err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- expireSessionDeadline(t.Context(), f.pool, f.env, f.session) }()
	waitSessionLifecycleLock(t, f)
	if _, err = tx.Exec(t.Context(), `UPDATE turns SET deadline_at=clock_timestamp() WHERE id=$1`, active.TurnID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT status='interrupted' FROM turns WHERE id=$1`, active.TurnID).Scan(&running); err != nil || !running {
		t.Fatalf("post-lock deadline missed: %v %v", running, err)
	}
}

func TestSessionLifecycleBatchMovesPastBlockedSession(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	for _, s := range []fixture{f, peer} {
		s.enqueue(t, "active")
		if _, err := Dispatch(t.Context(), f.pool, s.execution()); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second'`)
	var first uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions ORDER BY id LIMIT 1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM sessions WHERE id=$1 FOR NO KEY UPDATE`, first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		position sessionLifecyclePosition
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		p, _, e := reconcileSessionLifecycle(ctx, f.pool, sessionLifecyclePosition{})
		done <- outcome{p, e}
	}()
	waitSessionLifecycleLock(t, f)
	cancel()
	result := <-done
	if result.err == nil || result.position.session != first {
		t.Fatalf("blocked batch: %+v", result)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	next, _, err := reconcileSessionLifecycle(t.Context(), f.pool, result.position)
	if err != nil {
		t.Fatal(err)
	}
	var interrupted int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE status='interrupted'`).Scan(&interrupted); err != nil || interrupted != 1 {
		t.Fatalf("later Session excluded: %d %v", interrupted, err)
	}
	if next != (sessionLifecyclePosition{}) {
		t.Fatalf("short batch did not wrap: %+v", next)
	}
	if _, _, err = reconcileSessionLifecycle(t.Context(), f.pool, next); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE status='interrupted'`).Scan(&interrupted); err != nil || interrupted != 2 {
		t.Fatalf("blocked Session not revisited: %d %v", interrupted, err)
	}
}

func TestSessionLifecycleDoesNotLockParkedClosingTree(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	child.enqueue(t, "working")
	if _, err := Dispatch(t.Context(), f.pool, child.execution()); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM sessions WHERE id=$1 FOR NO KEY UPDATE`, f.session); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err = reconcileSessionLifecycle(ctx, f.pool, sessionLifecyclePosition{}); err != nil {
		t.Fatalf("parked tree was relocked: %v", err)
	}
}

func TestSessionLifecycleCandidateTimeoutDoesNotStopBatch(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	for _, s := range []fixture{f, peer} {
		s.enqueue(t, "active")
		if _, err := Dispatch(t.Context(), f.pool, s.execution()); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second'`)
	var first uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions ORDER BY id LIMIT 1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM sessions WHERE id=$1 FOR NO KEY UPDATE`, first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err = reconcileSessionLifecycle(ctx, f.pool, sessionLifecyclePosition{}); err == nil {
		t.Fatal("blocked candidate did not report timeout")
	}
	var advanced bool
	if err = f.pool.QueryRow(t.Context(), `SELECT status='interrupted' FROM turns WHERE session_id<>$1`, first).Scan(&advanced); err != nil || !advanced {
		t.Fatalf("later Session not settled within same batch: %v %v", advanced, err)
	}
}

func TestSessionLifecycleDeadlineDrainsClosingTree(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	active := f.enqueue(t, "active")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, active.TurnID)
	if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []uuid.UUID{f.session, child.session} {
		var closed bool
		if err := f.pool.QueryRow(t.Context(), `SELECT status='closed' FROM sessions WHERE id=$1`, s).Scan(&closed); err != nil || !closed {
			t.Fatalf("closing deadline did not drain %s: %v %v", s, closed, err)
		}
	}
}

func TestSessionLifecycleClosesLargeOwnedTree(t *testing.T) {
	f := newFixture(t)
	active, save := f.finalize(t, "parent")
	const children = 5000
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth)
 SELECT 'until_environment_deletion',$1,gen_random_uuid(),$2,$3,$4,$5,$5,$5,1 FROM generate_series(1,$6)`, f.env, f.agent, f.deployment, f.computer, f.session, children)
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "large-close")); err != nil {
		t.Fatal(err)
	}
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	f.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, active.TurnID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("closed %d owned Sessions in %s", children+1, time.Since(started))
	var closed int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE status='closed'`).Scan(&closed); err != nil || closed != children+1 {
		t.Fatalf("large tree incomplete: %d %v", closed, err)
	}
}
