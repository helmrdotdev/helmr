package computer_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// completeCheckpoint publishes the candidate with storage holding objects.
func completeCheckpoint(t *testing.T, f runtest.Fixture, ref computer.CheckpointRef, manifest computer.CheckpointManifest, objects computer.ObjectStore) (db.ComputerCheckpoint, error) {
	t.Helper()
	publisher, err := computer.NewPublisher(f.Pool, objects)
	if err != nil {
		t.Fatal(err)
	}
	return publisher.CompleteCheckpoint(t.Context(), ref, manifest)
}

func TestCheckpointReadyWholeSet(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "shared"
		if idle {
			name = "idle"
		}
		t.Run(name, func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, idle)
			for range 2 {
				cp := computertest.Complete(t, f, ref, manifest, objects)
				if cp.Status != "ready" || !cp.PrivateComputerDiskVersionID.Valid {
					t.Fatalf("checkpoint=%+v", cp)
				}
			}
			roles := []struct {
				column, kind string
				descriptor   computer.CheckpointArtifact
			}{
				{"vm_config_artifact_id", "computer_checkpoint_vm_config", manifest.RuntimeState.ConfigArtifact},
				{"vm_state_artifact_id", "computer_checkpoint_vm_state", manifest.RuntimeState.VMStateArtifact},
				{"memory_artifact_id", "computer_checkpoint_memory", manifest.RuntimeState.MemoryArtifacts[0]},
				{"scratch_disk_artifact_id", "computer_checkpoint_scratch_disk", manifest.RuntimeState.ScratchDiskArtifact},
			}
			for _, role := range roles {
				var exact bool
				err := f.Pool.QueryRow(t.Context(), `SELECT a.org_id=i.org_id AND a.project_id=i.project_id AND a.environment_id=i.environment_id AND a.kind=$2 AND a.digest=$3 AND a.size_bytes=$4 AND a.media_type=$5 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id JOIN artifacts a ON a.id=c.`+role.column+` WHERE c.id=$1`, ref.CheckpointID, role.kind, role.descriptor.Digest, role.descriptor.SizeBytes, role.descriptor.MediaType).Scan(&exact)
				if err != nil || !exact {
					t.Fatalf("artifact %s exact=%v err=%v", role.kind, exact, err)
				}
			}
			var retained bool
			err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='closed' AND i.desired_version=$2 AND i.admission_state='closed' AND i.reclaimed_at IS NULL AND c.head_disk_version_id=cp.base_computer_disk_version_id AND cp.private_computer_disk_version_id<>c.head_disk_version_id FROM computer_instances i JOIN computers c ON c.id=i.computer_id JOIN computer_checkpoints cp ON cp.id=i.capture_checkpoint_id WHERE i.id=$1`, ref.InstanceID, ref.DesiredVersion+1).Scan(&retained)
			if err != nil || !retained {
				t.Fatalf("retained=%v err=%v", retained, err)
			}
			if !idle {
				var count int
				err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN run_waits w ON w.id=m.run_wait_id JOIN runs r ON r.id=m.run_id WHERE m.checkpoint_id=$1 AND l.status='checkpointed' AND l.process_reconciled_at IS NULL AND w.suspension_status='parked' AND w.current_run_lease_id IS NULL AND w.prior_run_lease_id=l.id AND r.active_started_at IS NULL AND r.current_run_lease_id IS NULL`, ref.CheckpointID).Scan(&count)
				if err != nil || count != 2 {
					t.Fatalf("parked members=%d err=%v", count, err)
				}
			}
		})
	}
}

