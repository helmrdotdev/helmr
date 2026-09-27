package dispatch_test

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/jackc/pgx/v5"

	"errors"

	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestComputerCheckpointReadyWholeSet(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "shared"
		if idle {
			name = "idle"
		}
		t.Run(name, func(t *testing.T) {
			f, worker, r, uploaded := dispatchtest.ReadyCapture(t, idle)
			for range 2 {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				cp, err := dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, uploaded)
				if err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				if cp.Status != "ready" || !cp.PrivateComputerDiskVersionID.Valid {
					t.Fatalf("checkpoint=%+v", cp)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			roles := []struct {
				column, kind string
				descriptor   workerapi.CheckpointArtifact
			}{
				{"vm_config_artifact_id", "computer_checkpoint_vm_config", r.Manifest.RuntimeState.ConfigArtifact},
				{"vm_state_artifact_id", "computer_checkpoint_vm_state", r.Manifest.RuntimeState.VMStateArtifact},
				{"memory_artifact_id", "computer_checkpoint_memory", r.Manifest.RuntimeState.MemoryArtifacts[0]},
				{"scratch_disk_artifact_id", "computer_checkpoint_scratch_disk", r.Manifest.RuntimeState.ScratchDiskArtifact},
			}
			for _, role := range roles {
				var exact bool
				err := f.Pool.QueryRow(t.Context(), `SELECT a.org_id=i.org_id AND a.project_id=i.project_id AND a.environment_id=i.environment_id AND a.kind=$2 AND a.digest=$3 AND a.size_bytes=$4 AND a.media_type=$5 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id JOIN artifacts a ON a.id=c.`+role.column+` WHERE c.id=$1`, r.CheckpointID, role.kind, role.descriptor.Digest, role.descriptor.SizeBytes, role.descriptor.MediaType).Scan(&exact)
				if err != nil || !exact {
					t.Fatalf("artifact %s exact=%v err=%v", role.kind, exact, err)
				}
			}
			var retained bool
			err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='closed' AND i.desired_version=$2 AND i.admission_state='closed' AND i.reclaimed_at IS NULL AND c.head_disk_version_id=cp.base_computer_disk_version_id AND cp.private_computer_disk_version_id<>c.head_disk_version_id FROM computer_instances i JOIN computers c ON c.id=i.computer_id JOIN computer_checkpoints cp ON cp.id=i.capture_checkpoint_id WHERE i.id=$1`, r.ComputerInstanceID, r.DesiredVersion+1).Scan(&retained)
			if err != nil || !retained {
				t.Fatalf("retained=%v err=%v", retained, err)
			}
			if !idle {
				var count int
				err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN run_waits w ON w.id=m.run_wait_id JOIN runs r ON r.id=m.run_id WHERE m.checkpoint_id=$1 AND l.status='checkpointed' AND l.process_reconciled_at IS NULL AND w.suspension_status='parked' AND w.current_run_lease_id IS NULL AND w.prior_run_lease_id=l.id AND r.active_started_at IS NULL AND r.current_run_lease_id IS NULL`, r.CheckpointID).Scan(&count)
				if err != nil || count != 2 {
					t.Fatalf("parked members=%d err=%v", count, err)
				}
			}
		})
	}
}

func TestComputerCheckpointReadyRejectsUnprovedObjects(t *testing.T) {
	f, worker, r, uploaded := dispatchtest.ReadyCapture(t, false)
	for _, tc := range []struct {
		name   string
		change func([]cas.Object) []cas.Object
	}{
		{"missing", func(x []cas.Object) []cas.Object { return x[:3] }},
		{"size", func(x []cas.Object) []cas.Object { x[0].SizeBytes++; return x }},
		{"media", func(x []cas.Object) []cas.Object { x[0].MediaType = "text/plain"; return x }},
		{"duplicate", func(x []cas.Object) []cas.Object { x[1] = x[0]; return x }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, tc.change(append([]cas.Object{}, uploaded...))); err == nil {
				t.Fatal("unproved artifacts accepted")
			}
		})
	}
	for _, query := range []string{
		`DELETE FROM computer_object_pins WHERE computer_instance_id=$1`,
		`UPDATE computer_object_pins SET publication_key=decode(repeat('00',32),'hex') WHERE computer_instance_id=$1`,
		`UPDATE computer_object_pins SET instance_desired_version=instance_desired_version-1 WHERE computer_instance_id=$1`,
	} {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		dbtest.MustExec(t, t.Context(), tx, query, r.ComputerInstanceID)
		_, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, uploaded)
		tx.Rollback(t.Context())
		if err == nil {
			t.Fatal("unretained root accepted")
		}
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='creating' AND private_computer_disk_version_id IS NULL FROM computer_checkpoints WHERE id=$1`, r.CheckpointID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("partial readiness=%v err=%v", unchanged, err)
	}
}

func TestComputerCheckpointReadyPreservesResolvedMember(t *testing.T) {
	f, worker, r, uploaded := dispatchtest.ReadyCapture(t, false)
	resolved := r.Manifest.RecoveryPoint.Runs[0]
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET condition_status='completed',condition_result='{"answer":42}',condition_terminal_at=clock_timestamp() WHERE id=$1`, resolved.RunWaitID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, uploaded); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT suspension_status='resume_pending' AND condition_result='{"answer":42}'::jsonb AND condition_status='completed' FROM run_waits WHERE id=$1`, resolved.RunWaitID).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("condition=%v err=%v", preserved, err)
	}
}

func TestComputerCheckpointReadyRollsBackWholeSet(t *testing.T) {
	f, worker, r, uploaded := dispatchtest.ReadyCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_checkpoint_ready_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.desired_state='closed' THEN RAISE EXCEPTION 'injected close failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_checkpoint_ready_close BEFORE UPDATE ON computer_instances FOR EACH ROW EXECUTE FUNCTION reject_checkpoint_ready_close()`)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, uploaded)
	tx.Rollback(t.Context())
	if err == nil {
		t.Fatal("injected error accepted")
	}
	var intact bool
	err = f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.ready_request_fingerprint IS NULL AND c.private_computer_disk_version_id IS NULL AND i.desired_state='ready' AND NOT EXISTS(SELECT 1 FROM artifacts WHERE environment_id=c.environment_id AND kind::text LIKE 'computer_checkpoint_%') AND (SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=c.id AND l.status='checkpointing' AND w.suspension_status='checkpointing')=2 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, r.CheckpointID).Scan(&intact)
	if err != nil || !intact {
		t.Fatalf("rollback intact=%v err=%v", intact, err)
	}
}

