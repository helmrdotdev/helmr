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
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var instanceID, computerID uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT computer_instance_id,computer_id FROM run_leases WHERE id=$1", work.leaseID).Scan(&instanceID, &computerID); err != nil {
		t.Fatal(err)
	}
	waitID := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id) SELECT $1,environment_id,id,computer_id,'timer',now()+interval '1 hour',revision,1,$2 FROM runs WHERE id=$3`, waitID, work.leaseID, work.runID)
	credential := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, "INSERT INTO worker_host_credentials(id,worker_group_id,worker_host_id,key_prefix,secret_hash) VALUES ($1,$2,$3,'ownership-test',decode('abcd','hex'))", credential, runLeaseTestWorkerGroup, f.workerID)
	checkpointID := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoints(id,environment_id,computer_id,computer_spec_id,source_computer_instance_id,writer_generation,membership_revision,base_computer_disk_version_id) SELECT $1,i.environment_id,i.computer_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,r.base_computer_disk_version_id FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id JOIN runs r ON r.id=l.run_id WHERE l.id=$2`, checkpointID, work.leaseID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoint_runs(checkpoint_id,environment_id,computer_id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_computer_instance_id,writer_generation) SELECT $1,environment_id,computer_id,run_id,attempt_number,$2,id,computer_instance_id,writer_generation FROM run_leases WHERE id=$3`, checkpointID, waitID, work.leaseID)
	for _, test := range []struct {
		table, column string
		id            any
	}{
		{"worker_hosts", "worker_group_id", f.workerID}, {"worker_host_credentials", "worker_group_id", credential},
		{"computer_instances", "worker_group_id", instanceID}, {"computer_instances", "worker_host_id", instanceID},
		{"run_leases", "worker_group_id", work.leaseID}, {"run_leases", "worker_host_id", work.leaseID},
		{"run_leases", "environment_id", work.leaseID}, {"run_leases", "computer_instance_id", work.leaseID},
		{"runs", "computer_id", work.runID}, {"run_waits", "computer_id", waitID},
		{"computer_checkpoints", "computer_id", checkpointID}, {"computer_instances", "computer_spec_id", instanceID},
	} {
		t.Run(test.table+"."+test.column, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23503", "UPDATE "+test.table+" SET "+test.column+"=$2 WHERE id=$1", test.id, uuid.NewV7())
		})
	}
	for _, test := range []struct {
		table string
		id    any
	}{
		{"worker_groups", runLeaseTestWorkerGroup}, {"worker_hosts", f.workerID}, {"computer_instances", instanceID}, {"computers", computerID}, {"runs", work.runID}, {"run_waits", waitID},
	} {
		t.Run("delete "+test.table, func(t *testing.T) { rejectSchemaRow(t, tx, "23001", "DELETE FROM "+test.table+" WHERE id=$1", test.id) })
	}
	// Failed statements are rolled back only to their savepoint; all valid tuples
	// remain usable in the enclosing transaction.
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM computer_checkpoints WHERE id=$1", checkpointID).Scan(&count); err != nil || count != 1 {
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
	commandID, claimID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, "INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,receipt_expires_at) VALUES($1,$2,'computer.command.create',decode(repeat('fa',32),'hex'),decode(repeat('fb',32),'hex'),now(),now()+interval '30 days')", claimID, f.environmentID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_commands(id,environment_id,computer_id,argv,cwd,env,stdin,timeout_ms,claim_id,created_by_subject_type,created_by_subject_id) SELECT $1,environment_id,computer_id,ARRAY['true'],'/computer','{}','',300000,$2,'api_key','fixture' FROM runs WHERE id=$3`, commandID, claimID, work.runID)
	for _, column := range []string{"environment_id", "computer_id", "claim_id"} {
		t.Run(column, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23503", "UPDATE computer_commands SET "+column+"=$2 WHERE id=$1", commandID, uuid.NewV7())
		})
	}
	rejectSchemaRow(t, tx, "23514", "UPDATE computer_commands SET writer_generation=1 WHERE id=$1", commandID)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_commands c SET status='starting',computer_instance_id=l.computer_instance_id,writer_generation=l.writer_generation FROM run_leases l WHERE c.id=$1 AND l.id=$2`, commandID, work.leaseID)
	rejectSchemaRow(t, tx, "23503", "UPDATE computer_commands SET computer_instance_id=$2 WHERE id=$1", commandID, uuid.NewV7())
	rejectSchemaRow(t, tx, "23503", "UPDATE computer_commands SET writer_generation=writer_generation+1 WHERE id=$1", commandID)
	rejectSchemaRow(t, tx, "23001", "DELETE FROM environments WHERE id=$1", f.environmentID)
}
