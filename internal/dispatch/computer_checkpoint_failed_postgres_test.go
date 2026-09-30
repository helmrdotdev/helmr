package dispatch_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func failedComputerRequest(r workerapi.RegisterCheckpointRequest) workerapi.CheckpointFailedRequest {
	return workerapi.CheckpointFailedRequest{ComputerInstanceID: r.ComputerInstanceID, WorkerEpoch: r.WorkerEpoch, DesiredVersion: r.DesiredVersion, CheckpointID: r.CheckpointID, Error: "snapshot upload failed"}
}

func TestComputerCheckpointFailureClosesWholeSourceAndReplays(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "shared"
		if idle {
			name = "idle"
		}
		t.Run(name, func(t *testing.T) {
			f, worker, registered := dispatchtest.RegisteredCapture(t, idle)
			request := failedComputerRequest(registered)
			// Reporting failure remains possible after source authority deadlines lapse.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`, request.ComputerInstanceID)
			for i := 0; i < 2; i++ {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 {
					request.Error = "  " + request.Error + "  "
				}
				cp, err := dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request)
				if err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				if cp.Status != "invalid" || cp.InvalidationReasonCode.String != "checkpoint_failed" {
					t.Fatalf("checkpoint=%+v", cp)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var closed bool
			err := f.Pool.QueryRow(t.Context(), `SELECT desired_state='closed' AND desired_version=$2 AND admission_state='closed' AND mount_state='unmounting' AND finalization_action='discard' AND finalization_reason_code='checkpoint_failed' AND reclaimed_at IS NULL AND writer_generation=$3 AND finalization_error->>'message'='snapshot upload failed' FROM computer_instances WHERE id=$1`, request.ComputerInstanceID, request.DesiredVersion+1, registered.Manifest.RecoveryPoint.WriterGeneration).Scan(&closed)
			if err != nil || !closed {
				t.Fatalf("closed=%v err=%v", closed, err)
			}
			var blocked bool
			err = f.Pool.QueryRow(t.Context(), `SELECT c.status='active' AND c.desired_state='stopped' AND c.dirty_state='capture_failed'
 AND c.recovery_failure->>'code'='computer_capture_failed' AND c.recovery_failure->'details'->>'message'='snapshot upload failed'
 AND c.recovery_disk_version_id=c.head_disk_version_id AND c.head_disk_version_id=cp.base_computer_disk_version_id
 FROM computers c JOIN computer_checkpoints cp ON cp.computer_id=c.id WHERE cp.id=$1`, request.CheckpointID).Scan(&blocked)
			if err != nil || !blocked {
				t.Fatalf("capture failure left old head available=%v err=%v", blocked, err)
			}
			if !idle {
				var count int
				err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN runs r ON r.id=m.run_id JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=$1 AND l.status='checkpointing' AND l.process_reconciled_at IS NULL AND l.terminal_at IS NULL AND r.status='waiting' AND r.terminal_at IS NULL AND w.suspension_status='checkpointing'`, request.CheckpointID).Scan(&count)
				if err != nil || count != 2 {
					t.Fatalf("preserved resident members=%d err=%v", count, err)
				}
			}
			request.Error = "different failure"
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("changed replay=%v", err)
			}
		})
	}
}

func TestComputerCheckpointFailureRejectsWrongAuthority(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	for _, tc := range []struct {
		name   string
		mutate func(*workerapi.CheckpointFailedRequest, *dispatch.ComputerCaptureWorker)
	}{
		{"instance", func(r *workerapi.CheckpointFailedRequest, w *dispatch.ComputerCaptureWorker) {
			r.ComputerInstanceID = uuid.NewV7().String()
		}},
		{"checkpoint", func(r *workerapi.CheckpointFailedRequest, w *dispatch.ComputerCaptureWorker) {
			r.CheckpointID = uuid.NewV7().String()
		}},
		{"desired", func(r *workerapi.CheckpointFailedRequest, w *dispatch.ComputerCaptureWorker) { r.DesiredVersion++ }},
		{"epoch", func(r *workerapi.CheckpointFailedRequest, w *dispatch.ComputerCaptureWorker) { r.WorkerEpoch++ }},
		{"host", func(r *workerapi.CheckpointFailedRequest, w *dispatch.ComputerCaptureWorker) {
			w.HostID = pgvalue.UUID(uuid.NewV7())
		}},
		{"group", func(r *workerapi.CheckpointFailedRequest, w *dispatch.ComputerCaptureWorker) {
			w.GroupID = pgvalue.UUID(uuid.NewV7())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w := failedComputerRequest(registered), worker
			tc.mutate(&r, &w)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = dispatch.FailComputerCheckpoint(t.Context(), tx, w, r); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("wrong authority=%v", err)
			}
		})
	}
	for _, message := range []string{"  ", strings.Repeat("x", 1025)} {
		request := failedComputerRequest(registered)
		request.Error = message
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request)
		tx.Rollback(t.Context())
		if !errors.Is(err, dispatch.ErrCheckpointCandidate) {
			t.Fatalf("message validation=%v", err)
		}
	}
}

func TestComputerCheckpointFailureRollbackIsAtomic(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	request := failedComputerRequest(registered)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request); err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	err = f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.failed_request_fingerprint IS NULL AND i.desired_state='ready' AND i.admission_state='checkpointing' AND i.desired_version=$2 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, request.CheckpointID, request.DesiredVersion).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("atomic rollback=%v err=%v", unchanged, err)
	}
}

func TestComputerCheckpointFailureCloseErrorRollsBackReceipt(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	request := failedComputerRequest(registered)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_checkpoint_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.desired_state='closed' THEN RAISE EXCEPTION 'injected close failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_checkpoint_close BEFORE UPDATE ON computer_instances FOR EACH ROW EXECUTE FUNCTION reject_checkpoint_close()`)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request)
	tx.Rollback(t.Context())
	if err == nil {
		t.Fatal("injected close failure accepted")
	}
	var unchanged bool
	err = f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.failed_request_fingerprint IS NULL AND i.desired_state='ready' AND i.admission_state='checkpointing' FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, request.CheckpointID).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("partial failure leaked receipt=%v err=%v", unchanged, err)
	}
}

