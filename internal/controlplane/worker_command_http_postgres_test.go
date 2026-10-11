package controlplane

import (
	"net/http"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"uuid"
)

type commandRouteFixture struct {
	agenttest.Fixture
	OrgID, EnvironmentID, WorkerID uuid.UUID
}
type commandRouteTarget struct {
	ID, InstanceID   uuid.UUID
	WriterGeneration int64
}

// The ordinary Command uses the current Computer lease with no customer bundle.
func commandHTTPFixture(t *testing.T) (commandRouteFixture, workerHTTPClient, commandRouteTarget) {
	t.Helper()
	base := agenttest.New(t)
	f := commandRouteFixture{Fixture: base, EnvironmentID: base.Environment, WorkerID: base.Worker}
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	f.OrgID = org
	created, err := command.Create(t.Context(), f.Pool, command.CreateRequest{OrgID: org, ProjectID: project, EnvironmentID: f.Environment, ComputerID: f.Computer, Creator: command.Creator{SubjectType: string(auth.PrincipalKindSession), SubjectID: f.User.String()}, Argv: []string{"true"}, IdempotencyKey: "command"})
	if err != nil {
		t.Fatal(err)
	}
	return f, newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.Worker), commandRouteTarget{ID: uuid.UUID(created.ID.Bytes), InstanceID: f.Computer, WriterGeneration: 1}
}

// Worker Command routes answer a claim, a log record and a completion through
// the command owner: changed authority is 409, rejected input 400.
func TestWorkerCommandRoutesMapOwnerErrors(t *testing.T) {
	f, worker, command := commandHTTPFixture(t)
	claim := workerapi.ComputerCommandClaimRequest{EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration}
	invalid := claim
	invalid.EnvironmentID = "org"
	worker.post(t, "/worker/v1/computer-commands/claim", invalid, http.StatusBadRequest, nil)
	stale := claim
	stale.WriterGeneration++
	worker.post(t, "/worker/v1/computer-commands/claim", stale, http.StatusConflict, nil)
	var started workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, &started)
	if started.Command == nil || started.Command.CommandID != command.ID.String() || started.Command.ComputerInstanceID != command.InstanceID.String() || started.Command.WriterGeneration != command.WriterGeneration || started.Command.RequestFingerprint == "" {
		t.Fatalf("claim = %+v", started)
	}
	running := claim
	running.ActiveCommandIDs = []string{command.ID.String()}
	if out := worker.post(t, "/worker/v1/computer-commands/claim", running, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("running Command claimed again: %s", out.Body.String())
	}

	log := workerapi.CommandLogAppendRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Stream: workerapi.LogStreamStdout, Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte("output")}
	var receipt workerapi.DiagnosticLogReceipt
	worker.post(t, "/worker/v1/computer-commands/logs/append", log, http.StatusOK, &receipt)
	if receipt.ThroughSequence != 1 || receipt.AcceptedAt.IsZero() || receipt.ExpiresAt.Sub(receipt.AcceptedAt) != 90*24*time.Hour {
		t.Fatal(receipt)
	}
	for name, mutate := range map[string]func(*workerapi.CommandLogAppendRequest){
		"identity":    func(r *workerapi.CommandLogAppendRequest) { r.CommandID = "command" },
		"stream":      func(r *workerapi.CommandLogAppendRequest) { r.Stream = workerapi.LogStream("structured") },
		"observation": func(r *workerapi.CommandLogAppendRequest) { r.ObservedAt = time.Time{} },
		"content": func(r *workerapi.CommandLogAppendRequest) {
			r.Content = make([]byte, 1025)
		},
	} {
		t.Run("log "+name, func(t *testing.T) {
			rejected := log
			rejected.ObservedSeq++
			rejected.ThroughSequence++
			mutate(&rejected)
			worker.post(t, "/worker/v1/computer-commands/logs/append", rejected, http.StatusBadRequest, nil)
		})
	}
	changed := log
	changed.Content = []byte("different")
	worker.post(t, "/worker/v1/computer-commands/logs/append", changed, http.StatusConflict, nil)

	code := int32(0)
	completion := workerapi.ComputerCommandCompleteRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Outcome: "exited", ExitCode: &code, Stdout: workerapi.CommandOutputBoundary{ThroughSequence: 2, Complete: true}, Stderr: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}}
	ambiguous := completion
	ambiguous.ExitCode = nil
	worker.post(t, "/worker/v1/computer-commands/complete", ambiguous, http.StatusBadRequest, nil)
	staleCompletion := completion
	staleCompletion.WriterGeneration++
	worker.post(t, "/worker/v1/computer-commands/complete", staleCompletion, http.StatusConflict, nil)
	worker.post(t, "/worker/v1/computer-commands/reconcile", staleCompletion, http.StatusConflict, nil)
	if out := worker.post(t, "/worker/v1/computer-commands/complete", completion, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("completion body = %s", out.Body.String())
	}

	worker.post(t, "/worker/v1/computer-commands/reconcile", completion, http.StatusConflict, nil)
	var tail workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, &tail)
	if tail.Command == nil || !tail.Command.TailOnly || tail.Command.CommandID != command.ID.String() || tail.Command.RequestFingerprint != started.Command.RequestFingerprint || len(tail.Command.Secrets) != 0 || len(tail.Command.Stdin) != 0 || string(tail.Command.Request) != "{}" {
		t.Fatalf("tail replay: %+v", tail)
	}
	for _, stream := range []workerapi.LogStream{workerapi.LogStreamStdout, workerapi.LogStreamStderr} {
		end := log
		end.Stream, end.Kind, end.Content, end.Complete = stream, "end", nil, true
		if stream == workerapi.LogStreamStdout {
			end.ObservedSeq, end.ThroughSequence = 2, 2
		}
		worker.post(t, "/worker/v1/computer-commands/logs/append", end, http.StatusOK, nil)
	}
	var released workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, &released)
	if released.Release == nil || released.Release.ComputerID == "" || released.Release.RequestFingerprint != started.Command.RequestFingerprint || released.Release.Completion.EnvironmentID != completion.EnvironmentID || released.Release.Completion.CommandID != completion.CommandID || released.Release.Completion.Outcome != "exited" || released.Release.Completion.ExitCode == nil || *released.Release.Completion.ExitCode != 0 {
		t.Fatalf("release = %+v", released)
	}
	worker.post(t, "/worker/v1/computer-commands/reconcile", released.Release.Completion, http.StatusOK, nil)
	if out := worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("reconciled Command claimed: %s", out.Body.String())
	}
}

