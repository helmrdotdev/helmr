package session_test

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRuntimeHoldDuringSetupCountsOnceWithoutHolding(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	queued := f.enqueue(t, "queued")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.session)
	for range 2 {
		if err := session.RuntimeHold(t.Context(), auditpolicy.Policy{
			RetentionSeconds: 3600,
		}, f.pool, *f.host(), f.execution(), "setup_failed"); err != nil {
			t.Fatal(err)
		}
	}
	// A setup failure with no Turn counts once and does not hold yet.
	var count int
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_holds)+(SELECT count(*) FROM session_processes WHERE session_id=$1 AND failure_reason<>'process_failed')`, f.session).Scan(&count); err != nil || count != 0 {
		t.Fatalf("holds or wrong reason %d: %v", count, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='session.process_failed' AND convert_from(data,'UTF8')::jsonb @> '{"reason":"process_failed","holdId":null}'`, f.session).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure events %d: %v", count, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, queued.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("queued input %s: %v", status, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE session_id=$1`, f.session).Scan(&status); err != nil || status != "stopping" {
		t.Fatalf("process %s: %v", status, err)
	}
	next := peer.enqueue(t, "peer")
	if dispatch, err := dispatchTurnForTest(t.Context(), peer.pool, peer.execution()); err != nil || dispatch.TurnID != next.TurnID {
		t.Fatalf("peer stopped: %+v %v", dispatch, err)
	}
	if err := session.RuntimeHold(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), "setup_failed"); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='session.process_failed'`, f.session).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry recounted failure %d: %v", count, err)
	}
}

func TestRuntimeHoldPreservesRecordedSave(t *testing.T) {
	f := newFixture(t)
	a, save := f.finalize(t, "prepared")
	if err := session.RuntimeHold(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), "turn_drain_failed"); err != nil {
		t.Fatal(err)
	}
	var turnStatus, saveStatus string
	if err := f.pool.QueryRow(t.Context(), `SELECT t.status,s.status FROM turns t JOIN computer_saves s ON s.turn_id=t.id WHERE t.id=$1 AND s.id=$2`, a.TurnID, save.ID).Scan(&turnStatus, &saveStatus); err != nil || turnStatus != "completed" || saveStatus != "requested" {
		t.Fatalf("prepared result %s/%s: %v", turnStatus, saveStatus, err)
	}
}
