package run_test

import (
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestRestoredWaitAcknowledgementReleasesOnlyResolvedMember(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "still pending", true: "resolved"}[resolved], func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			waitID, lease := restoringTimer(t, f, work)
			if resolved {
				reconciler, err := run.NewTimerWaitReconciler(f.Pool)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := reconciler.ReconcileDue(t.Context(), 10); err != nil || n != 1 {
					t.Fatalf("resolve=%d %v", n, err)
				}
			}
			var revision int64
			var active time.Time
			if err := f.Pool.QueryRow(t.Context(), `SELECT revision,active_started_at FROM runs WHERE id=$1`, work.RunID).Scan(&revision, &active); err != nil {
				t.Fatal(err)
			}
			params := db.AcknowledgeRunWaitResumeParams{WaitID: pgvalue.UUID(waitID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), RunLeaseID: pgvalue.UUID(lease), LeaseSequence: 2}
			wait, err := db.New(f.Pool).AcknowledgeRunWaitResume(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			wantRun, wantWait := "waiting", db.RunWaitStatusHot
			if resolved {
				wantRun = "running"
				wantWait = db.RunWaitStatusReleased
				revision++
			}
			var status string
			var actualRevision int64
			var actualActive time.Time
			if err = f.Pool.QueryRow(t.Context(), `SELECT status,revision,active_started_at FROM runs WHERE id=$1`, work.RunID).Scan(&status, &actualRevision, &actualActive); err != nil {
				t.Fatal(err)
			}
			if status != wantRun || wait.SuspensionStatus != wantWait || actualRevision != revision || wait.ExpectedRunRevision != revision || !actualActive.Equal(active) || wait.PriorRunLeaseID.Valid || wait.SuspendCheckpointID.Valid {
				t.Fatalf("ack mismatch run=%s revision=%d active=%v wait=%+v", status, actualRevision, actualActive, wait)
			}
			if _, err = db.New(f.Pool).AcknowledgeRunWaitResume(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("transition applied twice: %v", err)
			}
		})
	}
}

func TestRestoredWaitAcknowledgementRejectsStaleAuthority(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"run revision", `UPDATE runs SET revision=revision+1 WHERE id=(SELECT run_id FROM run_waits WHERE id=$1)`},
		{"closed instance", `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,admission_state='closed' WHERE id=(SELECT l.computer_instance_id FROM run_waits w JOIN run_leases l ON l.id=w.current_run_lease_id WHERE w.id=$1)`},
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT l.computer_instance_id FROM run_waits w JOIN run_leases l ON l.id=w.current_run_lease_id WHERE w.id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			waitID, lease := restoringTimer(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, tc.sql, waitID)
			_, err := db.New(f.Pool).AcknowledgeRunWaitResume(t.Context(), db.AcknowledgeRunWaitResumeParams{WaitID: pgvalue.UUID(waitID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), RunLeaseID: pgvalue.UUID(lease), LeaseSequence: 2})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale ack=%v", err)
			}
			var state string
			if err = f.Pool.QueryRow(t.Context(), `SELECT suspension_status FROM run_waits WHERE id=$1`, waitID).Scan(&state); err != nil || state != "resuming" {
				t.Fatalf("partial ack=%s %v", state, err)
			}
		})
	}
}

func TestRestoredWaitAcknowledgementAuthenticatedReplay(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	waitID, lease := restoringTimer(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
	var fence run.ExecutionFence
	var checkpoint pgtype.UUID
	err := f.Pool.QueryRow(t.Context(), `SELECT l.id,l.lease_sequence,l.worker_group_id,l.worker_host_id,l.worker_epoch,g.claim_version,h.claim_version,w.suspend_checkpoint_id FROM run_leases l JOIN worker_groups g ON g.id=l.worker_group_id JOIN worker_hosts h ON h.id=l.worker_host_id JOIN run_waits w ON w.current_run_lease_id=l.id WHERE l.id=$1`, lease).Scan(&fence.LeaseID, &fence.LeaseSequence, &fence.WorkerGroupID, &fence.WorkerHostID, &fence.WorkerEpoch, &fence.GroupClaimVersion, &fence.HostClaimVersion, &checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	ack := func(id pgtype.UUID) (db.RunWait, error) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return db.RunWait{}, err
		}
		defer tx.Rollback(t.Context())
		w, err := run.AcknowledgeWaitResume(t.Context(), tx, fence, pgvalue.UUID(waitID), id)
		if err == nil {
			err = tx.Commit(t.Context())
		}
		return w, err
	}
	if _, err = ack(pgvalue.UUID(uuid.NewV7())); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong checkpoint=%v", err)
	}
	first, err := ack(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := ack(checkpoint)
	if err != nil || replay.SuspensionStatus != first.SuspensionStatus || replay.ExpectedRunRevision != first.ExpectedRunRevision {
		t.Fatalf("replay=%+v %v", replay, err)
	}

	fence.LeaseSequence++
	if _, err = ack(checkpoint); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong lease=%v", err)
	}
	fence.LeaseSequence--
	// A valid receipt can expire while waiting for its final row lock.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 second',expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, lease)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT id FROM run_waits WHERE id=$1 FOR UPDATE`, waitID)
	result := make(chan error, 1)
	go func() { _, err := ack(checkpoint); result <- err }()
	deadline := time.Now().Add(time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM run_waits WHERE id=%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("acknowledgement did not reach wait lock")
	}
	for {
		var expired bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT expires_at<=clock_timestamp() FROM run_leases WHERE id=$1`, lease).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired blocked acknowledgement=%v", err)
	}

}
