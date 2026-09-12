package db

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestOwnershipSurvivingScopePathsRejectAndRollback(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	w := ownershipTimerWait(t, f, work)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var mountID, runtimeID, workspaceID, workspaceLeaseID uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT workspace_mount_id,runtime_instance_id,workspace_id,id FROM workspace_leases WHERE owner_run_lease_id=$1", work.leaseID).Scan(&mountID, &runtimeID, &workspaceID, &workspaceLeaseID); err != nil {
		t.Fatal(err)
	}
	credential := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, "INSERT INTO worker_instance_credentials(id,worker_group_id,worker_instance_id,key_prefix,secret_hash) VALUES ($1,$2,$3,'ownership-test',decode('abcd','hex'))", credential, runLeaseTestWorkerGroup, f.workerID)
	checkpointID := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_checkpoints(id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id) SELECT $1,$2,1,$3,$4,id,workspace_id,base_version_id FROM workspace_leases WHERE owner_run_lease_id=$4`, checkpointID, work.runID, w.ID, work.leaseID)
	for _, test := range []struct {
		table, column string
		id            any
	}{
		{"worker_instances", "worker_group_id", f.workerID}, {"worker_instance_credentials", "worker_group_id", credential},
		{"workspace_mounts", "worker_group_id", mountID}, {"workspace_mounts", "worker_instance_id", mountID},
		{"run_leases", "worker_group_id", work.leaseID}, {"run_leases", "worker_instance_id", work.leaseID},
		{"workspace_leases", "worker_group_id", workspaceLeaseID}, {"workspace_leases", "worker_instance_id", workspaceLeaseID},
		{"workspace_leases", "environment_id", workspaceLeaseID}, {"workspace_leases", "runtime_instance_id", workspaceLeaseID},
		{"runs", "workspace_id", work.runID}, {"run_waits", "workspace_id", w.ID},
		{"run_checkpoints", "workspace_id", checkpointID}, {"runtime_instances", "deployment_definition_id", runtimeID},
	} {
		t.Run(test.table+"."+test.column, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23503", "UPDATE "+test.table+" SET "+test.column+"=$2 WHERE id=$1", test.id, uuid.NewV7())
		})
	}
	for _, test := range []struct {
		table string
		id    any
	}{
		{"worker_groups", runLeaseTestWorkerGroup}, {"worker_instances", f.workerID}, {"runtime_instances", runtimeID}, {"workspace_mounts", mountID}, {"workspaces", workspaceID}, {"runs", work.runID}, {"run_waits", w.ID},
	} {
		t.Run("delete "+test.table, func(t *testing.T) { rejectSchemaRow(t, tx, "23001", "DELETE FROM "+test.table+" WHERE id=$1", test.id) })
	}
	// Failed statements are rolled back only to their savepoint; all valid tuples
	// remain usable in the enclosing transaction.
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM run_checkpoints WHERE id=$1", checkpointID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback lost valid checkpoint=%d %v", count, err)
	}
}

func TestOwnershipTenantCopiesRejectBeforePlacement(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	substrateID, processID, claimID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, "INSERT INTO runtime_substrates(id,org_id,project_id,environment_id,deployment_definition_id,substrate_digest,substrate_format,substrate_contract,substrate_size_bytes) VALUES($1,$2,$3,$4,$5,'digest','format','contract',1)", substrateID, f.orgID, f.projectID, f.environmentID, f.workspaceDefinitionID)
	dbtest.MustExec(t, ctx, tx, "INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,expires_at) VALUES($1,$2,'workspace.exec',decode(repeat('fa',32),'hex'),decode(repeat('fb',32),'hex'),now(),now()+interval '30 days')", claimID, f.environmentID)
	dbtest.MustExec(t, ctx, tx, "INSERT INTO workspace_processes(id,org_id,project_id,environment_id,workspace_id,base_version_id,restore_desired_state,request,claim_id) SELECT $1,org_id,project_id,environment_id,workspace_id,base_workspace_version_id,'active','{}',$2 FROM runs WHERE id=$3", processID, claimID, work.runID)
	for _, table := range []string{"runtime_substrates", "workspace_processes"} {
		id := substrateID
		if table == "workspace_processes" {
			id = processID
		}
		for _, column := range []string{"org_id", "project_id", "environment_id"} {
			t.Run(table+"."+column, func(t *testing.T) {
				rejectSchemaRow(t, tx, "23503", "UPDATE "+table+" SET "+column+"=$2 WHERE id=$1", id, uuid.NewV7())
			})
		}
	}
	rejectSchemaRow(t, tx, "23514", "UPDATE workspace_processes SET worker_group_id=$2 WHERE id=$1", processID, runLeaseTestWorkerGroup)
	// Placement uses the full mount path, including copied tenant and epoch facts.
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_processes p SET state='starting',region_id=m.region_id,worker_group_id=m.worker_group_id,worker_instance_id=m.worker_instance_id,worker_epoch=m.worker_epoch,runtime_instance_id=m.runtime_instance_id,workspace_mount_id=m.id FROM workspace_mounts m JOIN run_leases l ON l.runtime_instance_id=m.runtime_instance_id WHERE p.id=$1 AND l.id=$2`, processID, work.leaseID)
	for _, column := range []string{"worker_group_id", "worker_instance_id", "runtime_instance_id", "workspace_mount_id"} {
		rejectSchemaRow(t, tx, "23503", "UPDATE workspace_processes SET "+column+"=$2 WHERE id=$1", processID, uuid.NewV7())
	}
	rejectSchemaRow(t, tx, "23503", "UPDATE workspace_processes SET worker_epoch=worker_epoch+1 WHERE id=$1", processID)
	var actions int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE (conname='runtime_substrates_environment_scope_fk' AND confdeltype='r') OR (conname='workspace_processes_environment_scope_fk' AND confdeltype='c')`).Scan(&actions); err != nil || actions != 2 {
		t.Fatalf("tenant delete actions=%d %v", actions, err)
	}
	rejectSchemaRow(t, tx, "23001", "DELETE FROM environments WHERE id=$1", f.environmentID)
}
