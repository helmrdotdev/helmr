package dispatch_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"testing"
	"uuid"
)

func TestCheckpointAssignmentAllocatesRestoringInstance(t *testing.T) {
	for _, kind := range []string{"run", "command", "missing parked checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, kind != "missing parked checkpoint")
			cp := computertest.Complete(t, f, ref, manifest, objects)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"session_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, ref.InstanceID)
			dbtest.MustExec(t, t.Context(), tx, `UPDATE run_leases SET process_reconciled_at=now() WHERE computer_instance_id=$1`, ref.InstanceID)
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Capacity observations are independent of the captured member fixture.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET per_vm_guest_ephemeral_disk_bytes=34359738368,epoch_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, f.WorkerID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_pools SET per_vm_guest_ephemeral_disk_bytes=34359738368,capacity_guest_ephemeral_disk_bytes=274877906944 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.WorkerID)
			key, err := disk.NewFencingKey(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			authority, err := dispatch.NewRunAuthority(f.Pool, key)
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.NewV7()
			var instanceID string
			if kind != "run" {
				claim := uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,$4,ARRAY['true'],'{}',''::bytea,60000,'test','test')`, id, f.EnvironmentID, cp.ComputerID, claim)
				if kind == "missing parked checkpoint" {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='test_missing_resume_source' WHERE id=$1`, cp.ID)
				}
				assigned, err := authority.AssignCommand(t.Context(), dispatch.CommandCandidate{OrgID: pgvalue.UUID(f.OrgID), CommandID: pgvalue.UUID(id), ExpectedRevision: 1})
				if kind == "missing parked checkpoint" {
					if !errors.Is(err, dispatch.ErrCandidateChanged) {
						t.Fatalf("parked wait fallback=%+v %v", assigned, err)
					}
					var blocked bool
					if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM run_waits WHERE computer_id=$1 AND suspension_status='parked') AND NOT EXISTS(SELECT 1 FROM computer_instances WHERE computer_id=$1 AND reclaimed_at IS NULL)`, cp.ComputerID).Scan(&blocked); err != nil || !blocked {
						t.Fatalf("parked process fell back=%v %v", !blocked, err)
					}
					return
				}
				if err != nil || assigned.ProcessBound {
					t.Fatalf("restore command assignment=%+v %v", assigned, err)
				}
				instanceID = pgvalue.UUIDString(assigned.ComputerInstanceID)
			} else {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id)
   SELECT $2,org_id,project_id,environment_id,program_deployment_id,$3,'task','test-task','api',computer_id,(SELECT head_disk_version_id FROM computers WHERE id=computer_id),'{}','default',now(),now(),300000,'{"enabled":false}','11111111111111111111111111111111','2222222222222222' FROM computer_instances WHERE id=$1`, ref.InstanceID, id, f.TaskDefinitionID)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT id,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, id)
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				assigned, err := authority.AssignRun(t.Context(), dispatch.RunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(id), ExpectedRunRevision: 1})
				if err != nil || assigned.LeaseCreated {
					t.Fatalf("restore run assignment=%+v %v", assigned, err)
				}
				instanceID = pgvalue.UUIDString(assigned.ComputerInstanceID)
			}
			instance, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(uuid.MustParse(instanceID))})
			if err != nil {
				t.Fatal(err)
			}
			if instance.SourceCheckpointID != cp.ID || instance.SourceDiskVersionID != cp.PrivateComputerDiskVersionID || instance.AdmissionState != "restoring" || instance.ObservedState != "allocated" || instance.MountState != "pending" {
				t.Fatalf("allocated restore=%+v", instance)
			}
			var pinned bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT a.worker_group_id=b.worker_group_id AND a.vm_platform_id=b.vm_platform_id AND a.cpu_config_digest=b.cpu_config_digest AND a.vm_vcpu_count=b.vm_vcpu_count AND a.writer_generation>b.writer_generation FROM computer_instances a,computer_instances b WHERE a.id=$1 AND b.id=$2`, instance.ID, ref.InstanceID).Scan(&pinned); err != nil || !pinned {
				t.Fatalf("restore identity pinned=%v %v", pinned, err)
			}
		})
	}
}
