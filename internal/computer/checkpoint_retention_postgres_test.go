package computer_test

import (
	"io"
	"log/slog"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestPublishedCheckpointRetention(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "unshared", true: "shared"}[shared], func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, true)
			computertest.Complete(t, f, ref, manifest, objects)
			store := &retiredBlobStore{t: t, q: db.New(f.Pool)}
			collector, err := computer.NewRetention(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			digest := manifest.RuntimeState.MemoryArtifacts[0].Digest
			if shared {
				// An independent artifact owns the same org membership and bytes.
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type)
 SELECT $2,org_id,project_id,environment_id,digest,'deployment_program',size_bytes,media_type FROM artifacts WHERE id=(SELECT memory_artifact_id FROM computer_checkpoints WHERE id=$1)`, ref.CheckpointID, uuid.NewV7())
			}
			if err = collector.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM artifacts WHERE digest=ANY($1)`, []string{manifest.RuntimeState.ConfigArtifact.Digest, manifest.RuntimeState.VMStateArtifact.Digest, digest, manifest.RuntimeState.ScratchDiskArtifact.Digest}).Scan(&count); err != nil || count != 4+map[bool]int{false: 0, true: 1}[shared] {
				t.Fatalf("ready artifacts=%d: %v", count, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='computer_deleted' WHERE id=$1`, ref.CheckpointID)
			for range 2 {
				if err = collector.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var history, retired bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT status='invalid' AND manifest IS NOT NULL AND ready_request_fingerprint IS NOT NULL FROM computer_checkpoints WHERE id=$1`, ref.CheckpointID).Scan(&history); err != nil || !history {
				t.Fatalf("history=%v: %v", history, err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM artifacts WHERE digest=ANY($1)`, []string{manifest.RuntimeState.ConfigArtifact.Digest, manifest.RuntimeState.VMStateArtifact.Digest, digest, manifest.RuntimeState.ScratchDiskArtifact.Digest}).Scan(&count); err != nil || count != map[bool]int{false: 0, true: 1}[shared] {
				t.Fatalf("invalid artifacts=%d: %v", count, err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL FROM cas_blobs WHERE digest=$1`, digest).Scan(&retired); err != nil || retired == shared {
				t.Fatalf("shared=%v retired=%v: %v", shared, retired, err)
			}
		})
	}
}

func TestCheckpointRetentionKeepsCommittedRestorePayload(t *testing.T) {
	f := newRestorePlanFixture(t, false, true)
	store := &retiredBlobStore{t: t, q: db.New(f.Pool)}
	collector, err := computer.NewRetention(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if plan, err := f.read(t.Context()); err != nil || plan == nil || len(plan.Members) != 2 {
		t.Fatalf("committed restore lost payload: %v", err)
	}
	var retained bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT vm_config_artifact_id IS NOT NULL AND vm_state_artifact_id IS NOT NULL AND memory_artifact_id IS NOT NULL AND scratch_disk_artifact_id IS NOT NULL FROM computer_checkpoints WHERE resume_computer_instance_id=$1`, f.ref.ID).Scan(&retained); err != nil || !retained {
		t.Fatalf("committed artifacts retained=%v: %v", retained, err)
	}
}
