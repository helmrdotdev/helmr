package session_test

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestExactInterruptRetainsProcessAndRequiresStopAcknowledgement(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	childTurn := child.enqueue(t, "child")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, child.execution()); err != nil {
		t.Fatal(err)
	}
	first := f.enqueue(t, "first")
	next := f.enqueue(t, "next")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	request := controlRequest(f, "turn_interrupt", "stop")
	request.TurnID = first.TurnID
	receipt, err := session.ControlSession(t.Context(), f.pool, f.caller(), request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := session.ControlSession(t.Context(), f.pool, f.caller(), request)
	if err != nil || receipt != retry {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	var status string
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status,(SELECT count(*) FROM session_holds WHERE session_id=$1) FROM session_processes p WHERE session_id=$1`, f.session).Scan(&status, &holds); err != nil || status != "ready" || holds != 0 {
		t.Fatalf("process=%s holds=%d: %v", status, holds, err)
	}
	var childUnchanged bool
	if err := f.pool.QueryRow(t.Context(), `SELECT t.status='running' AND s.authority_generation=1 AND NOT EXISTS(SELECT 1 FROM session_holds WHERE session_id=s.id) FROM turns t JOIN sessions s ON s.id=t.session_id WHERE t.id=$1`, childTurn.TurnID).Scan(&childUnchanged); err != nil || !childUnchanged {
		t.Fatalf("exact interrupt affected child: %v %v", childUnchanged, err)
	}
	e := f.execution()
	e.AuthorityGeneration++
	if _, err := dispatchTurnForTest(t.Context(), f.pool, e); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("premature dispatch: %v", err)
	}
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	control, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || control.StopTurnID == nil || *control.StopTurnID != first.TurnID || control.StopDeadline == nil {
		t.Fatalf("control: %+v %v", control, err)
	}
	assertStopDeadlineMargin(t, f)
	if err := session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence, control.Sequence, control.Generation, control.Kind, "", control.StopSequence+1); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("wrong stop ack: %v", err)
	}
	if err := session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence, control.Sequence, control.Generation, control.Kind, "", control.StopSequence); err != nil {
		t.Fatal(err)
	}
	dispatched, err := dispatchTurnForTest(t.Context(), f.pool, e)
	if err != nil || dispatched.TurnID != next.TurnID {
		t.Fatalf("next: %+v %v", dispatched, err)
	}
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), request); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, next.TurnID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("retry changed successor: %s %v", status, err)
	}
	request.TurnID = next.TurnID
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), request); err != nil {
		t.Fatalf("same key another exact target: %v", err)
	}
}

func TestInterruptStopDeadlineSurvivesResumeAndReattachment(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "first")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	hold, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "hold"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at FROM session_processes WHERE session_id=$1`, f.session).Scan(&before); err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(f, "resume", "resume")
	resume.HoldID = hold.HoldID
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	e := f.execution()
	e.AuthorityGeneration += 2
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at FROM session_processes WHERE session_id=$1`, f.session).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("deadline changed: %v %v %v", before, after, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET stop_deadline_at=clock_timestamp()-interval '1 second' WHERE session_id=$1`, f.session)
	if err := session.ReconcileProcessStopDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var state string
	var failed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status,failure_recorded_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&state, &failed); err != nil || state != "stopping" || !failed {
		t.Fatalf("expired stop: %s %v %v", state, failed, err)
	}
}

func TestExactInterruptRejectsStateAndPreservesOwnedChildren(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	childTurn := child.enqueue(t, "child")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, child.execution()); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "first")
	request := controlRequest(f, "turn_interrupt", "stop")
	request.TurnID = queued.TurnID
	_, err := session.ControlSession(t.Context(), f.pool, f.caller(), request)
	var rejection session.TurnNotInterruptibleError
	if !errors.As(err, &rejection) || rejection.Status != "queued" || rejection.Reason() != "not_started" {
		t.Fatalf("queued rejection: %v", err)
	}
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := closeProcessingForTest(t.Context(), f.pool, f.execution(), queued.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := recordResultForTest(t.Context(), f.pool, f.execution(), queued.TurnID, []byte(`null`), "joined"); err != nil {
		t.Fatal(err)
	}
	_, err = session.ControlSession(t.Context(), f.pool, f.caller(), request)
	if !errors.As(err, &rejection) || rejection.Status != "completed" || rejection.Reason() != "already_terminal" {
		t.Fatalf("finalizing rejection: %v", err)
	}
	var unchanged bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='running' FROM turns WHERE id=$1`, childTurn.TurnID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("child changed: %v %v", unchanged, err)
	}
	var deadline *time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT deadline_at FROM turns WHERE id=$1`, queued.TurnID).Scan(&deadline); err != nil || deadline != nil {
		t.Fatalf("finalization retained execution deadline: %v %v", deadline, err)
	}
}

func TestInterruptSetupRejectsStaleReadyAndRetainsQueue(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "queued")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.session)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "setup"))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("stale ready: %v", err)
	}
	resume := controlRequest(f, "resume", "resume")
	resume.HoldID = receipt.HoldID
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	e := f.execution()
	e.AuthorityGeneration += 2
	control, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || control.Kind != "shutdown" || control.StopTurnID != nil || control.StopDeadline == nil {
		t.Fatalf("setup stop: %+v %v", control, err)
	}
	assertStopDeadlineMargin(t, f)
	if err := session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence, control.Sequence, control.Generation, control.Kind, "", control.StopSequence); err != nil {
		t.Fatalf("shutdown acknowledgement: %v", err)
	}
	var awaitingStop bool
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at IS NOT NULL AND fenced_at IS NULL FROM session_processes WHERE session_id=$1 AND epoch=1`, f.session).Scan(&awaitingStop); err != nil || !awaitingStop {
		t.Fatalf("acknowledgment replaced physical cessation: %v %v", awaitingStop, err)
	}
	if _, err := session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, attachment.Sequence); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("shutdown acknowledgement allowed startup: %v", err)
	}
	view, err := session.GetSession(t.Context(), f.pool, f.caller(), f.env, f.session)
	if err != nil || view.Activity.Process == nil || view.Activity.Process.Stop == nil || view.Activity.Process.Stop.TurnID != nil {
		t.Fatalf("setup observation: %+v %v", view.Activity, err)
	}
}

func assertStopDeadlineMargin(t *testing.T, f fixture) {
	t.Helper()
	var margin bool
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at>clock_timestamp()+interval '30 seconds' AND stop_deadline_at<=clock_timestamp()+$2::bigint*interval '1 millisecond' FROM session_processes WHERE session_id=$1 AND epoch=1`, f.session, session.ProcessStopTimeout.Milliseconds()).Scan(&margin); err != nil || !margin {
		t.Fatalf("stop deadline has no host/guest margin: %v %v", margin, err)
	}
}
