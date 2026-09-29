package dispatch

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestReadyObservationFencesAndPreparationCommit(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL,guest_channel_token_hash=decode(repeat('ab',32),'hex'),guest_channel_token_expires_at=now()+interval '5 minutes' WHERE id=$1`, id)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=3,preparation_instance_id=$1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, id)
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	params := db.MarkComputerInstanceReadyParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, VMVCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest}
	acknowledge := func(p db.MarkComputerInstanceReadyParams, commit bool) (db.ComputerInstance, error) {
		tx, e := f.Pool.Begin(t.Context())
		if e != nil {
			return db.ComputerInstance{}, e
		}
		defer tx.Rollback(context.Background())
		row, e := RecordComputerInstanceReady(t.Context(), tx, i.WorkerGroupID, p)
		if e != nil {
			return row, e
		}
		if commit {
			e = tx.Commit(t.Context())
		}
		return row, e
	}
	for _, change := range []struct {
		name  string
		apply func(*db.MarkComputerInstanceReadyParams)
	}{
		{"observation", func(p *db.MarkComputerInstanceReadyParams) { p.ExpectedObservedVersion++ }},
		{"desired", func(p *db.MarkComputerInstanceReadyParams) { p.DesiredVersion++ }},
		{"CPU count", func(p *db.MarkComputerInstanceReadyParams) { p.VMVCPUCount++ }},
		{"CPU config", func(p *db.MarkComputerInstanceReadyParams) {
			p.CPUConfigDigest = hex.EncodeToString(dbtest.Hash("different"))
		}},
		{"epoch", func(p *db.MarkComputerInstanceReadyParams) { p.WorkerEpoch++ }},
	} {
		t.Run(change.name, func(t *testing.T) {
			p := params
			change.apply(&p)
			if _, err := acknowledge(p, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid readiness accepted: %v", err)
			}
		})
	}
	if _, err = acknowledge(params, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count FROM computers WHERE id=$1`, i.ComputerID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("rollback lost budget: %d %v", count, err)
	}
	ready, err := acknowledge(params, true)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ObservedState != "ready" || hex.EncodeToString(ready.GuestChannelTokenHash) != "abababababababababababababababababababababababababababababababab" {
		t.Fatalf("incorrect ready state: %+v", ready)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count FROM computers WHERE id=$1`, i.ComputerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ready budget: %d %v", count, err)
	}
	if _, err = acknowledge(params, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed observation: %v", err)
	}
	// A later Program acknowledgement on the same live Instance must not depend
	// on its already-completed initial preparation deadline.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_version=desired_version+1,preparation_expires_at=now()-interval '1 second' WHERE id=$1`, id)
	params.DesiredVersion++
	params.ExpectedObservedVersion = ready.ObservedVersion
	if _, err = acknowledge(params, true); err != nil {
		t.Fatalf("resident Program readiness: %v", err)
	}
}

