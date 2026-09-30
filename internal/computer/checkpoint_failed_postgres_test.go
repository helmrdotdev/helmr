package computer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const checkpointFailure = "snapshot upload failed"

func TestCheckpointFailureClosesWholeSourceAndReplays(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "shared"
		if idle {
			name = "idle"
		}
		t.Run(name, func(t *testing.T) {
			f, ref, manifest := computertest.RegisteredCapture(t, idle)
			// Reporting failure remains possible after source authority deadlines lapse.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, ref.InstanceID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`, ref.InstanceID)
			for _, message := range []string{checkpointFailure, "  " + checkpointFailure + "  "} {
				cp, err := computer.FailCheckpoint(t.Context(), f.Pool, ref, message)
				if err != nil {
					t.Fatal(err)
				}
				if cp.Status != "invalid" || cp.InvalidationReasonCode.String != "checkpoint_failed" {
					t.Fatalf("checkpoint=%+v", cp)
				}
			}
			var closed bool
			err := f.Pool.QueryRow(t.Context(), `SELECT desired_state='closed' AND desired_version=$2 AND admission_state='closed' AND mount_state='unmounting' AND finalization_action='discard' AND finalization_reason_code='checkpoint_failed' AND reclaimed_at IS NULL AND writer_generation=$3 AND finalization_error->>'message'='snapshot upload failed' FROM computer_instances WHERE id=$1`, ref.InstanceID, ref.DesiredVersion+1, manifest.RecoveryPoint.WriterGeneration).Scan(&closed)
			if err != nil || !closed {
				t.Fatalf("closed=%v err=%v", closed, err)
			}
			var blocked bool
			err = f.Pool.QueryRow(t.Context(), `SELECT c.status='active' AND c.desired_state='stopped' AND c.dirty_state='capture_failed'
 AND c.recovery_failure->>'code'='computer_capture_failed' AND c.recovery_failure->'details'->>'message'='snapshot upload failed'
 AND c.recovery_disk_version_id=c.head_disk_version_id AND c.head_disk_version_id=cp.base_computer_disk_version_id
 FROM computers c JOIN computer_checkpoints cp ON cp.computer_id=c.id WHERE cp.id=$1`, ref.CheckpointID).Scan(&blocked)
			if err != nil || !blocked {
				t.Fatalf("capture failure left old head available=%v err=%v", blocked, err)
			}
			if !idle {
				var count int
				err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN runs r ON r.id=m.run_id JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=$1 AND l.status='checkpointing' AND l.process_reconciled_at IS NULL AND l.terminal_at IS NULL AND r.status='waiting' AND r.terminal_at IS NULL AND w.suspension_status='checkpointing'`, ref.CheckpointID).Scan(&count)
				if err != nil || count != 2 {
					t.Fatalf("preserved resident members=%d err=%v", count, err)
				}
			}
			if _, err = computer.FailCheckpoint(t.Context(), f.Pool, ref, "different failure"); !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("changed replay=%v", err)
			}
		})
	}
}

func TestCheckpointFailureRejectsWrongAuthority(t *testing.T) {
	f, registered, _ := computertest.RegisteredCapture(t, false)
	for _, tc := range []struct {
		name   string
		mutate func(*computer.CheckpointRef)
	}{
		{"instance", func(r *computer.CheckpointRef) { r.InstanceID = uuid.NewV7() }},
		{"checkpoint", func(r *computer.CheckpointRef) { r.CheckpointID = uuid.NewV7() }},
		{"desired", func(r *computer.CheckpointRef) { r.DesiredVersion++ }},
		{"epoch", func(r *computer.CheckpointRef) { r.WorkerEpoch++ }},
		{"host", func(r *computer.CheckpointRef) { r.Host.HostID = uuid.NewV7() }},
		{"group", func(r *computer.CheckpointRef) { r.Host.GroupID = uuid.NewV7() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := registered
			tc.mutate(&ref)
			if _, err := computer.FailCheckpoint(t.Context(), f.Pool, ref, checkpointFailure); !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("wrong authority=%v", err)
			}
		})
	}
	for _, message := range []string{"  ", strings.Repeat("x", 1025)} {
		if _, err := computer.FailCheckpoint(t.Context(), f.Pool, registered, message); !errors.Is(err, computer.ErrCheckpointCandidate) {
			t.Fatalf("message validation=%v", err)
		}
	}
}

var errInjectedCommit = errors.New("injected commit failure")

// commitFailingDB begins real transactions whose commit fails after every
// write succeeded; the owner's transaction then rolls them back.
type commitFailingDB struct{ pool *pgxpool.Pool }

func (d commitFailingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return commitFailingTx{tx}, nil
}

type commitFailingTx struct{ pgx.Tx }

func (commitFailingTx) Commit(context.Context) error { return errInjectedCommit }

// A failure report whose writes all succeed but whose commit fails leaves no
// receipt and no change to the Instance's admission or desired state.
func TestCheckpointFailureRollbackIsAtomic(t *testing.T) {
	f, ref, _ := computertest.RegisteredCapture(t, false)
	if _, err := computer.FailCheckpoint(t.Context(), commitFailingDB{f.Pool}, ref, checkpointFailure); !errors.Is(err, errInjectedCommit) {
		t.Fatalf("failure with failed commit = %v", err)
	}
	var unchanged bool
	err := f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.failed_request_fingerprint IS NULL AND i.desired_state='ready' AND i.admission_state='checkpointing' AND i.desired_version=$2 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, ref.CheckpointID, ref.DesiredVersion).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("atomic rollback=%v err=%v", unchanged, err)
	}
	// The same report then commits: the rolled-back attempt left no partial receipt.
	if _, err = computer.FailCheckpoint(t.Context(), f.Pool, ref, checkpointFailure); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointFailureCloseErrorRollsBackReceipt(t *testing.T) {
	f, ref, _ := computertest.RegisteredCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_checkpoint_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.desired_state='closed' THEN RAISE EXCEPTION 'injected close failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_checkpoint_close BEFORE UPDATE ON computer_instances FOR EACH ROW EXECUTE FUNCTION reject_checkpoint_close()`)
	if _, err := computer.FailCheckpoint(t.Context(), f.Pool, ref, checkpointFailure); err == nil {
		t.Fatal("injected close failure accepted")
	}
	var unchanged bool
	err := f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.failed_request_fingerprint IS NULL AND i.desired_state='ready' AND i.admission_state='checkpointing' AND i.desired_version=$2 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, ref.CheckpointID, ref.DesiredVersion).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("partial failure leaked receipt=%v err=%v", unchanged, err)
	}
}

