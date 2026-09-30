package command

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// A claim grants a requested cancellation first, then releases a terminal
// Command, then starts one, skipping what the host reports it already runs.
func TestClaimSelectsCancellationReleaseAndStart(t *testing.T) {
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, lease.RunID)
	bound := commandtest.Bound(t, f, lease.LeaseID, "starting")
	worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	request := ClaimRequest{OrgID: f.OrgID, EnvironmentID: f.EnvironmentID, InstanceID: bound.InstanceID, WriterGeneration: bound.WriterGeneration}
	claim := func(request ClaimRequest) ClaimResult {
		t.Helper()
		result, err := Claim(t.Context(), f.Pool, worker, request)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	var fingerprint []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT request_fingerprint FROM idempotency_claims WHERE id=$1`, bound.ClaimID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}

	started := claim(request)
	if started.Start == nil || started.Cancellation != nil || started.Release != nil || started.Start.Command.Status != "running" || !bytes.Equal(started.Start.RequestFingerprint, fingerprint) || started.Start.Instance.WriterGeneration != bound.WriterGeneration {
		t.Fatalf("start = %+v", started)
	}
	running := request
	running.ActiveCommandIDs = []uuid.UUID{bound.ID}
	if idle := claim(running); idle != (ClaimResult{}) {
		t.Fatalf("running Command claimed again: %+v", idle)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, bound.ID)
	cancelled := claim(running)
	if c := cancelled.Cancellation; c == nil || cancelled.Start != nil || c.CommandID != bound.ID || c.InstanceID != bound.InstanceID || c.WriterGeneration != bound.WriterGeneration || !bytes.Equal(c.RequestFingerprint, fingerprint) || c.ExpiresAt.IsZero() {
		t.Fatalf("cancellation = %+v", cancelled)
	}
	delivering := running
	delivering.ActiveCancellationIDs = []uuid.UUID{bound.ID}
	if idle := claim(delivering); idle != (ClaimResult{}) {
		t.Fatalf("delivered cancellation claimed again: %+v", idle)
	}

	report := CompletionReport{OrgID: f.OrgID, CommandID: bound.ID, InstanceID: bound.InstanceID, WriterGeneration: bound.WriterGeneration, Outcome: "computer_command_cancelled"}
	if err := Complete(t.Context(), f.Pool, worker, report); err != nil {
		t.Fatal(err)
	}
	released := claim(request)
	if r := released.Release; r == nil || released.Start != nil || released.Cancellation != nil || !bytes.Equal(r.RequestFingerprint, fingerprint) || r.Completion.CommandID != bound.ID || r.Completion.Outcome != "computer_command_cancelled" || r.Completion.OrgID != f.OrgID || r.Completion.WriterGeneration != bound.WriterGeneration {
		t.Fatalf("release = %+v", released)
	}
	if err := Reconcile(t.Context(), f.Pool, worker, released.Release.Completion); err != nil {
		t.Fatal(err)
	}
	if idle := claim(request); idle != (ClaimResult{}) {
		t.Fatalf("reconciled Command claimed: %+v", idle)
	}

	foreign := request
	foreign.OrgID = uuid.NewV7()
	if _, err := Claim(t.Context(), f.Pool, worker, foreign); !errors.Is(err, ErrChanged) {
		t.Fatalf("foreign organization claim: %v", err)
	}
}
