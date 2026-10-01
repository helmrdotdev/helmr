package command

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
)

// A revoked running Command is stopped in the fence transaction and then
// recovered in its own transaction: on a lost Instance, recovery records the
// lost result after the stop. A pending Command fails without a process.
func TestStopSecretRevokedCommandsStopsThenRecovers(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	live := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	lost := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	idle := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	for _, work := range []runtest.RunLease{live, lost, idle} {
		f.PlaceSecret(t, work.LeaseID, secretID, 1)
	}
	running := commandtest.Bound(t, f, live.LeaseID, "running")
	stranded := commandtest.Bound(t, f, lost.LeaseID, "running")
	pending, _ := commandtest.Pending(t, f, idle.LeaseID)
	for _, id := range []uuid.UUID{running.ID, stranded.ID, pending} {
		commandtest.ResolveSecret(t, f, id, secretID)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, stranded.InstanceID)
	f.RevokeSecret(t, secretID, 1)
	revocation := secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}

	examined, err := StopSecretRevokedCommands(t.Context(), f.Pool, revocation, 10)
	if err != nil || examined != 3 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	for id, want := range map[uuid.UUID]string{running.ID: "stopping", stranded.ID: "lost", pending: "failed"} {
		var status, reason string
		var cancelRequested bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT status,coalesce(terminal_reason_code,''),cancel_requested_at IS NOT NULL FROM computer_commands WHERE id=$1`, id).Scan(&status, &reason, &cancelRequested); err != nil {
			t.Fatal(err)
		}
		if status != want || !cancelRequested {
			t.Fatalf("revoked Command status = %s, cancel requested %v; want %s", status, cancelRequested, want)
		}
		if id == pending && reason != "secret_revoked" {
			t.Fatalf("pending Command terminal reason = %s", reason)
		}
	}
	examined, err = StopSecretRevokedCommands(t.Context(), f.Pool, revocation, 10)
	if err != nil || examined != 0 {
		t.Fatalf("drained batch = %d, %v", examined, err)
	}
}

// A Command that changes between its fence and its recovery is still
// examined without error.
func TestStopSecretRevokedCommandsIgnoresChangedRecovery(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, work.LeaseID, secretID, 1)
	bound := commandtest.Bound(t, f, work.LeaseID, "running")
	commandtest.ResolveSecret(t, f, bound.ID, secretID)
	f.RevokeSecret(t, secretID, 1)
	// Advance the Command's revision again whenever it is stopped, so
	// recovery observes a revision other than the one the fence returned.
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION advance_stopped_command() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN UPDATE computer_commands SET revision=revision+1 WHERE id=NEW.id; RETURN NULL; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER advance_stopped_command AFTER UPDATE ON computer_commands FOR EACH ROW
 WHEN (OLD.cancel_requested_at IS NULL AND NEW.cancel_requested_at IS NOT NULL) EXECUTE FUNCTION advance_stopped_command()`)

	examined, err := StopSecretRevokedCommands(t.Context(), f.Pool, secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}, 10)
	if err != nil || examined != 1 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	var status string
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,revision FROM computer_commands WHERE id=$1`, bound.ID).Scan(&status, &revision); err != nil {
		t.Fatal(err)
	}
	if status != "stopping" || revision != 3 {
		t.Fatalf("changed Command status = %s, revision %d", status, revision)
	}
}

// A candidate whose Computer no longer places the Secret at the revoked
// generation is examined and left unchanged.
func TestStopSecretRevokedCommandsCountsStaleCandidates(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, work.LeaseID, secretID, 1)
	first := commandtest.Bound(t, f, work.LeaseID, "running")
	second := commandtest.Bound(t, f, work.LeaseID, "running")
	commandtest.ResolveSecret(t, f, first.ID, secretID)
	commandtest.ResolveSecret(t, f, second.ID, secretID)
	f.RevokeSecret(t, secretID, 2)

	examined, err := StopSecretRevokedCommands(t.Context(), f.Pool, secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}, 10)
	if err != nil || examined != 2 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	for _, id := range []uuid.UUID{first.ID, second.ID} {
		if status := secretRevocationCommandStatus(t, f, id); status != "running" {
			t.Fatalf("stale candidate Command status = %s", status)
		}
	}
}

func TestStopSecretRevokedCommandsReturnsPartialCountOnFailure(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	placed := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	overplaced := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, placed.LeaseID, secretID, 1)
	f.PlaceSecret(t, overplaced.LeaseID, secretID, 65)
	first := commandtest.Bound(t, f, placed.LeaseID, "running")
	failing := commandtest.Bound(t, f, overplaced.LeaseID, "running")
	last := commandtest.Bound(t, f, placed.LeaseID, "running")
	for _, id := range []uuid.UUID{first.ID, failing.ID, last.ID} {
		commandtest.ResolveSecret(t, f, id, secretID)
	}
	f.RevokeSecret(t, secretID, 1)

	examined, err := StopSecretRevokedCommands(t.Context(), f.Pool, secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}, 10)
	if err == nil || examined != 1 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	for id, want := range map[uuid.UUID]string{first.ID: "stopping", failing.ID: "running", last.ID: "running"} {
		if status := secretRevocationCommandStatus(t, f, id); status != want {
			t.Fatalf("Command status = %s, want %s", status, want)
		}
	}
}

func secretRevocationCommandStatus(t *testing.T, f runtest.Fixture, commandID uuid.UUID) string {
	t.Helper()
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, commandID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
