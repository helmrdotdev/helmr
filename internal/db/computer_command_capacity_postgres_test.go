package db_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

type computerCommandInterleavedPlanStore struct {
	*db.Queries
	afterExec func()
}

func (s computerCommandInterleavedPlanStore) ListPendingComputerCommandCapacityCandidates(
	ctx context.Context,
	params db.ListPendingComputerCommandCapacityCandidatesParams,
) ([]db.ListPendingComputerCommandCapacityCandidatesRow, error) {
	rows, err := s.Queries.ListPendingComputerCommandCapacityCandidates(ctx, params)
	if err == nil && s.afterExec != nil {
		s.afterExec()
	}
	return rows, err
}

func TestPendingComputerCommandCapacityCountsComputersAndPhysicalInstances(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	var computerID, instanceID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id,computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID, &instanceID); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	for range 2 {
		addCapacityCommand(t, f, computerID)
	}
	require := func(visible bool, accounted int) {
		t.Helper()
		rows, err := q.ListPendingComputerCommandCapacityCandidates(t.Context(), db.ListPendingComputerCommandCapacityCandidatesParams{RegionID: runtest.Region, RowLimit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if !visible {
			if len(rows) != 0 {
				t.Fatalf("ineligible computer remains in demand: %+v", rows)
			}
			return
		}
		if len(rows) != 1 || rows[0].ComputerID != pgvalue.UUID(computerID) || len(rows[0].AccountedPoolIds) != accounted {
			t.Fatalf("computer demand=%+v want one Computer and %d accounted pools", rows, accounted)
		}
		if _, err := definition.ParseComputerConfig(rows[0].ComputerConfig); err != nil {
			t.Fatal(err)
		}
		if accounted == 1 && rows[0].AccountedPoolIds[0] != pgvalue.UUID(f.WorkerPoolID) {
			t.Fatalf("wrong accounted pool: %+v", rows[0])
		}
	}
	require(true, 1)
	if rows, err := q.ListPendingComputerCommandCapacityCandidates(t.Context(), db.ListPendingComputerCommandCapacityCandidatesParams{RegionID: "other", RowLimit: 10}); err != nil || len(rows) != 0 {
		t.Fatalf("other region=%+v %v", rows, err)
	}
	for _, state := range []string{"stopped", "deleted"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state=$2 WHERE id=$1`, computerID, state)
		require(false, 0)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='active' WHERE id=$1`, computerID)
	for _, state := range []string{"dirty", "capturing", "capture_failed", "clean"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET dirty_state=$2 WHERE id=$1`, computerID, state)
		require(state != "capture_failed" && state != "dirty_state_lost", 1)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',recovery_id=$2,recovery_disk_version_id=head_disk_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, computerID, uuid.NewV7())
	require(false, 0)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='active',desired_state='active',dirty_state='clean',recovery_id=NULL,recovery_disk_version_id=NULL,recovery_reason=NULL,recovery_started_at=NULL WHERE id=$1`, computerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining',writer_expires_at=now()-interval '1 second' WHERE id=$1`, instanceID)
	require(true, 1)
	reclaimCapacityInstance(t, f, instanceID)
	require(true, 0)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled' WHERE computer_id=$1`, computerID)
	require(false, 0)
}

func TestComputerCommandCapacityPlanPreservesDiscoveryAcrossReclamation(t *testing.T) {
	for _, reclaimBeforeDiscovery := range []bool{false, true} {
		name := "reclaim after discovery"
		if reclaimBeforeDiscovery {
			name = "allocate after discovery"
		}
		t.Run(name, func(t *testing.T) {
			f := runtest.New(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_pools SET per_vm_guest_ephemeral_disk_bytes=34359738368,capacity_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, f.WorkerPoolID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET per_vm_guest_ephemeral_disk_bytes=34359738368,epoch_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, f.WorkerID)

			work := f.AddRunLease(t, "assigned", time.Now())
			var computerID, instanceID uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id,computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID, &instanceID); err != nil {
				t.Fatal(err)
			}
			addCapacityCommand(t, f, computerID)
			// The logical Run remains assigned while physical capacity changes.
			if reclaimBeforeDiscovery {
				reclaimCapacityInstance(t, f, instanceID)
			}
			store := computerCommandInterleavedPlanStore{Queries: db.New(f.Pool), afterExec: func() {
				if !reclaimBeforeDiscovery {
					reclaimCapacityInstance(t, f, instanceID)
					return
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,source_disk_version_id)
SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','prepare',writer_generation+1,decode(repeat('03',32),'hex'),now()+interval '10 minutes',source_disk_version_id FROM computer_instances WHERE id=$1`, instanceID, uuid.NewV7())
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=$1`, computerID)
			}}
			plan, err := workergroup.Plan(t.Context(), store, runtest.WorkerGroupID, workergroup.PlanRequest{Pools: []workergroup.PoolRequest{{PoolID: f.WorkerPoolID.String(), MaxAdditionalWorkers: 1}}}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Pools) != 1 || !plan.Complete {
				t.Fatalf("plan=%+v", plan)
			}
			got := plan.Pools[0]
			wantQueued := int64(0)
			if reclaimBeforeDiscovery {
				wantQueued = 1
			}
			if int64(got.CompatibleQueuedItems) != wantQueued || got.ScaleInBlocked == reclaimBeforeDiscovery {
				t.Fatalf("plan after concurrent physical change=%+v", got)
			}
		})
	}
}

func addCapacityCommand(t *testing.T, f runtest.Fixture, computerID uuid.UUID) {
	t.Helper()
	claimID, commandID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,receipt_expires_at) VALUES($1,$2,'computer.command.create',$3,$4,now(),now()+interval '30 days')`, claimID, f.EnvironmentID, dbtest.Hash(commandID.String()), dbtest.Hash("command"))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,cwd,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,$4,ARRAY['true'],'/workspace','{}','',300000,'api_key','fixture')`, commandID, f.EnvironmentID, computerID, claimID)
}

func reclaimCapacityInstance(t *testing.T, f runtest.Fixture, instanceID uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version+1,admission_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),terminal_reason_code='closed',reclaimed_at=now(),reclaim_evidence='{"method":"machine_closed"}' WHERE id=$1`, instanceID)
}