func TestCheckpointReadyRejectsUnprovedObjects(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	// Storage that lacks a registered object, including storage holding one
	// object in place of another, cannot prove the candidate; stored bytes that
	// differ from the registered descriptor reject the candidate itself.
	for _, tc := range []struct {
		name   string
		change func(computertest.Objects) computertest.Objects
		want   error
	}{
		{"missing", func(x computertest.Objects) computertest.Objects { return x[:3] }, computer.ErrStorageUnavailable},
		{"size", func(x computertest.Objects) computertest.Objects { x[0].SizeBytes++; return x }, computer.ErrCheckpointCandidate},
		{"media", func(x computertest.Objects) computertest.Objects { x[0].MediaType = "text/plain"; return x }, computer.ErrCheckpointCandidate},
		{"duplicate", func(x computertest.Objects) computertest.Objects { x[1] = x[0]; return x }, computer.ErrStorageUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := completeCheckpoint(t, f, ref, manifest, tc.change(append(computertest.Objects{}, objects...))); !errors.Is(err, tc.want) {
				t.Fatalf("unproved artifacts = %v, want %v", err, tc.want)
			}
		})
	}
	// A candidate naming one object twice never registers, so readiness can
	// never observe duplicate runtime objects.
	duplicate := manifest
	duplicate.RuntimeState.VMStateArtifact = duplicate.RuntimeState.ConfigArtifact
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, duplicate); !errors.Is(err, computer.ErrCheckpointCandidate) {
		t.Fatalf("duplicate runtime object registration = %v", err)
	}
	requireCreating := func(t *testing.T, f runtest.Fixture, ref computer.CheckpointRef) {
		t.Helper()
		var unchanged bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT status='creating' AND private_computer_disk_version_id IS NULL FROM computer_checkpoints WHERE id=$1`, ref.CheckpointID).Scan(&unchanged); err != nil || !unchanged {
			t.Fatalf("partial readiness=%v err=%v", unchanged, err)
		}
	}
	requireCreating(t, f, ref)
	for _, query := range []string{
		`DELETE FROM computer_object_pins WHERE computer_instance_id=$1`,
		`UPDATE computer_object_pins SET publication_key=decode(repeat('00',32),'hex') WHERE computer_instance_id=$1`,
		`UPDATE computer_object_pins SET instance_desired_version=instance_desired_version-1 WHERE computer_instance_id=$1`,
	} {
		t.Run(query, func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, false)
			dbtest.MustExec(t, t.Context(), f.Pool, query, ref.InstanceID)
			if _, err := completeCheckpoint(t, f, ref, manifest, objects); err == nil {
				t.Fatal("unretained root accepted")
			}
			requireCreating(t, f, ref)
		})
	}
}

func TestCheckpointReadyPreservesResolvedMember(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	resolved := manifest.RecoveryPoint.Runs[0]
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET condition_status='completed',condition_result='{"answer":42}',condition_terminal_at=clock_timestamp() WHERE id=$1`, resolved.RunWaitID)
	computertest.Complete(t, f, ref, manifest, objects)
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT suspension_status='resume_pending' AND condition_result='{"answer":42}'::jsonb AND condition_status='completed' FROM run_waits WHERE id=$1`, resolved.RunWaitID).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("condition=%v err=%v", preserved, err)
	}
}

func TestCheckpointReadyRollsBackWholeSet(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_checkpoint_ready_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.desired_state='closed' THEN RAISE EXCEPTION 'injected close failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_checkpoint_ready_close BEFORE UPDATE ON computer_instances FOR EACH ROW EXECUTE FUNCTION reject_checkpoint_ready_close()`)
	if _, err := completeCheckpoint(t, f, ref, manifest, objects); err == nil {
		t.Fatal("injected error accepted")
	}
	var intact bool
	err := f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.ready_request_fingerprint IS NULL AND c.private_computer_disk_version_id IS NULL AND i.desired_state='ready' AND NOT EXISTS(SELECT 1 FROM artifacts WHERE environment_id=c.environment_id AND kind::text LIKE 'computer_checkpoint_%') AND (SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=c.id AND l.status='checkpointing' AND w.suspension_status='checkpointing')=2 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, ref.CheckpointID).Scan(&intact)
	if err != nil || !intact {
		t.Fatalf("rollback intact=%v err=%v", intact, err)
	}
}

