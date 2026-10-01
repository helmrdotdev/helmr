package db_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerRestoreDiscovery(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
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
 SELECT $2,environment_id,computer_id,base_computer_disk_version_id,$3,4096,'private',source_computer_instance_id,writer_generation FROM computer_checkpoints WHERE id=$1`, checkpoint, private, dbtest.Digest("restore-discovery"))
	var computerID uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT computer_id FROM computer_instances WHERE id=$1`, source).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	dbtest.InsertComputerVersion(t, t.Context(), tx, f.EnvironmentID, computerID, private)
	artifacts := dbtest.InsertCheckpointArtifacts(t, t.Context(), tx, work.RunID, "restore-discovery")
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_checkpoints SET status='ready',ready_at=now(),private_computer_disk_version_id=$2,ready_request_fingerprint=$3,manifest='{"version":1}',vm_config_artifact_id=$4,vm_state_artifact_id=$5,memory_artifact_id=$6,scratch_disk_artifact_id=$7 WHERE id=$1`, checkpoint, private, dbtest.Digest("restore-discovery"), artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET desired_state='closed',desired_version=2,admission_state='closed',observed_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{}',terminal_reason_code='checkpointed' WHERE id=$1`, source)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state,source_checkpoint_id,source_disk_version_id)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','restore',3,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring',$3,$4 FROM computer_instances WHERE id=$1`, source, instance, checkpoint, private)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET writer_generation=3,preparation_attempt_count=1,preparation_instance_id=$2 WHERE id=$1`, computerID, instance)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	params := db.GetComputerInstanceRestoreCheckpointParams{
		EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerInstanceID: pgvalue.UUID(instance),
		WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID),
		WorkerEpoch: 1, DesiredVersion: 1,
	}
	q := db.New(f.Pool)
	ready, err := q.GetComputerInstanceRestoreCheckpoint(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ComputerCheckpoint.ID != pgvalue.UUID(checkpoint) || ready.MemoryDigest != dbtest.Digest("restore-discovery-memory") {
		t.Fatal("wrong checkpoint or artifact descriptor")
	}
	members, err := q.ListComputerCheckpointRuns(t.Context(), db.ListComputerCheckpointRunsParams{EnvironmentID: params.EnvironmentID, CheckpointID: pgvalue.UUID(checkpoint)})
	if err != nil || len(members) != 0 {
		t.Fatalf("idle checkpoint: %v %v", members, err)
	}
	for _, name := range []string{"environment", "worker group", "worker host", "epoch", "desired version"} {
		t.Run(name, func(t *testing.T) {
			stale := params
			switch name {
			case "environment":
				stale.EnvironmentID = pgvalue.UUID(uuid.NewV7())
			case "worker group":
				stale.WorkerGroupID = pgvalue.UUID(uuid.NewV7())
			case "worker host":
				stale.WorkerHostID = pgvalue.UUID(uuid.NewV7())
			case "epoch":
				stale.WorkerEpoch++
			case "desired version":
				stale.DesiredVersion++
			}
			if _, err := q.GetComputerInstanceRestoreCheckpoint(t.Context(), stale); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale authority accepted: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name, sql string
		args      []any
	}{
		{"expired checkpoint", `UPDATE computer_checkpoints SET expires_at=now()-interval '1 second' WHERE id=$1`, []any{checkpoint}},
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=now()-interval '1 second' WHERE id=$1`, []any{instance}},
		{"opened admission", `UPDATE computer_instances SET admission_state='open' WHERE id=$1`, []any{instance}},
		{"different program", `UPDATE computer_instances SET program_deployment_id=NULL WHERE id=$1`, []any{instance}},
		{"different disk", `UPDATE computer_instances SET source_disk_version_id=(SELECT base_computer_disk_version_id FROM computer_checkpoints WHERE id=$2) WHERE id=$1`, []any{instance, checkpoint}},
		{"different cpu", `UPDATE computer_instances SET cpu_config_digest=$2 WHERE id=$1`, []any{instance, dbtest.Digest("other-cpu")}},
		{"different generation", `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=$1`, []any{computerID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			dbtest.MustExec(t, t.Context(), tx, test.sql, test.args...)
			if _, err = db.New(tx).GetComputerInstanceRestoreCheckpoint(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid source accepted: %v", err)
			}
		})
	}
	t.Run("preparation without program", func(t *testing.T) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=NULL,status='queued' WHERE id=$1`, work.RunID)
		dbtest.MustExec(t, t.Context(), tx, `DELETE FROM run_leases WHERE id=$1`, work.LeaseID)
		dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_checkpoints SET program_deployment_id=NULL WHERE id=$1`, checkpoint)
		dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET program_deployment_id=NULL WHERE id IN ($1,$2)`, source, instance)
		dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL IMMEDIATE`)
		got, err := db.New(tx).GetComputerInstanceRestoreCheckpoint(t.Context(), params)
		if err != nil || got.ComputerCheckpoint.ProgramDeploymentID.Valid {
			t.Fatalf("preparation-only restore: %v", err)
		}
	})
	t.Run("shared membership", func(t *testing.T) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		peerID, peerLeaseID := uuid.NewV7(), uuid.NewV7()
		dbtest.MustExec(t, t.Context(), tx, `
        INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,
            deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,
            computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,
            queue_score_at,max_active_duration_ms,retry_policy,root_span_id)
        SELECT $2,org_id,project_id,environment_id,deployment_id,
            deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,
            computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,
            queue_score_at,max_active_duration_ms,retry_policy,root_span_id
        FROM runs WHERE id=$1`, work.RunID, peerID)
		dbtest.MustExec(t, t.Context(), tx, `
        INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id)
        SELECT $2,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, work.RunID, peerID)
		dbtest.MustExec(t, t.Context(), tx, `
        INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,
            lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,
            computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,
            requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
            status,created_at,start_deadline_at,claimed_at,started_at,expires_at)
        SELECT $2,org_id,project_id,environment_id,$3,computer_id,region_id,
            1,1,worker_group_id,worker_host_id,worker_epoch,
            computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,
            requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
            status,created_at,start_deadline_at,claimed_at,started_at,expires_at
        FROM run_leases WHERE id=$1`, work.LeaseID, peerLeaseID, peerID)
		dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=$2,status='running',started_at=now(),first_lease_at=now() WHERE id=$1`, peerID, peerLeaseID)
		dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='running',started_at=now() WHERE id=$1`, work.RunID)

		for _, member := range []struct{ run, lease uuid.UUID }{{work.RunID, work.LeaseID}, {peerID, peerLeaseID}} {
			waitID := uuid.NewV7()
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,prior_run_lease_id,suspend_checkpoint_id,suspension_status)
    SELECT $2,environment_id,id,computer_id,'timer',now()+interval '1 hour',revision,1,$3,$4,'parked' FROM runs WHERE id=$1`, member.run, waitID, member.lease, checkpoint)
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_checkpoint_runs(checkpoint_id,environment_id,computer_id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_computer_instance_id,writer_generation)
    SELECT $2,environment_id,computer_id,run_id,attempt_number,$3,id,computer_instance_id,writer_generation FROM run_leases WHERE id=$1`, member.lease, checkpoint, waitID)
		}
		dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL IMMEDIATE`)
		query := db.New(tx)
		got, err := query.GetComputerInstanceRestoreCheckpoint(t.Context(), params)
		if err != nil || got.ComputerCheckpoint.ID != pgvalue.UUID(checkpoint) {
			t.Fatalf("shared checkpoint: %v", err)
		}
		members, err := query.ListComputerCheckpointRuns(t.Context(), db.ListComputerCheckpointRunsParams{EnvironmentID: params.EnvironmentID, CheckpointID: pgvalue.UUID(checkpoint)})
		if err != nil || len(members) != 2 {
			t.Fatalf("shared members: %v %v", members, err)
		}
		for _, member := range members {
			if member.SourceComputerInstanceID != source {
				t.Fatal("member from another source")
			}
		}
	})

	t.Run("committed destination", func(t *testing.T) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_checkpoints SET resume_computer_instance_id=$2,resume_committed_at=now() WHERE id=$1`, checkpoint, instance)
		if _, err = db.New(tx).GetComputerInstanceRestoreCheckpoint(t.Context(), params); err != nil {
			t.Fatalf("same destination replay: %v", err)
		}
		replacement := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET desired_state='closed',desired_version=2,admission_state='closed',observed_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{}',terminal_reason_code='lost' WHERE id=$1`, instance)
		dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state,source_checkpoint_id,source_disk_version_id)
   SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','restore',4,decode(repeat('04',32),'hex'),now()+interval '10 minutes','restoring',source_checkpoint_id,source_disk_version_id FROM computer_instances WHERE id=$1`, instance, replacement)
		dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET writer_generation=4,preparation_instance_id=$2 WHERE id=$1`, computerID, replacement)
		dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL IMMEDIATE`)
		newer := params
		newer.ComputerInstanceID = pgvalue.UUID(replacement)
		if _, err = db.New(tx).GetComputerInstanceRestoreCheckpoint(t.Context(), newer); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("committed checkpoint reused: %v", err)
		}
	})

}
