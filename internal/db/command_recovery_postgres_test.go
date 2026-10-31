package db_test

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func insertRecoveryCommand(t *testing.T, f sessiontest.Fixture, computer uuid.UUID, terminal bool) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO commands(environment_id,id,computer_id,computer_lease_epoch,argv,env,stdin,timeout_ms,caller_kind,caller_id,status,terminal_at,terminal_reason_code) VALUES($1,$2,$3,1,ARRAY['true'],'{}','',60000,'user',uuidv7(),CASE WHEN $4::boolean THEN 'cancelled' ELSE 'running' END,CASE WHEN $4::boolean THEN now() END,CASE WHEN $4::boolean THEN 'cancelled' END)`, f.Environment, id, computer, terminal)
	return id
}

func TestCommandRecoveryDiscoverySkipsPendingProcessCleanup(t *testing.T) {
	f := sessiontest.New(t)
	for range 32 {
		insertRecoveryCommand(t, f, f.Computer, true)
	}
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE environment_id=$1 AND id=$3`, f.Environment, other, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT environment_id,$3,epoch,worker_host_id,worker_epoch,clock_timestamp()-interval '1 second','lost',$3,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer, other)
	actionable := insertRecoveryCommand(t, f, other, false)
	position, more, err := command.RecoverBatch(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, command.RecoveryPosition{}, 1)
	if err != nil || !more || position.ID != actionable {
		t.Fatalf("recovery position=%+v more=%v %v", position, more, err)
	}
	var lost, unreconciled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='lost',process_reconciled_at IS NULL FROM commands WHERE environment_id=$1 AND id=$2`, f.Environment, actionable).Scan(&lost, &unreconciled); err != nil || !lost || !unreconciled {
		t.Fatalf("lost Command starved or falsely reconciled: %v %v %v", lost, unreconciled, err)
	}
}
