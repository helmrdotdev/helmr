package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Readiness of an already ready Instance continues admitted work on paused or
// draining supply; the first allocated-to-ready transition completes
// preparation and needs admitting supply.
func TestReadyObservationOnNonAdmittingSupply(t *testing.T) {
	for _, supply := range []struct{ name, sql string }{
		{"paused Group", `UPDATE worker_groups SET status='paused',claim_version=claim_version+1 WHERE id=$1`},
		{"draining Group", `UPDATE worker_groups SET status='draining',primary_pool_id=NULL,claim_version=claim_version+1 WHERE id=$1`},
		{"paused Host", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak',vm_paused_reason='runtime_health' WHERE worker_group_id=$1`},
		{"draining Pool", `WITH g AS (UPDATE worker_groups SET primary_pool_id=NULL WHERE id=$1 RETURNING id) UPDATE worker_pools SET status='draining' WHERE worker_group_id=(SELECT id FROM g)`},
		{"draining Host", `WITH h AS (UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE worker_group_id=$1 RETURNING id) UPDATE computer_instances SET admission_state='draining' WHERE worker_host_id IN (SELECT id FROM h) AND admission_state='open'`},
	} {
		for _, allocated := range []bool{false, true} {
			t.Run(supply.name+map[bool]string{false: "/ready", true: "/allocated"}[allocated], func(t *testing.T) {
				f, work, _ := commandPlacementFixture(t)
				var id pgtype.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				if allocated {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL,guest_channel_token_hash=decode(repeat('ab',32),'hex'),guest_channel_token_expires_at=now()+interval '5 minutes' WHERE id=$1`, id)
				} else {
					// A Program-ready acknowledgement on the resident Instance.
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_version=desired_version+1,preparation_expires_at=now()-interval '1 second' WHERE id=$1`, id)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, supply.sql, runtest.WorkerGroupID)
				i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
				if err != nil {
					t.Fatal(err)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				row, err := RecordComputerInstanceReady(t.Context(), tx, i.WorkerGroupID, db.MarkComputerInstanceReadyParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, VMVCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest})
				if allocated {
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("first readiness on %s: %v", supply.name, err)
					}
					return
				}
				if err != nil || row.ObservedDesiredVersion != i.DesiredVersion {
					t.Fatalf("resident readiness on %s: %v %v", supply.name, row.ObservedDesiredVersion, err)
				}
			})
		}
	}
}
