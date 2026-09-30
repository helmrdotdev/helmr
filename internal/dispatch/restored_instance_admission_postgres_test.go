package dispatch_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNewMembersJoinPreviouslyRestoredInstance(t *testing.T) {
	f, a, fence := dispatchtest.Restore(t, true)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	checkpoint, err := a.CommitRestore(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err = tx.QueryRow(t.Context(), `SELECT writer_generation FROM computer_instances WHERE id=$1`, fence.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	instance, err := db.New(tx).OpenRestoredComputerInstance(t.Context(), db.OpenRestoredComputerInstanceParams{ComputerInstanceID: pgvalue.UUID(fence.ID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), WriterGeneration: generation, DesiredVersion: fence.DesiredVersion, WorkerHostID: pgvalue.UUID(fence.Host.HostID), WorkerEpoch: fence.Host.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []bool{false, true} {
		id := uuid.NewV7()
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
		dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id)
  SELECT $2,org_id,project_id,environment_id,program_deployment_id,$3,'task','test-task','api',computer_id,(SELECT head_disk_version_id FROM computers WHERE id=computer_id),'{}','default',now(),now(),300000,'{"enabled":false}','11111111111111111111111111111111','2222222222222222' FROM computer_instances WHERE id=$1`, instance.ID, id, f.TaskDefinitionID)
		dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT id,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, id)
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if actor {
			f.ConvertToActor(t, t.Context(), runtest.RunLease{RunID: id}, `{"enabled":false}`)
		}
		assigned, err := a.AssignRun(t.Context(), dispatch.RunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(id), ExpectedRunRevision: 1})
		if err != nil || !assigned.LeaseCreated {
			t.Fatalf("fresh member assignment: %+v %v", assigned, err)
		}
		tx, err = f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		claimed, err := run.ClaimExecution(t.Context(), tx, run.ExecutionFence{LeaseID: assigned.Lease.ID, LeaseSequence: assigned.Lease.LeaseSequence, WorkerGroupID: instance.WorkerGroupID, WorkerHostID: instance.WorkerHostID, WorkerEpoch: instance.WorkerEpoch, GroupClaimVersion: 1, HostClaimVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if claimed.Instance.ID != instance.ID || claimed.Instance.SourceCheckpointID != checkpoint.ID || claimed.Lease.Status != "starting" || claimed.Attempt.Number != 1 {
			t.Fatalf("fresh claim treated as captured member: %+v", claimed.Lease)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	id, claim := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,$4,ARRAY['true'],'{}',''::bytea,60000,'test','test')`, id, f.EnvironmentID, instance.ComputerID, claim)
	assigned, err := a.AssignCommand(t.Context(), dispatch.CommandCandidate{OrgID: pgvalue.UUID(f.OrgID), CommandID: pgvalue.UUID(id), ExpectedRevision: 1})
	if err != nil || !assigned.ProcessBound || assigned.ComputerInstanceID != instance.ID {
		t.Fatalf("command on restored instance: %+v %v", assigned, err)
	}
	var checkpointMember pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM computer_checkpoints WHERE id=$1 AND resume_computer_instance_id=$2 AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_runs WHERE checkpoint_id=$1)`, checkpoint.ID, instance.ID).Scan(&checkpointMember); err != nil {
		t.Fatalf("fresh members altered immutable capture: %v", err)
	}
}
