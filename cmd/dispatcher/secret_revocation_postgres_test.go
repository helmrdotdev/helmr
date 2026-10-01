package main

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// A revocation batch fails revoked Runs before it stops revoked Commands,
// and the Commands share the limit the Runs left.
func TestSecretRevocationBatchFailsRunsBeforeStoppingCommands(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	first := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	second := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	host := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	for _, work := range []runtest.RunLease{first, second, host} {
		f.PlaceSecret(t, work.LeaseID, secretID, 1)
	}
	f.ResolveRunSecret(t, first, secretID)
	f.ResolveRunSecret(t, second, secretID)
	early := commandtest.Bound(t, f, host.LeaseID, "running")
	late := commandtest.Bound(t, f, host.LeaseID, "running")
	commandtest.ResolveSecret(t, f, early.ID, secretID)
	commandtest.ResolveSecret(t, f, late.ID, secretID)
	f.RevokeSecret(t, secretID, 1)
	reconcile := reconcileSecretRevocation(f.Pool)

	examined, err := reconcile(t.Context(), f.EnvironmentID, secretID, 1, 3)
	if err != nil || examined != 3 {
		t.Fatalf("first batch = %d, %v", examined, err)
	}
	for _, work := range []runtest.RunLease{first, second} {
		if status := runStatus(t, f, work.RunID); status != "failed" {
			t.Fatalf("revoked Run status = %s", status)
		}
	}
	if status := commandStatus(t, f, early.ID); status != "stopping" {
		t.Fatalf("first revoked Command status = %s", status)
	}
	if status := commandStatus(t, f, late.ID); status != "running" {
		t.Fatalf("Command beyond the limit status = %s", status)
	}
	examined, err = reconcile(t.Context(), f.EnvironmentID, secretID, 1, 3)
	if err != nil || examined != 1 {
		t.Fatalf("second batch = %d, %v", examined, err)
	}
	if status := commandStatus(t, f, late.ID); status != "stopping" {
		t.Fatalf("second revoked Command status = %s", status)
	}
	examined, err = reconcile(t.Context(), f.EnvironmentID, secretID, 1, 3)
	if err != nil || examined != 0 {
		t.Fatalf("drained batch = %d, %v", examined, err)
	}
}

// A Command fence failure after a Run was examined returns the Run in the
// partial count.
func TestSecretRevocationBatchReturnsPartialCountOnCommandFailure(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	host := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, work.LeaseID, secretID, 1)
	f.PlaceSecret(t, host.LeaseID, secretID, 65)
	f.ResolveRunSecret(t, work, secretID)
	bound := commandtest.Bound(t, f, host.LeaseID, "running")
	commandtest.ResolveSecret(t, f, bound.ID, secretID)
	f.RevokeSecret(t, secretID, 1)

	examined, err := reconcileSecretRevocation(f.Pool)(t.Context(), f.EnvironmentID, secretID, 1, 10)
	if err == nil || examined != 1 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	if status := runStatus(t, f, work.RunID); status != "failed" {
		t.Fatalf("revoked Run status = %s", status)
	}
	if status := commandStatus(t, f, bound.ID); status != "running" {
		t.Fatalf("unfenced Command status = %s", status)
	}
}

// A batch the Runs fill does not reach the Commands.
func TestSecretRevocationBatchFilledByRunsLeavesCommands(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	first := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	second := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	host := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	for _, work := range []runtest.RunLease{first, second, host} {
		f.PlaceSecret(t, work.LeaseID, secretID, 1)
	}
	f.ResolveRunSecret(t, first, secretID)
	f.ResolveRunSecret(t, second, secretID)
	bound := commandtest.Bound(t, f, host.LeaseID, "running")
	commandtest.ResolveSecret(t, f, bound.ID, secretID)
	f.RevokeSecret(t, secretID, 1)

	examined, err := reconcileSecretRevocation(f.Pool)(t.Context(), f.EnvironmentID, secretID, 1, 2)
	if err != nil || examined != 2 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	for _, work := range []runtest.RunLease{first, second} {
		if status := runStatus(t, f, work.RunID); status != "failed" {
			t.Fatalf("revoked Run status = %s", status)
		}
	}
	if status := commandStatus(t, f, bound.ID); status != "running" {
		t.Fatalf("Command beyond a full batch status = %s", status)
	}
}

// A Run failure returns the Runs examined before it and leaves the Commands
// unchanged.
func TestSecretRevocationBatchReturnsPartialCountOnRunFailure(t *testing.T) {
	f := runtest.New(t)
	secretID := f.AddSecret(t, "revoked-secret")
	first := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	overplaced := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	host := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	f.PlaceSecret(t, first.LeaseID, secretID, 1)
	f.PlaceSecret(t, overplaced.LeaseID, secretID, 65)
	f.PlaceSecret(t, host.LeaseID, secretID, 1)
	f.ResolveRunSecret(t, first, secretID)
	f.ResolveRunSecret(t, overplaced, secretID)
	bound := commandtest.Bound(t, f, host.LeaseID, "running")
	commandtest.ResolveSecret(t, f, bound.ID, secretID)
	f.RevokeSecret(t, secretID, 1)

	examined, err := reconcileSecretRevocation(f.Pool)(t.Context(), f.EnvironmentID, secretID, 1, 10)
	if err == nil || examined != 1 {
		t.Fatalf("batch = %d, %v", examined, err)
	}
	if status := runStatus(t, f, first.RunID); status != "failed" {
		t.Fatalf("first revoked Run status = %s", status)
	}
	if status := runStatus(t, f, overplaced.RunID); status == "failed" {
		t.Fatalf("failing Run status = %s", status)
	}
	if status := commandStatus(t, f, bound.ID); status != "running" {
		t.Fatalf("Command after a Run failure status = %s", status)
	}
}

func runStatus(t *testing.T, f runtest.Fixture, runID uuid.UUID) string {
	t.Helper()
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM runs WHERE id=$1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func commandStatus(t *testing.T, f runtest.Fixture, commandID uuid.UUID) string {
	t.Helper()
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, commandID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