func TestComputerCheckpointReadyAllowsLaterConditionResolution(t *testing.T) {
	f, worker, r, uploaded := dispatchtest.ReadyCapture(t, false)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, uploaded); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	m := r.Manifest.RecoveryPoint.Runs[0]
	var revision int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT expected_run_revision FROM run_waits WHERE id=$1`, m.RunWaitID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	result, err := db.New(f.Pool).CompleteParkedRunWait(t.Context(), db.CompleteParkedRunWaitParams{RunID: pgvalue.UUID(uuid.MustParse(m.RunID)), ID: pgvalue.UUID(uuid.MustParse(m.RunWaitID)), PriorRunLeaseID: pgvalue.UUID(uuid.MustParse(m.RunLeaseID)), SuspendCheckpointID: pgvalue.UUID(uuid.MustParse(r.CheckpointID)), AttemptNumber: m.AttemptNumber, ExpectedRunRevision: revision, ConditionResult: []byte(`{"ready":true}`)})
	if err != nil || result.ConditionStatus != "completed" || result.SuspensionStatus != "resume_pending" {
		t.Fatalf("later resolution=%+v err=%v", result, err)
	}
}

func TestComputerCheckpointReadyRechecksExpiryAfterArtifactLock(t *testing.T) {
	for _, expiry := range []string{"writer", "member"} {
		t.Run(expiry, func(t *testing.T) {
			f, worker, request, uploaded := dispatchtest.ReadyCapture(t, false)
			hold, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(t.Context())
			dbtest.MustExec(t, t.Context(), hold, `LOCK TABLE artifacts IN SHARE MODE`)
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
				_, e = dispatch.CompleteComputerCheckpoint(ctx, tx, worker, request, uploaded)
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
			if err = f.Pool.QueryRow(ctx, `SELECT count(*) FROM computer_checkpoints WHERE id=$1 AND status='ready'`, request.CheckpointID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("expired candidate retained: %d %v", count, err)
			}
		})
	}
}

func TestComputerCheckpointReadyReplayIdentity(t *testing.T) {
	f, worker, r, uploaded := dispatchtest.ReadyCapture(t, false)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, uploaded); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.Manifest.RecoveryPoint.Runs[0], r.Manifest.RecoveryPoint.Runs[1] = r.Manifest.RecoveryPoint.Runs[1], r.Manifest.RecoveryPoint.Runs[0]
	r.Manifest.Phases = []workerapi.CheckpointPhase{{Name: "upload", DurationMs: 100}}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	// A committed receipt requires neither current CAS availability nor live leases.
	cp, err := dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, r, nil)
	if err != nil || cp.Status != "ready" {
		t.Fatalf("exact reordered replay=%+v err=%v", cp, err)
	}
	tx.Rollback(t.Context())
	for _, change := range []func(*workerapi.RegisterCheckpointRequest){func(r *workerapi.RegisterCheckpointRequest) { r.DesiredVersion++ }, func(r *workerapi.RegisterCheckpointRequest) {
		r.Manifest.RuntimeState.Config = []byte(`{"changed":true}`)
	}} {
		changed := r
		change(&changed)
		tx, err = f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, changed, nil)
		tx.Rollback(t.Context())
		if err == nil {
			t.Fatal("changed ready replay accepted")
		}
	}
}

func TestComputerCheckpointReadyRequiresRegisteredCandidate(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unregistered"
		if changed {
			name = "changed registration"
		}
		t.Run(name, func(t *testing.T) {
			f, worker, request, uploaded := dispatchtest.ReadyCapture(t, false)
			if changed {
				request.Manifest.RuntimeState.MemoryArtifacts[0].SizeBytes++
				uploaded[2].SizeBytes++
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET manifest=NULL WHERE id=$1`, request.CheckpointID)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err = dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, request, uploaded); !errors.Is(err, dispatch.ErrCheckpointCandidate) {
				t.Fatalf("candidate error=%v", err)
			}
			var unchanged bool
			if err = tx.QueryRow(t.Context(), `SELECT c.status='creating' AND c.private_computer_disk_version_id IS NULL AND c.vm_config_artifact_id IS NULL AND NOT EXISTS(SELECT 1 FROM artifacts a WHERE a.environment_id=c.environment_id AND a.kind::text LIKE 'computer_checkpoint_%') FROM computer_checkpoints c WHERE c.id=$1`, request.CheckpointID).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("rejected candidate changed storage=%v err=%v", !unchanged, err)
			}
		})
	}
}