func TestWorkerCommandClaimDeliversCancellation(t *testing.T) {
	f, worker, command := commandHTTPFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET computer_lease_epoch=1,status='stopping',started_at=now(),cancel_requested_at=now() WHERE id=$1`, command.ID)
	claim := workerapi.ComputerCommandClaimRequest{EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration}
	var response workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, &response)
	c := response.Cancellation
	if c == nil || response.Command != nil || c.CommandID != command.ID.String() || c.ComputerInstanceID != command.InstanceID.String() || c.WriterGeneration != command.WriterGeneration || c.RequestFingerprint == "" || c.ExpiresAt.IsZero() {
		t.Fatalf("cancellation = %+v", response)
	}
	claim.ActiveCancellationIDs = []string{command.ID.String()}
	claim.ActiveCommandIDs = []string{command.ID.String()}
	if out := worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, nil); out.Body.String() != "{}\n" {
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
		"/worker/v1/computer-commands/claim":       workerapi.ComputerCommandClaimRequest{EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration},
		"/worker/v1/computer-commands/complete":    workerapi.ComputerCommandCompleteRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Outcome: "exited", ExitCode: &code, Stdout: workerapi.CommandOutputBoundary{ThroughSequence: 2, Complete: true}, Stderr: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}},
		"/worker/v1/computer-commands/logs/append": workerapi.CommandLogAppendRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: command.ID.String(), ComputerInstanceID: command.InstanceID.String(), WriterGeneration: command.WriterGeneration, Stream: workerapi.LogStreamStdout, Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now(), Content: []byte("output")},
	} {
		worker.post(t, path, body, http.StatusUnauthorized, nil)
	}
}
