package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestPreparationExpirySerializesWithCancellationAndWorkerLoss(t *testing.T) {
	for _, action := range []string{"cancel", "worker loss"} {
		t.Run(action, func(t *testing.T) {
			f, work, a := commandPlacementFixture(t)
			candidate := queuedSharedRun(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now() WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			placement, err := a.PlaceReadyRun(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			q := db.New(f.Pool)
			before, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: placement.ComputerInstanceID})
			if err != nil {
				t.Fatal(err)
			}
			var claim int64
			if err = f.Pool.QueryRow(ctx, `SELECT claim_version FROM worker_hosts WHERE id=$1`, f.WorkerID).Scan(&claim); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE computer_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, before.ID)
			start := make(chan struct{})
			results := make(chan error, 3)
			for range 2 {
				go func() { <-start; _, e := a.ReconcileComputerInstances(ctx, 10); results <- e }()
			}
			go func() {
				<-start
				if action == "cancel" {
					c, e := run.NewCanceler(f.Pool)
					if e == nil {
						_, e = c.Cancel(ctx, run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: pgvalue.MustUUIDValue(candidate.RunID)})
					}
					results <- e
				} else {
					_, e := q.FenceWorkerHost(ctx, db.FenceWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: before.WorkerGroupID, ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: claim, ReasonCode: pgvalue.Text("test_loss")})
					results <- e
				}
			}()
			close(start)
			for range 3 {
				select {
				case e := <-results:
					if e != nil {
						t.Fatal(e)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			after, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: before.ID})
			if err != nil {
				t.Fatal(err)
			}
			closedByExpiry := after.DesiredState == "closed" && after.DesiredVersion == before.DesiredVersion+1
			fencedBeforeExpiry := action == "worker loss" && after.DesiredState == before.DesiredState && after.DesiredVersion == before.DesiredVersion && after.AdmissionState == "closed" && after.MountState == "lost"
			if after.ReclaimedAt.Valid || (!closedByExpiry && !fencedBeforeExpiry) {
				t.Fatalf("expiry changed physical authority incorrectly: %+v", after)
			}
			if action == "worker loss" && after.ObservedState != "lost" {
				t.Fatalf("unfenced instance: %s", after.ObservedState)
			}
			var attempts int
			if err = f.Pool.QueryRow(ctx, `SELECT preparation_attempt_count FROM computers WHERE id=$1`, before.ComputerID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatalf("preparation attempts=%d: %v", attempts, err)
			}
			if action == "cancel" {
				var status string
				if err = f.Pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, candidate.RunID).Scan(&status); err != nil || status != "cancelled" {
					t.Fatalf("cancellation=%s: %v", status, err)
				}
			}
		})
	}
}