func TestComputerCheckpointFailureFencesPersistedWorkerAuthority(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	request := failedComputerRequest(registered)
	for _, tc := range []struct{ name, query string }{
		{"epoch", `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`},
		{"host status", `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp() WHERE id=$1`},
		{"group status", `UPDATE worker_groups SET status='disabled' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			dbtest.MustExec(t, t.Context(), tx, tc.query, worker.HostID)
			if _, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("persisted authority accepted=%v", err)
			}
		})
	}
}

func TestComputerCheckpointFailureReleasesCandidateObjects(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, registered); err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	objects, err := q.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(registered.CheckpointID)))
	if err != nil || len(objects) != 4 {
		t.Fatalf("candidate=%d err=%v", len(objects), err)
	}
	for _, object := range objects {
		if _, err = f.Pool.Exec(t.Context(), `UPDATE cas_blobs SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, object.Digest); err == nil {
			t.Fatal("creating checkpoint lost object retention")
		}
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, failedComputerRequest(registered)); err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		n, err := q.RetireAbandonedCasBlob(t.Context(), object.Digest)
		if err != nil || n != 1 {
			t.Fatalf("invalid candidate object retained: n=%d err=%v", n, err)
		}
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, registered); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("invalid candidate reopened: %v", err)
	}
}

func TestComputerCheckpointFailureConcurrentReplay(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	request := failedComputerRequest(registered)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback(t.Context())
			if _, err = dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request); err == nil {
				err = tx.Commit(t.Context())
			}
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var version int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_version FROM computer_instances WHERE id=$1`, request.ComputerInstanceID).Scan(&version); err != nil || version != request.DesiredVersion+1 {
		t.Fatalf("concurrent close version=%d err=%v", version, err)
	}
}

func TestComputerCheckpointFailureSettlesRetryingResidents(t *testing.T) {
	f, worker, registered := dispatchtest.RegisteredCapture(t, false)
	request := failedComputerRequest(registered)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000,retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'`)
	var originalInstances int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_instances`).Scan(&originalInstances); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := dispatch.FailComputerCheckpoint(t.Context(), tx, worker, request); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(uuid.MustParse(request.ComputerInstanceID))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computer.RecordInstanceClosed(t.Context(), f.Pool, computer.Closure{
		Observation: computer.Observation{
			Instance: computer.InstanceRef{
				Host: computer.Host{GroupID: pgvalue.MustUUIDValue(i.WorkerGroupID), HostID: pgvalue.MustUUIDValue(i.WorkerHostID), Epoch: i.WorkerEpoch},
				ID:   pgvalue.MustUUIDValue(i.ID), DesiredVersion: i.DesiredVersion,
			},
			ExpectedObservedVersion: i.ObservedVersion,
		},
		Reason: "checkpoint_failed", CleanupProof: &computer.CleanupProof{Method: computer.CleanupHostReconciled, CompletedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := authority.RecoverRunExecutionLeases(t.Context(), 10); err != nil || n != 2 {
		t.Fatalf("resident recovery=%d err=%v", n, err)
	}
	var retries int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE status='retry_delayed' AND current_run_lease_id IS NULL`).Scan(&retries); err != nil || retries != 2 {
		t.Fatalf("retry candidates=%d err=%v", retries, err)
	}
	for range 2 {
		if _, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var settled int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE status='system_failed' AND current_run_lease_id IS NULL AND failure->>'code'='computer_source_unavailable'`).Scan(&settled); err != nil || settled != 2 {
		t.Fatalf("settled residents=%d err=%v", settled, err)
	}
	var replacements int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_instances`).Scan(&replacements); err != nil || replacements != originalInstances {
		t.Fatalf("stale-head replacements=%d err=%v", replacements, err)
	}
}
