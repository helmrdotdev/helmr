package db_test

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"testing"
	"time"
	"uuid"
)

func TestCommandRecoveryDiscoverySkipsPendingProcessCleanup(t *testing.T) {
	f := runtest.New(t)
	live := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	lost := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	for range 32 {
		insertRecoveryCommand(t, f, live, true)
	}
	actionable := insertRecoveryCommand(t, f, lost, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=now()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, lost.LeaseID)
	rows, err := db.New(f.Pool).ListRecoverableComputerCommandCandidates(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != pgvalue.UUID(actionable) {
		t.Fatalf("lost Command starved behind unchanged scopes: %+v", rows)
	}
}

func TestSecretRevocationRetainsCommandProcessUntilReconciled(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	id := insertRecoveryCommand(t, f, work, false)
	command, err := db.New(f.Pool).StopSecretRevokedComputerCommand(t.Context(), db.StopSecretRevokedComputerCommandParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(id), ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if command.Status != "stopping" || command.TerminalAt.Valid || !command.CancelRequestedAt.Valid || !command.ComputerInstanceID.Valid || command.ProcessReconciledAt.Valid {
		t.Fatalf("revoked Command: %+v", command)
	}
	var writerLive bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_state='ready' AND admission_state='open' AND reclaimed_at IS NULL AND writer_expires_at>now() FROM computer_instances WHERE id=$1`, command.ComputerInstanceID).Scan(&writerLive); err != nil {
		t.Fatal(err)
	}
	if !writerLive {
		t.Fatal("revocation changed Instance writer")
	}
}

func insertRecoveryCommand(t *testing.T, f runtest.Fixture, work runtest.RunLease, terminal bool) uuid.UUID {
	t.Helper()
	id, claim := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,computer_instance_id,writer_generation,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,status,terminal_at,terminal_reason_code)
 SELECT $2,environment_id,computer_id,$3,computer_instance_id,writer_generation,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text,
 CASE WHEN $4::boolean THEN 'cancelled' ELSE 'running' END,CASE WHEN $4::boolean THEN now() END,CASE WHEN $4::boolean THEN 'cancelled' END FROM run_leases WHERE id=$1`, work.LeaseID, id, claim, terminal)
	return id
}

func TestSecretRevocationDiscoveryAdvancesPastStoppedCommands(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	secretID, versionID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secrets(id,environment_id,name,current_version_id,revocation_generation) VALUES($1,$2,'revocation-batch',$3,2)`, secretID, f.EnvironmentID, versionID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($1,$2,1,decode(repeat('01',12),'hex'),decode(repeat('02',16),'hex'))`, versionID, secretID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_secrets(mode,computer_id,environment_id,placement_kind,placement_target,secret_id) SELECT 'raw',computer_id,environment_id,'env','TOKEN',$2 FROM run_leases WHERE id=$1`, work.LeaseID, secretID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	for n := 0; n < 3; n++ {
		id := insertRecoveryCommand(t, f, work, false)
		if n == 0 {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET computer_instance_id=NULL,writer_generation=NULL,status='pending' WHERE id=$1`, id)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO secret_resolutions(id,computer_id,command_id,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation) SELECT $2,computer_id,id,'env','TOKEN',$3,$4,1 FROM computer_commands WHERE id=$1`, id, uuid.NewV7(), secretID, versionID)
		if n < 2 {
			stopped, err := q.StopSecretRevokedComputerCommand(t.Context(), db.StopSecretRevokedComputerCommandParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(id), ExpectedRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 && (stopped.Status != "failed" || stopped.TerminalReasonCode.String != "secret_revoked" || !stopped.TerminalAt.Valid) {
				t.Fatalf("pending revocation=%+v", stopped)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET created_at=now()-interval '1 hour' WHERE id=$1`, id)
		} else {
			rows, err := q.ListSecretRevocationProcesses(t.Context(), db.ListSecretRevocationProcessesParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SecretID: pgvalue.UUID(secretID), RevocationGeneration: 2, RowLimit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].ID != pgvalue.UUID(id) {
				t.Fatalf("live command starved behind stopped work: %+v", rows)
			}
		}
	}
}
