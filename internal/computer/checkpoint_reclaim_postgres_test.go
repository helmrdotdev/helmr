package computer_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCaptureSourceReclaimReleasesCandidatePins(t *testing.T) {
	for _, method := range []string{"worker_cleanup", "provider_absent", "startup_recovery"} {
		for _, aborted := range []bool{false, true} {
			t.Run(method+map[bool]string{false: "/creating", true: "/aborted"}[aborted], func(t *testing.T) {
				f, ref, manifest, key := captureAbortFixture(t, false)
				if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
					t.Fatal(err)
				}
				if aborted {
					if _, err := computer.AbortCapture(t.Context(), f.Pool, key, ref); err != nil {
						t.Fatal(err)
					}
				}
				q := db.New(f.Pool)
				switch method {
				case "worker_cleanup":
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp() WHERE id=$1`, ref.InstanceID)
					if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
						i, err := db.New(tx).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(ref.InstanceID)})
						if err != nil {
							return err
						}
						_, err = computer.ExpireInstance(t.Context(), tx, i)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					// A close intent without physical exclusion cannot release the candidate.
					if rows, err := q.ListAbandonedCasBlobs(t.Context(), 100); err != nil || (!aborted && len(rows) != 0) {
						t.Fatalf("unexcluded candidate=%v %v", rows, err)
					}
					i, err := q.GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(ref.InstanceID)})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = computer.RecordInstanceClosed(t.Context(), f.Pool, computer.Closure{Observation: computer.Observation{Instance: computer.InstanceRef{Host: ref.Host, ID: ref.InstanceID, DesiredVersion: i.DesiredVersion}, ExpectedObservedVersion: i.ObservedVersion}, Reason: "computer_writer_expired", CleanupProof: &computer.CleanupProof{Method: computer.CleanupHostReconciled, CompletedAt: time.Now()}}); err != nil {
						t.Fatal(err)
					}
				case "provider_absent":
					if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
						q := db.New(tx)
						if _, err := q.ConfirmWorkerHostProviderAbsent(t.Context(), pgvalue.UUID(ref.Host.HostID)); err != nil {
							return err
						}
						_, err := q.ReconcileProviderAbsentWorkerInstances(t.Context(), pgvalue.UUID(ref.Host.HostID))
						return err
					}); err != nil {
						t.Fatal(err)
					}
				case "startup_recovery":
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='registering',current_epoch=2,current_service_id=$2,epoch_started_at=now(),activated_at=NULL,vm_platform_id=NULL,epoch_cpu_millis=0,epoch_memory_bytes=0,epoch_guest_ephemeral_disk_bytes=0,per_vm_cpu_millis=0,per_vm_memory_bytes=0,per_vm_guest_ephemeral_disk_bytes=0,max_vm_slots=0,max_vm_starts=0,cpu_environment=NULL,cpu_environment_digest=NULL,observed_at=NULL,run_paused_reason=NULL,vm_paused_reason=NULL WHERE id=$1`, ref.Host.HostID, uuid.NewV7())
					if _, err := q.CompleteWorkerStartupRecovery(t.Context(), db.CompleteWorkerStartupRecoveryParams{WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerEpoch: pgtype.Int8{Int64: 2, Valid: true}, RecoveryEvidence: []byte(`{"observed_at":"2026-10-01T00:00:00Z","quarantined":[]}`)}); err != nil {
						t.Fatal(err)
					}
				}
				var settled bool
				want := map[bool]string{false: "invalid", true: "aborted"}[aborted]
				if err := f.Pool.QueryRow(t.Context(), `SELECT cp.status=$2 AND i.reclaimed_at IS NOT NULL AND (cp.status='aborted' OR cp.invalidation_reason_code='capture_source_reclaimed') FROM computer_checkpoints cp JOIN computer_instances i ON i.id=cp.source_computer_instance_id WHERE cp.id=$1`, ref.CheckpointID, want).Scan(&settled); err != nil || !settled {
					t.Fatalf("candidate settled=%v %v", settled, err)
				}
				rows, err := q.ListAbandonedCasBlobs(t.Context(), 100)
				if err != nil || len(rows) != 4 {
					t.Fatalf("reclaimed objects=%v %v", rows, err)
				}
				for _, row := range rows {
					if n, err := q.RetireAbandonedCasBlob(t.Context(), row); err != nil || n != 1 {
						t.Fatalf("retire=%d %v", n, err)
					}
				}
			})
		}
	}
}

// Deletion holds Computer then checkpoint then Instance. Host-wide reclaim
// must wait at the Computer boundary, before taking the opposing Instance lock.
func TestCaptureReclaimSerializesWithDeletionLocks(t *testing.T) {
	for _, method := range []string{"provider_absent", "startup_recovery"} {
		t.Run(method, func(t *testing.T) {
			f, ref, _, _ := captureAbortFixture(t, true)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			q := db.New(f.Pool)
			queryName := "ReconcileProviderAbsentWorkerInstances"
			reclaim := func() error {
				_, err := q.ReconcileProviderAbsentWorkerInstances(ctx, pgvalue.UUID(ref.Host.HostID))
				return err
			}
			if method == "provider_absent" {
				if _, err := q.ConfirmWorkerHostProviderAbsent(ctx, pgvalue.UUID(ref.Host.HostID)); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, ctx, f.Pool, `UPDATE worker_hosts SET status='registering',current_epoch=2,current_service_id=$2,epoch_started_at=now(),activated_at=NULL,vm_platform_id=NULL,epoch_cpu_millis=0,epoch_memory_bytes=0,epoch_guest_ephemeral_disk_bytes=0,per_vm_cpu_millis=0,per_vm_memory_bytes=0,per_vm_guest_ephemeral_disk_bytes=0,max_vm_slots=0,max_vm_starts=0,cpu_environment=NULL,cpu_environment_digest=NULL,observed_at=NULL,run_paused_reason=NULL,vm_paused_reason=NULL WHERE id=$1`, ref.Host.HostID, uuid.NewV7())
				queryName = "CompleteWorkerStartupRecovery"
				reclaim = func() error {
					_, err := q.CompleteWorkerStartupRecovery(ctx, db.CompleteWorkerStartupRecoveryParams{WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerEpoch: pgtype.Int8{Int64: 2, Valid: true}, RecoveryEvidence: []byte(`{"observed_at":"2026-10-01T00:00:00Z","quarantined":[]}`)})
					return err
				}
			}
			deletion, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer deletion.Rollback(context.Background())
			dbtest.MustExec(t, ctx, deletion, `SELECT id FROM computers WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1) FOR UPDATE`, ref.InstanceID)
			dbtest.MustExec(t, ctx, deletion, `SELECT id FROM computer_checkpoints WHERE id=$1 FOR UPDATE`, ref.CheckpointID)
			done := make(chan error, 1)
			go func() { done <- reclaim() }()
			for {
				var waiting bool
				if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query LIKE '%' || $1 || '%' AND wait_event_type='Lock')`, queryName).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("reclaim bypassed locked Computer: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			// If reclaim locks Instance before Computer, deletion cannot finish here.
			dbtest.MustExec(t, ctx, deletion, `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE NOWAIT`, ref.InstanceID)
			if err := deletion.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
