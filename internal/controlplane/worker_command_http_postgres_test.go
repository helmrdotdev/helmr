package controlplane

import (
	"net/http"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// commandHTTPFixture serves NewServer to the worker host of a starting
// Command bound to a running lease's Instance.
func commandHTTPFixture(t *testing.T) (runtest.Fixture, workerHTTPClient, commandtest.Command) {
	t.Helper()
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, lease.RunID)
	command := commandtest.Bound(t, f, lease.LeaseID, "starting")
	return f, newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.WorkerID), command
}

// Worker Command routes answer a claim, a log record and a completion through
// the command owner: changed authority is 409, rejected input 400.
func TestWorkerCommandRoutesMapOwnerErrors(t *testing.T) {
	f, worker, command := commandHTTPFixture(t)
	claim := workerapi.ComputerCommandClaimRequest{OrgID: f.OrgID.String(), EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration}
	invalid := claim
	invalid.OrgID = "org"
	worker.post(t, "/worker/v1/run/computer-commands/claim", invalid, http.StatusBadRequest, nil)
	stale := claim
	stale.WriterGeneration++
	worker.post(t, "/worker/v1/run/computer-commands/claim", stale, http.StatusConflict, nil)
	var started workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/run/computer-commands/claim", claim, http.StatusOK, &started)
	if started.Command == nil || started.Command.CommandID != command.ID.String() || started.Command.ComputerInstanceID != command.InstanceID.String() || started.Command.WriterGeneration != command.WriterGeneration || started.Command.RequestFingerprint == "" {
		t.Fatalf("claim = %+v", started)
	}
	running := claim
	running.ActiveCommandIDs = []string{command.ID.String()}
	if out := worker.post(t, "/worker/v1/run/computer-commands/claim", running, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("running Command claimed again: %s", out.Body.String())
	}

	log := workerapi.CommandLogAppendRequest{OrgID: f.OrgID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Stream: workerapi.LogStreamStdout, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte("output")}
	worker.post(t, "/worker/v1/run/computer-commands/logs/append", log, http.StatusNoContent, nil)
	for name, mutate := range map[string]func(*workerapi.CommandLogAppendRequest){
		"identity":    func(r *workerapi.CommandLogAppendRequest) { r.CommandID = "command" },
		"stream":      func(r *workerapi.CommandLogAppendRequest) { r.Stream = workerapi.LogStreamStructured },
		"observation": func(r *workerapi.CommandLogAppendRequest) { r.ObservedAt = time.Time{} },
		"content": func(r *workerapi.CommandLogAppendRequest) {
			r.Content = make([]byte, telemetry.MaxRunLogContentBytes+1)
		},
	} {
		t.Run("log "+name, func(t *testing.T) {
			rejected := log
			rejected.ObservedSeq++
			mutate(&rejected)
			worker.post(t, "/worker/v1/run/computer-commands/logs/append", rejected, http.StatusBadRequest, nil)
		})
	}
	changed := log
	changed.Content = []byte("different")
	worker.post(t, "/worker/v1/run/computer-commands/logs/append", changed, http.StatusConflict, nil)

	code := int32(0)
	completion := workerapi.ComputerCommandCompleteRequest{OrgID: f.OrgID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Outcome: "exited", ExitCode: &code}
	ambiguous := completion
	ambiguous.ExitCode = nil
	worker.post(t, "/worker/v1/run/computer-commands/complete", ambiguous, http.StatusBadRequest, nil)
	staleCompletion := completion
	staleCompletion.WriterGeneration++
	worker.post(t, "/worker/v1/run/computer-commands/complete", staleCompletion, http.StatusConflict, nil)
	worker.post(t, "/worker/v1/run/computer-commands/reconcile", staleCompletion, http.StatusConflict, nil)
	if out := worker.post(t, "/worker/v1/run/computer-commands/complete", completion, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("completion body = %s", out.Body.String())
	}

	var released workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/run/computer-commands/claim", claim, http.StatusOK, &released)
	if released.Release == nil || released.Release.ComputerID == "" || released.Release.RequestFingerprint != started.Command.RequestFingerprint || released.Release.Completion.OrgID != completion.OrgID || released.Release.Completion.CommandID != completion.CommandID || released.Release.Completion.Outcome != "exited" || released.Release.Completion.ExitCode == nil || *released.Release.Completion.ExitCode != 0 {
		t.Fatalf("release = %+v", released)
	}
	worker.post(t, "/worker/v1/run/computer-commands/reconcile", released.Release.Completion, http.StatusOK, nil)
	if out := worker.post(t, "/worker/v1/run/computer-commands/claim", claim, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("reconciled Command claimed: %s", out.Body.String())
	}
}

func TestWorkerCommandClaimDeliversCancellation(t *testing.T) {
	f, worker, command := commandHTTPFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',started_at=now(),cancel_requested_at=now() WHERE id=$1`, command.ID)
	claim := workerapi.ComputerCommandClaimRequest{OrgID: f.OrgID.String(), EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration}
	var response workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/run/computer-commands/claim", claim, http.StatusOK, &response)
	c := response.Cancellation
	if c == nil || response.Command != nil || c.CommandID != command.ID.String() || c.ComputerInstanceID != command.InstanceID.String() || c.WriterGeneration != command.WriterGeneration || c.RequestFingerprint == "" || c.ExpiresAt.IsZero() {
		t.Fatalf("cancellation = %+v", response)
	}
	claim.ActiveCancellationIDs = []string{command.ID.String()}
	claim.ActiveCommandIDs = []string{command.ID.String()}
	if out := worker.post(t, "/worker/v1/run/computer-commands/claim", claim, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("delivered cancellation claimed again: %s", out.Body.String())
	}
}

// Worker Command routes ask a host whose credential claims went stale to
// re-authenticate.
func TestWorkerCommandRoutesRejectStaleClaims(t *testing.T) {
	f, worker, command := commandHTTPFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, f.WorkerID)
	code := int32(0)
	for path, body := range map[string]any{
		"/worker/v1/run/computer-commands/claim":       workerapi.ComputerCommandClaimRequest{OrgID: f.OrgID.String(), EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration},
		"/worker/v1/run/computer-commands/complete":    workerapi.ComputerCommandCompleteRequest{OrgID: f.OrgID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Outcome: "exited", ExitCode: &code},
		"/worker/v1/run/computer-commands/logs/append": workerapi.CommandLogAppendRequest{OrgID: f.OrgID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Stream: workerapi.LogStreamStdout, ObservedAt: time.Now(), Content: []byte("output")},
	} {
		worker.post(t, path, body, http.StatusUnauthorized, nil)
	}
}