func TestResidentReadinessRejectsWorkerExpiringDuringComputerLock(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(context.Background())
	if _, err = hold.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR UPDATE`, i.ComputerID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp()-($2-2)*interval '1 second' WHERE id=$1`, i.WorkerHostID, workergroup.ObservationFreshnessSeconds)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pidCh := make(chan int32, 1)
	done := make(chan error, 1)
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
		_, e = RecordComputerInstanceReady(ctx, tx, i.WorkerGroupID, db.MarkComputerInstanceReadyParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, VMVCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest})
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
		if err = f.Pool.QueryRow(ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false),observed_at<clock_timestamp()-$3*interval '1 second' FROM worker_hosts WHERE id=$2`, pid, i.WorkerHostID, workergroup.ObservationFreshnessSeconds).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("readiness finished before lock release: %v", e)
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
			t.Fatalf("expired Worker acknowledged: %v", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestFrozenReadinessRetainsPreparationBudget(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	checkpoint, private, instance := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	var source pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_checkpoints(id,computer_id,environment_id,computer_spec_id,source_computer_instance_id,writer_generation,membership_revision,program_deployment_id,base_computer_disk_version_id)
 SELECT $2,i.computer_id,i.environment_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,i.program_deployment_id,c.head_disk_version_id FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, source, checkpoint)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,source_computer_instance_id,writer_generation)
 SELECT $2,environment_id,computer_id,base_computer_disk_version_id,$3,4096,'private',source_computer_instance_id,writer_generation FROM computer_checkpoints WHERE id=$1`, checkpoint, private, dbtest.Digest("frozen-ready"))
	var computerID uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT computer_id FROM computer_instances WHERE id=$1`, source).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	dbtest.InsertComputerGeneration(t, t.Context(), tx, f.EnvironmentID, computerID, private)
	artifacts := dbtest.InsertCheckpointArtifacts(t, t.Context(), tx, work.RunID, "frozen-ready")
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_checkpoints SET status='ready',ready_at=now(),private_computer_disk_version_id=$2,ready_request_fingerprint=$3,manifest='{"version":1}',vm_config_artifact_id=$4,vm_state_artifact_id=$5,memory_artifact_id=$6,scratch_disk_artifact_id=$7 WHERE id=$1`, checkpoint, private, dbtest.Digest("frozen-ready"), artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET desired_state='closed',desired_version=2,admission_state='closed',observed_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{}',terminal_reason_code='checkpointed' WHERE id=$1`, source)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state,source_checkpoint_id,source_disk_version_id)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','restore',3,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring',$3,$4 FROM computer_instances WHERE id=$1`, source, instance, checkpoint, private)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET writer_generation=3,preparation_attempt_count=8,preparation_instance_id=$2 WHERE id=$1`, computerID, instance)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(instance)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	ready, err := RecordComputerInstanceReady(t.Context(), tx, i.WorkerGroupID, db.MarkComputerInstanceReadyParams{ID: i.ID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, VMVCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count FROM computers WHERE id=$1`, computerID).Scan(&count); err != nil || count != 8 || ready.AdmissionState != "restoring" {
		t.Fatalf("frozen readiness ended preparation: count=%d admission=%s err=%v", count, ready.AdmissionState, err)
	}
}

func TestInitialReadinessRechecksDeadlineAfterComputerLock(t *testing.T) {
	for _, deadline := range []string{"preparation_expires_at", "writer_expires_at"} {
		t.Run(deadline, func(t *testing.T) {
			f, work, _ := commandPlacementFixture(t)
			var id pgtype.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL,guest_channel_token_hash=decode(repeat('ab',32),'hex'),guest_channel_token_expires_at=now()+interval '5 minutes' WHERE id=$1`, id)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=1,preparation_instance_id=$1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, id)
			i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			hold, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(context.Background())
			var blocker int32
			if err = hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, hold, `SELECT id FROM computers WHERE id=$1 FOR UPDATE`, i.ComputerID)
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE computer_instances SET `+deadline+`=clock_timestamp()+interval '2 seconds' WHERE id=$1`, id)
			snapshot := func() string {
				t.Helper()
				var value string
				if err := f.Pool.QueryRow(ctx, `SELECT jsonb_build_object('instance',to_jsonb(i),'computer',to_jsonb(c))::text FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, id).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			done := make(chan error, 1)
			go func() {
				tx, e := f.Pool.Begin(ctx)
				if e != nil {
					done <- e
					return
				}
				defer tx.Rollback(context.Background())
				_, e = RecordComputerInstanceReady(ctx, tx, i.WorkerGroupID, db.MarkComputerInstanceReadyParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, VMVCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest})
				if e == nil {
					e = tx.Commit(ctx)
				}
				done <- e
			}()
			for {
				var blocked, expired bool
				if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))),`+deadline+`<clock_timestamp() FROM computer_instances WHERE id=$2`, blocker, id).Scan(&blocked, &expired); err != nil {
					t.Fatal(err)
				}
				if blocked && expired {
					break
				}
				select {
				case e := <-done:
					t.Fatalf("readiness did not wait: %v", e)
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
					t.Fatalf("expired preparation accepted: %v", e)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if after := snapshot(); after != before {
				t.Fatal("expired readiness changed Computer or Instance")
			}
		})
	}
}
