package dispatch

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// The dispatcher's failure documents for pending Commands keep their exact
// encoding: a permanent rejection carries its code and message, an assignment
// timeout only its code.
func TestPendingCommandFailureDocumentsKeepTheirEncoding(t *testing.T) {
	f, work, a := commandAssignmentFixture(t)
	failure := func(id uuid.UUID) (string, string) {
		t.Helper()
		var status, document string
		if err := f.Pool.QueryRow(t.Context(), `SELECT status,error::text FROM computer_commands WHERE id=$1`, id).Scan(&status, &document); err != nil {
			t.Fatal(err)
		}
		return status, document
	}

	timedOut := pendingSharedCommand(t, f, work)
	if err := a.FailPendingComputerCommand(t.Context(), timedOut, "computer_command_placement_timed_out"); err != nil {
		t.Fatal(err)
	}
	if status, document := failure(uuid.UUID(timedOut.CommandID.Bytes)); status != "failed" || document != `{"code": "computer_command_placement_timed_out"}` {
		t.Fatalf("timeout status=%s error=%s", status, document)
	}

	// A Secret placement without a resolution for the Command rejects it.
	secretID, versionID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secrets(id,environment_id,name,current_version_id) VALUES($1,$2,'rejection',$3)`, secretID, f.EnvironmentID, versionID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($1,$2,1,decode(repeat('01',12),'hex'),decode(repeat('02',16),'hex'))`, versionID, secretID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_secrets(mode,computer_id,environment_id,placement_kind,placement_target,secret_id) SELECT 'raw',computer_id,environment_id,'env','TOKEN',$2 FROM run_leases WHERE id=$1`, work.LeaseID, secretID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	rejected := pendingSharedCommand(t, f, work)
	if _, err := a.AssignCommand(t.Context(), rejected); err != nil {
		t.Fatal(err)
	}
	if status, document := failure(uuid.UUID(rejected.CommandID.Bytes)); status != "failed" || document != `{"code": "computer_command_secret_unavailable", "message": "command secret resolution is revoked or incomplete"}` {
		t.Fatalf("rejection status=%s error=%s", status, document)
	}
}