func TestCheckpointReadyAllowsLaterConditionResolution(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	computertest.Complete(t, f, ref, manifest, objects)
	m := manifest.RecoveryPoint.Runs[0]
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT expected_run_revision FROM run_waits WHERE id=$1`, m.RunWaitID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	result, err := db.New(f.Pool).CompleteParkedRunWait(t.Context(), db.CompleteParkedRunWaitParams{RunID: pgvalue.UUID(uuid.MustParse(m.RunID)), ID: pgvalue.UUID(uuid.MustParse(m.RunWaitID)), PriorRunLeaseID: pgvalue.UUID(uuid.MustParse(m.RunLeaseID)), SuspendCheckpointID: pgvalue.UUID(ref.CheckpointID), AttemptNumber: m.AttemptNumber, ExpectedRunRevision: revision, ConditionResult: []byte(`{"ready":true}`)})
	if err != nil || result.ConditionStatus != "completed" || result.SuspensionStatus != "resume_pending" {
		t.Fatalf("later resolution=%+v err=%v", result, err)
	}
}

func TestCheckpointReadyRechecksExpiryAfterArtifactLock(t *testing.T) {
	for _, expiry := range []string{"writer", "member"} {
		t.Run(expiry, func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, false)
			expiryQuery := `SELECT writer_expires_at<clock_timestamp() FROM computer_instances WHERE id=$1`
			id := ref.InstanceID.String()
			if expiry == "writer" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
			} else {
				id = manifest.RecoveryPoint.Runs[0].RunLeaseID
				expiryQuery = `SELECT expires_at<clock_timestamp() FROM run_leases WHERE id=$1`
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp(),expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
			}
			publisher, err := computer.NewPublisher(f.Pool, objects)
			if err != nil {
				t.Fatal(err)
			}
			err = holdLockUntilExpired(t, f, `LOCK TABLE artifacts IN SHARE MODE`, expiryQuery, id, func(ctx context.Context) error {
				_, err := publisher.CompleteCheckpoint(ctx, ref, manifest)
				return err
			})
			if !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("expired source admitted: %v", err)
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoints WHERE id=$1 AND status='ready'`, ref.CheckpointID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("expired candidate retained: %d %v", count, err)
			}
		})
	}
}

func TestCheckpointReadyReplayIdentity(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	computertest.Complete(t, f, ref, manifest, objects)
	manifest.RecoveryPoint.Runs[0], manifest.RecoveryPoint.Runs[1] = manifest.RecoveryPoint.Runs[1], manifest.RecoveryPoint.Runs[0]
	manifest.Phases = []computer.CheckpointPhase{{Name: "upload", DurationMs: 100}}
	// A committed receipt requires neither current storage availability nor live leases.
	cp, err := completeCheckpoint(t, f, ref, manifest, unavailableObjects{})
	if err != nil || cp.Status != "ready" {
		t.Fatalf("exact reordered replay=%+v err=%v", cp, err)
	}
	changedVersion := ref
	changedVersion.DesiredVersion++
	if _, err = completeCheckpoint(t, f, changedVersion, manifest, unavailableObjects{}); err == nil {
		t.Fatal("changed ready replay accepted")
	}
	changed := manifest
	changed.RuntimeState.Config = []byte(`{"changed":true}`)
	if _, err = completeCheckpoint(t, f, ref, changed, unavailableObjects{}); err == nil {
		t.Fatal("changed ready replay accepted")
	}
}

func TestCheckpointReadyRequiresRegisteredCandidate(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unregistered"
		if changed {
			name = "changed registration"
		}
		t.Run(name, func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, false)
			if changed {
				manifest.RuntimeState.MemoryArtifacts[0].SizeBytes++
				objects[2].SizeBytes++
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET manifest=NULL WHERE id=$1`, ref.CheckpointID)
			}
			if _, err := completeCheckpoint(t, f, ref, manifest, objects); !errors.Is(err, computer.ErrCheckpointCandidate) {
				t.Fatalf("candidate error=%v", err)
			}
			var unchanged bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.private_computer_disk_version_id IS NULL AND c.vm_config_artifact_id IS NULL AND NOT EXISTS(SELECT 1 FROM artifacts a WHERE a.environment_id=c.environment_id AND a.kind::text LIKE 'computer_checkpoint_%') FROM computer_checkpoints c WHERE c.id=$1`, ref.CheckpointID).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("rejected candidate changed storage=%v err=%v", !unchanged, err)
			}
		})
	}
}

// Readiness reads each registered runtime object from storage outside its
// transactions and reports a storage failure as ErrStorageUnavailable.
func TestCheckpointReadyReportsUnavailableStorage(t *testing.T) {
	f, ref, manifest, _ := computertest.ReadyCapture(t, false)
	if _, err := completeCheckpoint(t, f, ref, manifest, unavailableObjects{}); !errors.Is(err, computer.ErrStorageUnavailable) {
		t.Fatalf("readiness with unavailable storage: %v", err)
	}
	var creating bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='creating' FROM computer_checkpoints WHERE id=$1`, ref.CheckpointID).Scan(&creating); err != nil || !creating {
		t.Fatalf("unavailable storage changed checkpoint=%v err=%v", !creating, err)
	}
}
