package controlplane

import (
	"crypto/sha256"
	"io"
	"log/slog"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestAbandonedSaveRetainsObjectsForLaterCheckpoint(t *testing.T) {
	f, member, worker, request := instanceSaveFixture(t)
	s := &Server{db: db.New(f.Pool), tx: f.Pool}
	if _, err := s.withComputerSave(t.Context(), worker, request, computerSaveBegin, nil); err != nil {
		t.Fatal(err)
	}
	instance, err := s.db.GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: pgvalue.UUID(uuid.MustParse(request.ComputerInstanceID)), EnvironmentID: pgvalue.UUID(f.EnvironmentID)})
	if err != nil {
		t.Fatal(err)
	}
	key := pgvalue.NewUUIDv7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key, instance.EnvironmentID, instance.ComputerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, instance.ID, key)
	inspection := blockformat.ObjectInspection{Segment: &blockformat.Ref{Digest: sha256.Sum256([]byte("abandoned save candidate")), Key: pgvalue.UUIDString(key), Kind: blockformat.SegmentKind, Count: 1, Size: 64}}
	if err = s.recordComputerSaveObject(t.Context(), worker, request, inspection, "register"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.abandonComputerSave(t.Context(), worker, request); err != nil {
			t.Fatal(err)
		}
	}
	store := &computerGraphReclaimStore{t: t, q: s.db}
	collector, err := computer.NewRetention(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.calls != 0 {
		t.Fatal("abandoned save ciphertext was retired while its VM can reuse it")
	}
	// Finish the resident process, then use the ordinary whole-instance capture
	// admission and publication owners for the same ciphertext.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE id=$1`, member.LeaseID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := dispatch.BeginComputerCapture(t.Context(), tx, db.BeginComputerCheckpointParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, WriterGeneration: instance.WriterGeneration, MembershipRevision: instance.MembershipRevision, DesiredVersion: instance.DesiredVersion, CheckpointID: pgvalue.NewUUIDv7()})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, registered := dispatchtest.CaptureRequest(t, f, cp)
	if _, err = s.registerCheckpoint(t.Context(), worker, registered); err != nil {
		t.Fatal(err)
	}
	capture := workerapi.CheckpointComputerObjectRequest{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: worker.Epoch, DesiredVersion: instance.DesiredVersion + 1, CheckpointID: pgvalue.UUIDString(cp.ID), Inspection: inspection}
	if err = s.recordCheckpointComputerObject(t.Context(), worker, capture, nil, "register"); err != nil {
		t.Fatalf("later checkpoint could not register the retained bytes: %v", err)
	}
	// Logical closure alone cannot release either publication's retention.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='fixture',admission_state='closed',mount_state='lost' WHERE id=$1`, instance.ID)
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.calls != 0 {
		t.Fatal("logical closure released live ciphertext")
	}
	// The fixture supplies physical exclusion; the production collector then owns
	// pin release and immutable-byte retirement.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='fixture' WHERE id=$1`, cp.ID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, instance.ID)
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retired bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_object_pins WHERE digest=$1) FROM cas_blobs WHERE digest=$1`, objectDigest(inspection.Segment.Digest)).Scan(&retired); err != nil || !retired {
		t.Fatalf("reclaimed candidate still retained: %v %v", retired, err)
	}
}
