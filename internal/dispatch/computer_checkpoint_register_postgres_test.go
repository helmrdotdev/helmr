package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/jackc/pgx/v5"

	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestComputerCheckpointRegistrationWholeSetAndReplay(t *testing.T) {
	f, worker, request := dispatchtest.RegisteredCapture(t, false)
	for i := 0; i < 2; i++ {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			request.Manifest.RecoveryPoint.Runs[0], request.Manifest.RecoveryPoint.Runs[1] = request.Manifest.RecoveryPoint.Runs[1], request.Manifest.RecoveryPoint.Runs[0]
			request.Manifest.Phases = []workerapi.CheckpointPhase{{Name: "upload", DurationMs: 99}}
		}
		cp, err := dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, request)
		if err != nil {
			_ = tx.Rollback(t.Context())
			t.Fatal(err)
		}
		if cp.Status != "creating" || cp.PrivateComputerDiskVersionID.Valid {
			t.Fatal("registration made checkpoint ready")
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var objects, artifacts int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1),(SELECT count(*) FROM computer_checkpoints WHERE id=$1 AND vm_config_artifact_id IS NOT NULL)`, request.CheckpointID).Scan(&objects, &artifacts); err != nil || objects != 4 || artifacts != 0 {
		t.Fatalf("objects=%d artifacts=%d err=%v", objects, artifacts, err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	request.Manifest.RecoveryPoint.Runs[0].CorrelationID = uuid.NewV7().String()
	if _, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, request); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("changed replay: %v", err)
	}
}

func TestComputerCheckpointRegistrationRejectsChangedCandidate(t *testing.T) {
	f, worker, original := dispatchtest.RegisteredCapture(t, false)
	encoded, _ := json.Marshal(original)
	for _, name := range []string{"instance", "epoch", "desired", "computer", "writer", "membership", "program", "spec", "missing-member", "duplicate-member", "member-lease", "member-wait", "platform", "cpu", "cpu-config", "disk", "disk-root", "missing-memory", "multiple-memory", "duplicate-object", "object-size", "config"} {
		t.Run(name, func(t *testing.T) {
			var request workerapi.RegisterCheckpointRequest
			if err := json.Unmarshal(encoded, &request); err != nil {
				t.Fatal(err)
			}
			point := &request.Manifest.RecoveryPoint
			switch name {
			case "instance":
				request.ComputerInstanceID = uuid.NewV7().String()
			case "epoch":
				request.WorkerEpoch++
			case "desired":
				request.DesiredVersion++
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
				request.Manifest.RuntimeState.Computer.Root.Pack.Digest = "invalid"
			case "missing-memory":
				request.Manifest.RuntimeState.MemoryArtifacts = nil
			case "multiple-memory":
				request.Manifest.RuntimeState.MemoryArtifacts = append(request.Manifest.RuntimeState.MemoryArtifacts, request.Manifest.RuntimeState.MemoryArtifacts[0])
			case "disk":
				request.Manifest.RuntimeState.Computer.LogicalBytes++
			case "duplicate-object":
				request.Manifest.RuntimeState.VMStateArtifact.Digest = request.Manifest.RuntimeState.ConfigArtifact.Digest
			case "object-size":
				request.Manifest.RuntimeState.ConfigArtifact.SizeBytes = 0
			case "config":
				request.Manifest.RuntimeState.Config = nil
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, request); err == nil {
				t.Fatal("changed candidate accepted")
			}
		})
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1`, original.CheckpointID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected candidate retained objects: %d %v", count, err)
	}
}

func TestComputerCheckpointRegistrationRollsBackPartialObjectSet(t *testing.T) {
	f, worker, request := dispatchtest.RegisteredCapture(t, false)
	// A globally retained digest with a different immutable size must reject the
	// entire candidate, including objects registered earlier in the same call.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,999)`, request.Manifest.RuntimeState.MemoryArtifacts[0].Digest)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, request)
	_ = tx.Rollback(t.Context())
	if err == nil {
		t.Fatal("conflicting retained object accepted")
	}
	var objects int
	var hasManifest bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT manifest IS NOT NULL,(SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1) FROM computer_checkpoints WHERE id=$1`, request.CheckpointID).Scan(&hasManifest, &objects); err != nil || hasManifest || objects != 0 {
		t.Fatalf("partial registration: manifest=%v objects=%d err=%v", hasManifest, objects, err)
	}
}

func TestComputerCheckpointRegistrationIdleComputer(t *testing.T) {
	f, worker, request := dispatchtest.RegisteredCapture(t, true)
	if request.Manifest.RecoveryPoint.Runs == nil || len(request.Manifest.RecoveryPoint.Runs) != 0 {
		t.Fatal("idle fixture has members")
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, request); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestComputerCheckpointRegistrationRechecksExpiryAfterObjectLock(t *testing.T) {
	for _, expiry := range []string{"writer", "member"} {
		t.Run(expiry, func(t *testing.T) {
			f, worker, request := dispatchtest.RegisteredCapture(t, false)
			hold, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(t.Context())
			dbtest.MustExec(t, t.Context(), hold, `LOCK TABLE computer_checkpoint_objects IN SHARE MODE`)
			expiryQuery := `SELECT writer_expires_at<clock_timestamp() FROM computer_instances WHERE id=$1`
			id := request.ComputerInstanceID
			if expiry == "writer" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
			} else {
				id = request.Manifest.RecoveryPoint.Runs[0].RunLeaseID
				expiryQuery = `SELECT expires_at<clock_timestamp() FROM run_leases WHERE id=$1`
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp(),expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			pidCh, done := make(chan int32, 1), make(chan error, 1)
			go func() {
				tx, e := f.Pool.Begin(ctx)
				if e != nil {
					done <- e
					return
				}
				defer tx.Rollback(context.Background())
				var pid int32
				if e = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
					done <- e
					return
				}
				pidCh <- pid
				_, e = dispatch.RegisterComputerCheckpoint(ctx, tx, worker, request)
				done <- e
			}()
			var pid int32
			select {
			case pid = <-pidCh:
			case e := <-done:
				t.Fatal(e)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for {
				var blocked, expired bool
				if err = f.Pool.QueryRow(ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&blocked); err != nil {
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
					t.Fatalf("registration did not wait: %v", e)
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
				if !errors.Is(e, pgx.ErrNoRows) {
					t.Fatalf("expired source admitted: %v", e)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var count int
			if err = f.Pool.QueryRow(ctx, `SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=$1`, request.CheckpointID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("expired candidate retained: %d %v", count, err)
			}
		})
	}
}
