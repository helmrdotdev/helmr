package computer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestCheckpointRegistrationWholeSetAndReplay(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, false)
	for i := 0; i < 2; i++ {
		if i == 1 {
			manifest.RecoveryPoint.Runs[0], manifest.RecoveryPoint.Runs[1] = manifest.RecoveryPoint.Runs[1], manifest.RecoveryPoint.Runs[0]
			manifest.Phases = []computer.CheckpointPhase{{Name: "upload", DurationMs: 99}}
		}
		cp, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if cp.Status != "creating" || cp.PrivateComputerDiskVersionID.Valid {
			t.Fatal("registration made checkpoint ready")
		}
	}
	var objects, artifacts int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1),(SELECT count(*) FROM computer_checkpoints WHERE id=$1 AND vm_config_artifact_id IS NOT NULL)`, ref.CheckpointID).Scan(&objects, &artifacts); err != nil || objects != 4 || artifacts != 0 {
		t.Fatalf("objects=%d artifacts=%d err=%v", objects, artifacts, err)
	}
	manifest.RecoveryPoint.Runs[0].CorrelationID = uuid.NewV7().String()
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("changed replay: %v", err)
	}
}

func TestCheckpointRegistrationRejectsChangedCandidate(t *testing.T) {
	f, original, registered := computertest.RegisteredCapture(t, false)
	encoded, err := json.Marshal(registered)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"instance", "epoch", "desired", "computer", "writer", "membership", "program", "spec", "missing-member", "duplicate-member", "member-lease", "member-wait", "platform", "cpu", "cpu-config", "disk", "disk-root", "missing-memory", "multiple-memory", "duplicate-object", "object-size", "config"} {
		t.Run(name, func(t *testing.T) {
			ref := original
			var manifest computer.CheckpointManifest
			if err := json.Unmarshal(encoded, &manifest); err != nil {
				t.Fatal(err)
			}
			point := &manifest.RecoveryPoint
			switch name {
			case "instance":
				ref.InstanceID = uuid.NewV7()
			case "epoch":
				ref.WorkerEpoch++
			case "desired":
				ref.DesiredVersion++
			case "computer":
				point.ComputerID = uuid.NewV7().String()
			case "writer":
				point.WriterGeneration++
			case "membership":
				point.MembershipRevision++
			case "program":
				point.ProgramDeploymentID = uuid.NewV7().String()
			case "spec":
				point.ComputerSpecID = uuid.NewV7().String()
			case "missing-member":
				point.Runs = point.Runs[:1]
			case "duplicate-member":
				point.Runs[1] = point.Runs[0]
			case "member-lease":
				point.Runs[0].RunLeaseID = uuid.NewV7().String()
			case "member-wait":
				point.Runs[0].RunWaitID = uuid.NewV7().String()
			case "platform":
				point.Runtime.ID = dbtest.Digest("other")
			case "cpu":
				point.Runtime.VMVCPUCount++
			case "cpu-config":
				point.Runtime.CPUConfigDigest = dbtest.Digest("different-cpu-config")
			case "disk-root":
				manifest.RuntimeState.Computer.Root.Pack.Digest = "invalid"
			case "missing-memory":
				manifest.RuntimeState.MemoryArtifacts = nil
			case "multiple-memory":
				manifest.RuntimeState.MemoryArtifacts = append(manifest.RuntimeState.MemoryArtifacts, manifest.RuntimeState.MemoryArtifacts[0])
			case "disk":
				manifest.RuntimeState.Computer.LogicalBytes++
			case "duplicate-object":
				manifest.RuntimeState.VMStateArtifact.Digest = manifest.RuntimeState.ConfigArtifact.Digest
			case "object-size":
				manifest.RuntimeState.ConfigArtifact.SizeBytes = 0
			case "config":
				manifest.RuntimeState.Config = nil
			}
			if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err == nil {
				t.Fatal("changed candidate accepted")
			}
		})
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1`, original.CheckpointID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected candidate retained objects: %d %v", count, err)
	}
}

func TestCheckpointRegistrationRollsBackPartialObjectSet(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, false)
	// A globally retained digest with a different immutable size must reject the
	// entire candidate, including objects registered earlier in the same call.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,999)`, manifest.RuntimeState.MemoryArtifacts[0].Digest)
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err == nil {
		t.Fatal("conflicting retained object accepted")
	}
	var objects int
	var hasManifest bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT manifest IS NOT NULL,(SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1) FROM computer_checkpoints WHERE id=$1`, ref.CheckpointID).Scan(&hasManifest, &objects); err != nil || hasManifest || objects != 0 {
		t.Fatalf("partial registration: manifest=%v objects=%d err=%v", hasManifest, objects, err)
	}
}

func TestCheckpointRegistrationIdleComputer(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, true)
	if manifest.RecoveryPoint.Runs == nil || len(manifest.RecoveryPoint.Runs) != 0 {
		t.Fatal("idle fixture has members")
	}
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRegistrationRechecksExpiryAfterObjectLock(t *testing.T) {
	for _, expiry := range []string{"writer", "member"} {
		t.Run(expiry, func(t *testing.T) {
			f, ref, manifest := computertest.RegisteredCapture(t, false)
			expiryQuery := `SELECT writer_expires_at<clock_timestamp() FROM computer_instances WHERE id=$1`
			id := ref.InstanceID.String()
			if expiry == "writer" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
			} else {
				id = manifest.RecoveryPoint.Runs[0].RunLeaseID
				expiryQuery = `SELECT expires_at<clock_timestamp() FROM run_leases WHERE id=$1`
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp(),expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
			}
			err := holdLockUntilExpired(t, f, `LOCK TABLE computer_checkpoint_objects IN SHARE MODE`, expiryQuery, id, func(ctx context.Context) error {
				_, err := computer.RegisterCheckpoint(ctx, f.Pool, ref, manifest)
				return err
			})
			if !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("expired source admitted: %v", err)
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1`, ref.CheckpointID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("expired candidate retained: %d %v", count, err)
			}
		})
	}
}

// holdLockUntilExpired takes lock in a holding transaction, runs operation
// until it waits behind the holder and expiryQuery reports the deadline
// passed, then releases the holder and returns the operation's result.
func holdLockUntilExpired(t *testing.T, f runtest.Fixture, lock, expiryQuery, id string, operation func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hold, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(context.Background())
	dbtest.MustExec(t, ctx, hold, lock)
	holder := hold.Conn().PgConn().PID()
	done := make(chan error, 1)
	go func() { done <- operation(ctx) }()
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if err = f.Pool.QueryRow(ctx, expiryQuery, id).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("operation did not wait: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func TestComputerCheckpointRegistrationContinuesOnPausedGroup(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, pauseWorkerGroup, runtest.WorkerGroupID)
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatalf("registration on paused Group: %v", err)
	}
}
