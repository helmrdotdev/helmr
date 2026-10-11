package agent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRuntimeFinalizationReconcilesOriginalSaveAfterGrantChange(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a := f.enqueue(t, "finalize")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"answer":42}`)
	if outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, result, "drained"); err != nil || outcome != nil {
		t.Fatalf("prepared result reported terminal: %s %v", outcome, err)
	}
	var save SaveRequest
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM computer_saves WHERE turn_id=$1`, a.TurnID).Scan(&save.ID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.session)
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatalf("close replay: %v", err)
	}
	for range 2 {
		if outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, result, "drained"); err != nil || outcome != nil {
			t.Fatalf("pending replay: %s %v", outcome, err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_saves WHERE turn_id=$1`, a.TurnID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("save identity replaced: %d %v", count, err)
	}
	if _, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`{"answer":43}`), "drained"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed result: %v", err)
	}
	root, _ := s.cut(t, 8)
	if err := s.publisher.Capture(t.Context(), s.ref(save.ID), root, "owned cut"); err != nil {
		t.Fatal(err)
	}
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, result, "drained")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Status string
		Result map[string]int
	}
	if err := json.Unmarshal(outcome, &parsed); err != nil || parsed.Status != "completed" || parsed.Result["answer"] != 42 {
		t.Fatalf("terminal outcome: %s %v", outcome, err)
	}
	foreign := f.execution()
	foreign.ProcessEpoch++
	if _, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), foreign, a.TurnID, result, "drained"); err == nil {
		t.Fatal("other process observed result")
	}
}

func TestRuntimeFinalizationDoesNotPromoteStaleGrantForNewResult(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "stale")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.session)
	if _, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`null`), "drained"); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale grant admitted result: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_saves WHERE turn_id=$1`, a.TurnID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale result created save: %d %v", count, err)
	}
}

func TestRuntimeFinalizationObservesOriginalTurnAfterPhysicalOwnerRestore(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, save := f.finalize(t, "before-restore")
	root, _ := s.cut(t, 8)
	if err := s.publisher.Capture(t.Context(), s.ref(save.ID), root, "owned cut"); err != nil {
		t.Fatal(err)
	}
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	cp := checkpointStorageForSaveFixture(t, s)
	if err := cp.publisher.RegisterCheckpoint(t.Context(), cp.ref, cp.manifest); err != nil {
		t.Fatal(err)
	}
	cp.upload(t)
	if err := cp.publish(t, cp.save, cp.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := cp.publisher.CompleteCheckpoint(t.Context(), cp.ref, cp.manifest); err != nil {
		t.Fatal(err)
	}
	r := restoreCheckpointFixture(t, cp)
	p := r.prepare(t)
	if err := ValidateComputerRestore(t.Context(), f.pool, r.host, f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(t.Context(), f.pool, r.host, f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f, r.host, p)
	if err := CompleteComputerRestore(t.Context(), f.pool, r.host, f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	// HTTP authentication replaces only Host identity, leaving the retained
	// operation's original lease and business generation unchanged.
	original := f.execution()
	original.WorkerHostID = r.host.HostID
	if err := RuntimeCloseProcessing(t.Context(), f.pool, r.host, original, a.TurnID); err != nil {
		t.Fatalf("restored close receipt: %v", err)
	}
	outcome, err := RuntimeFinalize(t.Context(), f.pool, r.host, original, a.TurnID, json.RawMessage(`{"result":1}`), "original native receipt")
	if err != nil || string(outcome) != `{"result":{"result":1},"status":"completed"}` {
		t.Fatalf("restored terminal winner: %s %v", outcome, err)
	}
	if _, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`{"result":1}`), "original native receipt"); err == nil {
		t.Fatal("retired source observed restored process")
	}
}
