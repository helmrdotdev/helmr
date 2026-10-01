package run

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func TestFailSecretRevokedRunsFailsRevokedRun(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, work.LeaseID, secretID, 1)
	f.ResolveRunSecret(t, work, secretID)
	f.RevokeSecret(t, secretID, 1)
	revocation := secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}

	examined, err := FailSecretRevokedRuns(t.Context(), f.Pool, revocation, 10)
	if err != nil || examined != 1 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	var runStatus, runReason, attemptReason, leaseStatus string
	if err := f.Pool.QueryRow(t.Context(), `
SELECT runs.status, runs.failure->>'code', run_attempts.terminal_reason_code, run_leases.status
  FROM runs
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases ON run_leases.id = $2
 WHERE runs.id = $1`, work.RunID, work.LeaseID).Scan(&runStatus, &runReason, &attemptReason, &leaseStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" || runReason != "secret_revoked" || attemptReason != "secret_revoked" || leaseStatus != "failed" {
		t.Fatalf("revoked Run = run:%s/%s attempt:%s lease:%s", runStatus, runReason, attemptReason, leaseStatus)
	}
	examined, err = FailSecretRevokedRuns(t.Context(), f.Pool, revocation, 10)
	if err != nil || examined != 0 {
		t.Fatalf("drained batch = %d, %v", examined, err)
	}
}

// A candidate whose Computer no longer places the Secret at the revoked
// generation is examined and left unchanged.
func TestFailSecretRevokedRunsCountsStaleCandidates(t *testing.T) {
	for name, revoke := range map[string]func(*testing.T, runtest.Fixture, uuid.UUID){
		"superseded generation": func(t *testing.T, f runtest.Fixture, secretID uuid.UUID) { f.RevokeSecret(t, secretID, 2) },
		"secret not revoked":    func(*testing.T, runtest.Fixture, uuid.UUID) {},
	} {
		t.Run(name, func(t *testing.T) {
			f := runtest.New(t)
			secretID := f.AddSecret(t, "revoked-secret")
			runs := []runtest.RunLease{
				f.AddRunLease(t, "running", time.Now().Add(-time.Minute)),
				f.AddRunLease(t, "running", time.Now().Add(-time.Minute)),
			}
			for _, work := range runs {
				f.PlaceSecret(t, work.LeaseID, secretID, 1)
				f.ResolveRunSecret(t, work, secretID)
			}
			revoke(t, f, secretID)

			examined, err := FailSecretRevokedRuns(t.Context(), f.Pool, secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}, 10)
			if err != nil || examined != 2 {
				t.Fatalf("batch = %d, %v", examined, err)
			}
			for _, work := range runs {
				if status := secretRevocationRunStatus(t, f, work.RunID); status == "failed" {
					t.Fatalf("stale candidate Run status = %s", status)
				}
			}
		})
	}
}

func TestFailSecretRevokedRunsReturnsPartialCountOnFailure(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	first := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	overplaced := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	last := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, first.LeaseID, secretID, 1)
	f.PlaceSecret(t, overplaced.LeaseID, secretID, 65)
	f.PlaceSecret(t, last.LeaseID, secretID, 1)
	for _, work := range []runtest.RunLease{first, overplaced, last} {
		f.ResolveRunSecret(t, work, secretID)
	}
	f.RevokeSecret(t, secretID, 1)
	revocation := secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}

	examined, err := FailSecretRevokedRuns(t.Context(), f.Pool, revocation, 10)
	if err == nil || examined != 1 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	if status := secretRevocationRunStatus(t, f, first.RunID); status != "failed" {
		t.Fatalf("first Run status = %s", status)
	}
	if status := secretRevocationRunStatus(t, f, last.RunID); status == "failed" {
		t.Fatalf("Run after the failure status = %s", status)
	}
	examined, err = FailSecretRevokedRuns(t.Context(), f.Pool, revocation, 10)
	if err == nil || examined != 0 {
		t.Fatalf("retried batch = %d, %v", examined, err)
	}
}

func TestFailSecretRevokedRunsStopsAtLimit(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	for range 3 {
		work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
		f.PlaceSecret(t, work.LeaseID, secretID, 1)
		f.ResolveRunSecret(t, work, secretID)
	}
	f.RevokeSecret(t, secretID, 1)
	revocation := secret.Revocation{EnvironmentID: f.EnvironmentID, SecretID: secretID, Generation: 1}

	for _, want := range []int{2, 1, 0} {
		examined, err := FailSecretRevokedRuns(t.Context(), f.Pool, revocation, 2)
		if err != nil || examined != want {
			t.Fatalf("batch = %d, %v; want %d", examined, err, want)
		}
	}
}

func secretRevocationRunStatus(t *testing.T, f runtest.Fixture, runID uuid.UUID) string {
	t.Helper()
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM runs WHERE id=$1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
