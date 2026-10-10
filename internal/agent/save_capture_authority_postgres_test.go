package agent

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
	"uuid"
)

func TestSaveCaptureRequiresOriginalLiveWriter(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "capture")
	root, _ := s.cut(t, 7)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts
 SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-capture-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.worker, other)
	ref := s.ref(save.ID)
	foreign := ref
	foreign.Host.HostID = other
	if err := s.publisher.Capture(t.Context(), foreign, root, "foreign"); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign capture: %v", err)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, save.ID).Scan(&state); err != nil || state != "requested" {
		t.Fatalf("foreign capture changed request: %s %v", state, err)
	}
	if err := s.publisher.Capture(t.Context(), ref, root, "owned cut"); err != nil {
		t.Fatal(err)
	}
	if err := s.publisher.Capture(t.Context(), ref, root, "same owned cut"); err != nil {
		t.Fatal(err)
	}
	changed, _ := s.cut(t, 8)
	if err := s.publisher.Capture(t.Context(), ref, changed, "new cut"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed capture: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.env)
	if err := s.publisher.Capture(t.Context(), ref, root, "expired"); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired writer: %v", err)
	}
}

func TestPublishedSaveCompletesWithoutSourceWorker(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, save := f.finalize(t, "lost-reply")
	root, _ := s.cut(t, 8)
	if err := s.publisher.Capture(t.Context(), s.ref(save.ID), root, "owned cut"); err != nil {
		t.Fatal(err)
	}
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.env)
	if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var completed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='completed' AND completion_save_id=$2 FROM turns WHERE id=$1`, a.TurnID, save.ID).Scan(&completed); err != nil || !completed {
		t.Fatalf("own-save completion after source loss: %v %v", completed, err)
	}
}

func TestSaveDiscoveryDoesNotTakeCheckpointCut(t *testing.T) {
	f := newFixture(t)
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}
	checkpoint, _ := beginCapture(t, f, f.captureRequest())
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), identity, true); err != nil || save != nil {
		t.Fatalf("checkpoint cut entered live capture: %+v %v", save, err)
	}
	// Even an older checkpoint blocks a later unlinked disk request.
	optional := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,2)`, f.env, optional, f.computer)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET next_save_seq=3 WHERE environment_id=$1 AND id=$2`, f.env, f.computer)
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), identity, true); err != nil || save != nil {
		t.Fatalf("later cut passed checkpoint: %+v %v", save, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='checkpoint cancelled before capture' WHERE environment_id=$1 AND id=$2`, f.env, checkpoint.SaveID)
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), identity, false); err != nil || save == nil || save.ID != optional {
		t.Fatalf("unlinked disk cut missing: %+v %v", save, err)
	}
}