func TestCheckpointFailureFencesPersistedWorkerAuthority(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"epoch", `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`},
		{"host status", `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp() WHERE id=$1`},
		{"group status", `UPDATE worker_groups SET status='disabled' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ref, _ := computertest.RegisteredCapture(t, false)
			dbtest.MustExec(t, t.Context(), f.Pool, tc.query, ref.Host.HostID)
			if _, err := computer.FailCheckpoint(t.Context(), f.Pool, ref, checkpointFailure); !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("persisted authority accepted=%v", err)
			}
		})
	}
}

func TestCheckpointFailureReleasesCandidateObjects(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, false)
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	objects, err := q.ListCheckpointObjects(t.Context(), pgvalue.UUID(ref.CheckpointID))
	if err != nil || len(objects) != 4 {
		t.Fatalf("candidate=%d err=%v", len(objects), err)
	}
	for _, object := range objects {
		if _, err = f.Pool.Exec(t.Context(), `UPDATE cas_blobs SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, object.Digest); err == nil {
			t.Fatal("creating checkpoint lost object retention")
		}
	}
	if _, err = computer.FailCheckpoint(t.Context(), f.Pool, ref, checkpointFailure); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		n, err := q.RetireAbandonedCasBlob(t.Context(), object.Digest)
		if err != nil || n != 1 {
			t.Fatalf("invalid candidate object retained: n=%d err=%v", n, err)
		}
	}
	if _, err = computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("invalid candidate reopened: %v", err)
	}
}

func TestCheckpointFailureConcurrentReplay(t *testing.T) {
	f, ref, _ := computertest.RegisteredCapture(t, false)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := computer.FailCheckpoint(t.Context(), f.Pool, ref, checkpointFailure)
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
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_version FROM computer_instances WHERE id=$1`, ref.InstanceID).Scan(&version); err != nil || version != ref.DesiredVersion+1 {
		t.Fatalf("concurrent close version=%d err=%v", version, err)
	}
}
