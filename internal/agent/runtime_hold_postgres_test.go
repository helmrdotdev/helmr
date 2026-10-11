package agent

import (
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
	"uuid"
)

func TestRuntimeHoldDuringSetupIsLocalAndIdempotent(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	queued := f.enqueue(t, "queued")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.session)
	for range 2 {
		if err := RuntimeHold(t.Context(), f.pool, *f.host(), f.execution(), "setup_failed"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var hold uuid.UUID
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("holds %d: %v", count, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND scope='local' AND reason='setup_failed'`, f.session).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, queued.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("queued input %s: %v", status, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE session_id=$1`, f.session).Scan(&status); err != nil || status != "stopping" {
		t.Fatalf("process %s: %v", status, err)
	}
	next := peer.enqueue(t, "peer")
	if dispatch, err := Dispatch(t.Context(), peer.pool, peer.execution()); err != nil || dispatch.TurnID != next.TurnID {
		t.Fatalf("peer stopped: %+v %v", dispatch, err)
	}
	req := controlRequest(f, "resume", "release-setup")
	req.HoldID = hold
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeHold(t.Context(), f.pool, *f.host(), f.execution(), "setup_failed"); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retry recreated hold %d: %v", count, err)
	}
}

func TestRuntimeHoldPreservesRecordedSave(t *testing.T) {
	f := newFixture(t)
	a, save := f.finalize(t, "prepared")
	if err := RuntimeHold(t.Context(), f.pool, *f.host(), f.execution(), "native_convergence_failed"); err != nil {
		t.Fatal(err)
	}
	var turnStatus, saveStatus string
	if err := f.pool.QueryRow(t.Context(), `SELECT t.status,s.status FROM turns t JOIN computer_saves s ON s.turn_id=t.id WHERE t.id=$1 AND s.id=$2`, a.TurnID, save.ID).Scan(&turnStatus, &saveStatus); err != nil || turnStatus != "finalizing" || saveStatus != "requested" {
		t.Fatalf("prepared result %s/%s: %v", turnStatus, saveStatus, err)
	}
}
