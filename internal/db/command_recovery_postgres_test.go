package db_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func insertRecoveryCommand(t *testing.T, f agenttest.Fixture, computer uuid.UUID, terminal bool) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(environment_id,id,computer_id,computer_lease_epoch,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,status,terminal_at,terminal_reason_code) VALUES($1,$2,$3,1,ARRAY['true'],'{}','',60000,'user','fixture',CASE WHEN $4::boolean THEN 'cancelled' ELSE 'running' END,CASE WHEN $4::boolean THEN now() END,CASE WHEN $4::boolean THEN 'cancelled' END)`, f.Environment, id, computer, terminal)
	return id
}

func TestCommandRecoveryDiscoverySkipsPendingProcessCleanup(t *testing.T) {
	f := agenttest.New(t)
	for range 32 {
		insertRecoveryCommand(t, f, f.Computer, true)
	}
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE environment_id=$1 AND id=$3`, f.Environment, other, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT environment_id,$3,epoch,worker_host_id,worker_epoch,clock_timestamp()-interval '1 second','lost',$3,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer, other)
	actionable := insertRecoveryCommand(t, f, other, false)
	n, err := command.RecoverBatch(t.Context(), f.Pool, 1)
	if err != nil || n != 1 {
		t.Fatalf("recovery=%d %v", n, err)
	}
	var lost, unreconciled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='lost',process_reconciled_at IS NULL FROM computer_commands WHERE environment_id=$1 AND id=$2`, f.Environment, actionable).Scan(&lost, &unreconciled); err != nil || !lost || !unreconciled {
		t.Fatalf("lost Command starved or falsely reconciled: %v %v %v", lost, unreconciled, err)
	}
}

func TestSecretRevocationDiscoveryAdvancesPastStoppedCommands(t *testing.T) {
	f := agenttest.New(t)
	secretID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO secrets(id,environment_id,name,status,revoked_at,revocation_generation) VALUES($1,$2,'TOKEN','revoked',clock_timestamp(),2)`, secretID, f.Environment)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.Environment, f.Computer, secretID)
	for n := 0; n < 3; n++ {
		id := insertRecoveryCommand(t, f, f.Computer, false)
		if n == 0 {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='pending',computer_lease_epoch=NULL WHERE environment_id=$1 AND id=$2`, f.Environment, id)
		}
		count, err := command.StopSecretRevokedCommands(t.Context(), f.Pool, secret.Revocation{EnvironmentID: f.Environment, SecretID: secretID, Generation: 2}, 1)
		if err != nil || count != 1 {
			t.Fatalf("revocation discovery %d=%d %v", n, count, err)
		}
		var status string
		var terminal, reconciled bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT status,terminal_at IS NOT NULL,process_reconciled_at IS NOT NULL FROM computer_commands WHERE environment_id=$1 AND id=$2`, f.Environment, id).Scan(&status, &terminal, &reconciled); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			if status != "failed" || !terminal {
				t.Fatalf("pending revocation=%s %v", status, terminal)
			}
		} else if status != "stopping" || terminal || reconciled {
			t.Fatalf("physical process falsely stopped=%s terminal=%v reconciled=%v", status, terminal, reconciled)
		}
	}
	var live bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='active' AND fenced_at IS NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer).Scan(&live); err != nil || !live {
		t.Fatalf("revocation changed physical lease: %v %v", live, err)
	}
}
