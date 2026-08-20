package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestWorkerLifecycleClient(t *testing.T) {
	claim := workerapi.RunLease{
		ID: "00000000-0000-0000-0000-000000000001", RunID: "00000000-0000-0000-0000-000000000002",
		WorkerGroupID: "run-us-east-1", WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
		WorkerEpoch: 1, LeaseSequence: 1, RuntimeInstanceID: "00000000-0000-0000-0000-000000000501",
		AttemptNumber: 1, ExpiresAt: time.Date(2026, 5, 8, 12, 5, 0, 0, time.UTC),
	}
	paths := []string{}
	workerToken := "worker-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/worker/v0/instance/token":
			if got := r.Header.Get("authorization"); got != "" {
				t.Fatalf("worker token request auth = %s", got)
			}
			var request workerapi.TokenRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || request.WorkerInstanceSecret != "worker-secret" || request.ServiceID != "00000000-0000-0000-0000-000000000901" {
				t.Fatalf("worker token request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
				Token:            workerToken,
				ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/api/worker/v0/run/leases/discover":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.RunLeaseDiscoveryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(workerapi.RunLeaseDiscoveryResponse{
				Items: []workerapi.RunLeaseWork{{
					LeaseID:       claim.ID,
					LeaseSequence: claim.LeaseSequence,
				}},
			})
		case "/api/worker/v0/instance/activate":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.ActivateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Capabilities.Runtime.Arch != "arm64" {
				t.Fatalf("activate capabilities = %+v", request.Capabilities)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusActive})
		case "/api/worker/v0/instance/drain":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusDraining, ActiveExecutions: 1})
		case "/api/worker/v0/instance/drain/complete":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.DrainCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() || len(request.Inventory) != 0 {
				t.Fatalf("worker drain completion = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusTerminationReady})
		case "/api/worker/v0/instance":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusDraining, ActiveExecutions: 1})
		case "/api/worker/v0/instance/fence":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.FenceRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ReasonCode != "termination_drain_failed" {
				t.Fatalf("fence reason = %q", request.ReasonCode)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := client.DiscoverRunLeases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered.Items) != 1 ||
		discovered.Items[0].LeaseID != claim.ID ||
		discovered.Items[0].LeaseSequence != claim.LeaseSequence {
		t.Fatalf("discovered = %+v", discovered)
	}
	if status, err := client.ActivateWorker(context.Background(), workerClientCapabilities()); err != nil || status.Status != workerapi.StatusActive {
		t.Fatalf("activate status = %+v err=%v", status, err)
	}
	if status, err := client.DrainWorker(context.Background()); err != nil || status.Status != workerapi.StatusDraining || status.ActiveExecutions != 1 {
		t.Fatalf("drain status = %+v err=%v", status, err)
	}
	if status, err := client.GetWorkerStatus(context.Background()); err != nil || status.Status != workerapi.StatusDraining || status.ActiveExecutions != 1 {
		t.Fatalf("worker status = %+v err=%v", status, err)
	}
	if status, err := client.CompleteWorkerDrain(context.Background(), workerapi.DrainCompletionRequest{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        time.Now().UTC(),
		Inventory:         []string{},
	}); err != nil || status.Status != workerapi.StatusTerminationReady {
		t.Fatalf("complete worker drain status = %+v, err = %v", status, err)
	}
	if err := client.FenceWorker(context.Background(), "termination_drain_failed"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got != "/api/worker/v0/instance/token,/api/worker/v0/run/leases/discover,/api/worker/v0/instance/activate,/api/worker/v0/instance/drain,/api/worker/v0/instance,/api/worker/v0/instance/drain/complete,/api/worker/v0/instance/fence" {
		t.Fatalf("paths = %s", got)
	}
}

func TestWorkerRunLeaseClaimProtocolClient(t *testing.T) {
	receipt := workerapi.RunLeaseAssignment{
		ID:                     "00000000-0000-0000-0000-000000000001",
		RunID:                  "00000000-0000-0000-0000-000000000002",
		AttemptNumber:          1,
		LeaseSequence:          3,
		BaseWorkspaceVersionID: "00000000-0000-0000-0000-000000000003",
	}
	operationID := "00000000-0000-0000-0000-000000000004"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch r.URL.Path {
			case "/api/worker/v0/instance/token":
				_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
					Token:            "worker-token",
					ExpiresInSeconds: 3600,
				})
			case "/api/worker/v0/run/leases/claim":
				var request workerapi.RunLeaseClaimRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.LeaseID != receipt.ID ||
					request.LeaseSequence != receipt.LeaseSequence {
					t.Fatalf("claim request = %+v", request)
				}
				_ = json.NewEncoder(w).Encode(
					workerapi.RunLeaseClaimResponse{
						Lease: receipt,
						Workspace: workerapi.WorkspaceAttachment{ResetTarget: workerapi.WorkspaceResetTarget{
							BaseWorkspaceVersionID: receipt.BaseWorkspaceVersionID,
							Tree: workerapi.WorkspaceTreeIdentity{
								Digest: workspace.CanonicalEmptyTreeDigest,
							},
							Empty: &workerapi.EmptyWorkspace{},
						}},
						Execution: workerapi.RunLeaseExecution{
							Fresh: &workerapi.RunLeaseFresh{
								ProgramStart: []byte("frame"),
							},
						},
					},
				)
			case "/api/worker/v0/run/leases/start":
				var request workerapi.RunStartRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() || request.Fresh == nil ||
					request.Restore != nil {
					t.Fatalf("start request = %+v", request)
				}
				_ = json.NewEncoder(w).Encode(
					workerapi.RunStartResponse{Lease: receipt.Fence()},
				)
			case "/api/worker/v0/run/leases/entrypoint":
				var request workerapi.RunEntrypointRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() ||
					request.EntrypointKind != "task" ||
					request.EntrypointDeclaredID != "deploy" {
					t.Fatalf("entrypoint request = %+v", request)
				}
				w.WriteHeader(http.StatusNoContent)
			case "/api/worker/v0/run/leases/renew":
				var request workerapi.RunLeaseRenewRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() ||
					!request.ExpectedExpiresAt.Equal(receipt.ExpiresAt) {
					t.Fatalf("renew request = %+v", request)
				}
				_ = json.NewEncoder(w).Encode(workerapi.RunLeaseRenewResponse{
					Lease: receipt.Fence(), ExpiresAt: receipt.ExpiresAt,
					BaseWorkspaceVersionID: receipt.BaseWorkspaceVersionID,
				})
			case "/api/worker/v0/run/logs/append":
				var request workerapi.RunLogAppendRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() ||
					request.Stream != workerapi.LogStreamStdout ||
					request.ObservedSeq != 7 ||
					request.ContentBase64 != "bG9n" {
					t.Fatalf("log request = %+v", request)
				}
				w.WriteHeader(http.StatusNoContent)
			case "/api/worker/v0/run/finalization/begin":
				var request workerapi.BeginRunFinalizationRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() ||
					request.OperationID != operationID ||
					request.Kind != workerapi.RunFinalizationCapture ||
					request.ProgramQuiesced.RunID != receipt.RunID ||
					request.ProgramQuiesced.AttemptNumber != receipt.AttemptNumber ||
					request.ProgramQuiesced.RunLeaseID != receipt.ID {
					t.Fatalf("finalization request = %+v", request)
				}
				_ = json.NewEncoder(w).Encode(
					workerapi.BeginRunFinalizationResponse{
						Lease: receipt.Fence(), BaseWorkspaceVersionID: receipt.BaseWorkspaceVersionID,
						ExpiresAt: receipt.ExpiresAt, OperationID: operationID,
						Kind: workerapi.RunFinalizationCapture,
					},
				)
			case "/api/worker/v0/run/tasks/complete":
				var request workerapi.CompleteTaskRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() ||
					request.Outcome.Succeeded == nil ||
					string(request.Outcome.Succeeded.Output) != `{"ok":true}` ||
					request.Workspace.Captured == nil ||
					request.Workspace.Captured.Receipt.OperationID != operationID {
					t.Fatalf("Task completion request = %+v", request)
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		},
	))
	defer server.Close()
	client, err := New(
		server.URL,
		WithHTTPClient(server.Client()),
		WithAuth(
			"00000000-0000-0000-0000-000000000401",
			"worker-secret",
		),
		WithService(
			"00000000-0000-0000-0000-000000000901",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.ClaimRunLease(
		context.Background(),
		workerapi.RunLeaseWork{
			LeaseID:       receipt.ID,
			LeaseSequence: receipt.LeaseSequence,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Lease != receipt ||
		claim.Execution.Fresh == nil ||
		string(claim.Execution.Fresh.ProgramStart) != "frame" {
		t.Fatalf("claim response = %+v", claim)
	}
	started, err := client.AcknowledgeRunStart(
		context.Background(),
		workerapi.RunStartRequest{Lease: receipt.Fence(), Fresh: &workerapi.RunStartFresh{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if started.Lease != receipt.Fence() {
		t.Fatalf("start response = %+v", started)
	}
	if err := client.AcknowledgeRunEntrypoint(
		context.Background(),
		workerapi.RunEntrypointRequest{
			Lease:                receipt.Fence(),
			EntrypointKind:       "task",
			EntrypointDeclaredID: "deploy",
		},
	); err != nil {
		t.Fatal(err)
	}
	renewed, err := client.RenewRunLease(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Lease != receipt.Fence() {
		t.Fatalf("renew response = %+v", renewed)
	}
	finalization, err := client.BeginRunFinalization(
		context.Background(),
		workerapi.BeginRunFinalizationRequest{
			Lease: receipt.Fence(),
			ProgramQuiesced: workerapi.RunQuiescenceProof{
				RunID: receipt.RunID, AttemptNumber: receipt.AttemptNumber,
				RunLeaseID: receipt.ID,
			},
			OperationID: operationID,
			Kind:        workerapi.RunFinalizationCapture,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if finalization.Lease != receipt.Fence() ||
		finalization.OperationID != operationID ||
		finalization.Kind != workerapi.RunFinalizationCapture {
		t.Fatalf("finalization response = %+v", finalization)
	}
	if err := client.CompleteTask(
		context.Background(),
		workerapi.CompleteTaskRequest{
			Lease: receipt.Fence(),
			Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
				Output: json.RawMessage(`{"ok":true}`),
			}},
			Workspace: workerapi.TaskWorkspaceProof{Captured: &workerapi.TaskWorkspaceCapture{
				Receipt: workerapi.WorkspaceFinalizationReceipt{OperationID: operationID},
			}},
		},
	); err != nil {
		t.Fatal(err)
	}
	err = client.AppendRunLog(
		context.Background(),
		receipt,
		workerapi.LogStreamStdout,
		7,
		[]byte("log"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got !=
		"/api/worker/v0/instance/token,/api/worker/v0/run/leases/claim,"+
			"/api/worker/v0/run/leases/start,/api/worker/v0/run/leases/entrypoint,/api/worker/v0/run/leases/renew,"+
			"/api/worker/v0/run/finalization/begin,/api/worker/v0/run/tasks/complete,"+
			"/api/worker/v0/run/logs/append" {
		t.Fatalf("paths = %s", got)
	}
}

func TestCompleteWorkerDrainRetriesTheIdenticalProofAfterAmbiguousResponse(t *testing.T) {
	attempts := 0
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/v0/instance/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "worker-token", ExpiresInSeconds: 3600})
		case "/api/worker/v0/instance/drain/complete":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
			attempts++
			if attempts == 1 {
				http.Error(w, "ambiguous upstream failure", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusTerminationReady})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("worker", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.DrainCompletionRequest{
		InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0",
		ObservedAt: time.Now().UTC(), Inventory: []string{},
	}
	status, err := client.CompleteWorkerDrain(context.Background(), request)
	if err != nil || status.Status != workerapi.StatusTerminationReady {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	if attempts != 2 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("attempts = %d, request bodies differ: %q != %q", attempts, bodies[0], bodies[1])
	}
}

func TestFenceWorkerRetriesTheIdenticalRequestAfterAmbiguousResponse(t *testing.T) {
	attempts := 0
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/v0/instance/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "worker-token", ExpiresInSeconds: 3600})
		case "/api/worker/v0/instance/fence":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
			attempts++
			if attempts == 1 {
				http.Error(w, "ambiguous upstream failure", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("worker", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.FenceWorker(context.Background(), "termination_drain_failed"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("attempts = %d, request bodies differ: %q != %q", attempts, bodies[0], bodies[1])
	}
}

func TestWorkerClientRefreshesTokenAndReplaysBufferedRequestAfterUnauthorized(t *testing.T) {
	var tokenRequests int
	var activateBodies [][]byte
	var statusRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/v0/instance/token":
			tokenRequests++
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
				Token: fmt.Sprintf("worker-token-%d", tokenRequests), ExpiresInSeconds: 3600,
			})
		case "/api/worker/v0/instance/activate":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			activateBodies = append(activateBodies, body)
			if r.Header.Get("authorization") == "Bearer worker-token-1" {
				http.Error(w, `{"error":"stale token"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
		case "/api/worker/v0/instance":
			statusRequests++
			if statusRequests == 1 {
				http.Error(w, `{"error":"stale group claims"}`, http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("authorization"); got != "Bearer worker-token-3" {
				t.Fatalf("refreshed status authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()),
		WithAuth("00000000-0000-0000-0000-000000000401", "worker-secret"),
		WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ActivateWorker(context.Background(), workerClientCapabilities()); err != nil {
		t.Fatal(err)
	}
	if len(activateBodies) != 2 || !bytes.Equal(activateBodies[0], activateBodies[1]) {
		t.Fatalf("activate request was not replayed exactly: %q", activateBodies)
	}
	if _, err := client.GetWorkerStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tokenRequests != 3 || statusRequests != 2 {
		t.Fatalf("token requests=%d status requests=%d, want 3 and 2", tokenRequests, statusRequests)
	}
}

func TestWorkerRunWaitClient(t *testing.T) {
	claim := workerapi.RunLeaseAssignment{
		ID: "00000000-0000-0000-0000-000000000001", RunID: "00000000-0000-0000-0000-000000000002",
		WorkerGroupID: "run-us-east-1", WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
		WorkerEpoch: 1, LeaseSequence: 1, RuntimeInstanceID: "00000000-0000-0000-0000-000000000501",
		AttemptNumber: 1, WorkspaceID: "00000000-0000-0000-0000-000000000701",
		WorkspaceMountID:       "00000000-0000-0000-0000-000000000702",
		WorkspaceLeaseID:       "00000000-0000-0000-0000-000000000703",
		BaseWorkspaceVersionID: "00000000-0000-0000-0000-000000000704",
		ExpiresAt:              time.Date(2026, 5, 8, 12, 5, 0, 0, time.UTC),
	}
	kernelDigest := "sha256:kernel"
	rootfsDigest := "sha256:rootfs"
	configDigest := "sha256:runtime-config"
	manifestDigest := "sha256:manifest"
	vmStateDigest := "sha256:state"
	memoryDigest := "sha256:memory"
	scratchDigest := "sha256:scratch"
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/worker/v0/instance/token" {
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "worker-token", ExpiresInSeconds: int64(time.Hour / time.Second)})
			return
		}
		if got := r.Header.Get("authorization"); got != "Bearer worker-token" {
			t.Fatalf("worker auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/worker/v0/run/waits/create":
			var request workerapi.CreateRunWaitRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.CorrelationID != "corr-1" || request.Kind != workerapi.RunWaitKindToken || string(request.Params) != `{"prompt":"ship?"}` {
				t.Fatalf("create run wait = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.CreateRunWaitResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1"})
		case "/api/worker/v0/run/waits/poll":
			var request workerapi.RunWaitPollRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RunWaitID != "run-wait-id-1" {
				t.Fatalf("poll run wait request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.RunWaitPollResponse{
				RunID: claim.RunID, RunWaitID: request.RunWaitID, Status: "resume_requested",
				RequestVersion: 7, ResumeKind: "completed", ResumePayload: json.RawMessage(`{"approved":true}`),
			})
		case "/api/worker/v0/run/waits/resume-ack":
			var request workerapi.RunWaitResumeAckRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RunWaitID != "run-wait-id-1" || request.ResumeRequestVersion != 7 {
				t.Fatalf("resume ack request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.RunWaitResumeAckResponse{
				RunID: claim.RunID, RunWaitID: request.RunWaitID, ResumeRequestVersion: request.ResumeRequestVersion,
			})
		case "/api/worker/v0/run/checkpoints/ready":
			var request workerapi.CheckpointReadyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RequestVersion != 42 || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" {
				t.Fatalf("checkpoint ready request = %+v", request)
			}
			if request.SourceCleanup == nil || request.SourceCleanup.Method != workerapi.RuntimeCleanupSessionClosed {
				t.Fatalf("checkpoint source cleanup = %+v", request.SourceCleanup)
			}
			if request.Manifest.RecoveryPoint.Runtime.KernelDigest != kernelDigest || request.Manifest.RecoveryPoint.Runtime.RootfsDigest != rootfsDigest {
				t.Fatalf("checkpoint manifest = %+v", request.Manifest)
			}
			_ = json.NewEncoder(w).Encode(workerapi.CheckpointResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/v0/run/checkpoints/failed":
			var request workerapi.CheckpointFailedRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RequestVersion != 43 || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" || request.Error != "snapshot failed" {
				t.Fatalf("checkpoint failed request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.CheckpointResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateRunWait(context.Background(), workerapi.CreateRunWaitRequest{
		Lease:         claim.Fence(),
		CorrelationID: "corr-1",
		Kind:          workerapi.RunWaitKindToken,
		Params:        json.RawMessage(`{"prompt":"ship?"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.RunWaitID != "run-wait-id-1" {
		t.Fatalf("created = %+v", created)
	}
	polled, err := client.PollRunWait(context.Background(), workerapi.RunWaitPollRequest{Lease: claim.Fence(), RunWaitID: "run-wait-id-1"})
	if err != nil || polled.RequestVersion != 7 || polled.ResumeKind != "completed" {
		t.Fatalf("polled = %+v, err = %v", polled, err)
	}
	resumeAck, err := client.AcknowledgeRunWaitResume(context.Background(), workerapi.RunWaitResumeAckRequest{
		Lease: claim.Fence(), RunWaitID: "run-wait-id-1", ResumeRequestVersion: 7,
	})
	if err != nil || resumeAck.ResumeRequestVersion != 7 {
		t.Fatalf("resume ack = %+v, err = %v", resumeAck, err)
	}
	ready, err := client.MarkCheckpointReady(context.Background(), workerapi.CheckpointReadyRequest{
		Lease:          claim.Fence(),
		RequestVersion: 42,
		RunWaitID:      "run-wait-id-1",
		CheckpointID:   "checkpoint-1",
		SourceCleanup: &workerapi.RuntimeCleanupProof{
			Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC(),
		},
		Manifest: testClientCheckpointManifest(kernelDigest, rootfsDigest, configDigest, manifestDigest, vmStateDigest, scratchDigest, memoryDigest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready = %+v", ready)
	}
	failed, err := client.MarkCheckpointFailed(context.Background(), workerapi.CheckpointFailedRequest{
		Lease:          claim.Fence(),
		RequestVersion: 43,
		RunWaitID:      "run-wait-id-1",
		CheckpointID:   "checkpoint-1",
		Error:          "snapshot failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed = %+v", failed)
	}
	if got := strings.Join(paths, ","); got != "/api/worker/v0/instance/token,/api/worker/v0/run/waits/create,/api/worker/v0/run/waits/poll,/api/worker/v0/run/waits/resume-ack,/api/worker/v0/run/checkpoints/ready,/api/worker/v0/run/checkpoints/failed" {
		t.Fatalf("paths = %s", got)
	}
}

func TestAcknowledgeRunResumeRelease(t *testing.T) {
	lease := workerapi.RunLeaseAssignment{ID: "lease-1", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", LeaseSequence: 3}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/v0/instance/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
				Token: "worker-token", ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/api/worker/v0/run/leases/resume-release":
			if got := r.Header.Get("authorization"); got != "Bearer worker-token" {
				t.Fatalf("worker auth = %q", got)
			}
			var request workerapi.RunResumeReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease != lease.Fence() ||
				request.RunWaitID != "wait-1" ||
				request.CheckpointID != "checkpoint-1" ||
				request.ResumeAttachID != "attach-1" ||
				request.ResumeRequestVersion != 7 {
				t.Fatalf("resume release request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.RunResumeReleaseResponse{
				Lease:                lease.Fence(),
				RunWaitID:            request.RunWaitID,
				CheckpointID:         request.CheckpointID,
				ResumeAttachID:       request.ResumeAttachID,
				ResumeRequestVersion: request.ResumeRequestVersion,
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(
		server.URL,
		WithHTTPClient(server.Client()),
		WithAuth("worker-1", "worker-secret"),
		WithService("service-1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.AcknowledgeRunResumeRelease(context.Background(), workerapi.RunResumeReleaseRequest{
		Lease:                lease.Fence(),
		RunWaitID:            "wait-1",
		CheckpointID:         "checkpoint-1",
		ResumeAttachID:       "attach-1",
		ResumeRequestVersion: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Lease.ID != lease.ID ||
		response.RunWaitID != "wait-1" ||
		response.CheckpointID != "checkpoint-1" ||
		response.ResumeAttachID != "attach-1" ||
		response.ResumeRequestVersion != 7 {
		t.Fatalf("resume release response = %+v", response)
	}
}

func testClientCheckpointManifest(kernelDigest string, rootfsDigest string, configDigest string, manifestDigest string, vmStateDigest string, scratchDigest string, memoryDigest string) workerapi.CheckpointManifest {
	return workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{Runtime: workerapi.CheckpointRuntime{
			Backend:         "firecracker",
			ID:              "sha256:runtime",
			Arch:            "arm64",
			Contract:        capacityapi.RuntimeContract,
			KernelDigest:    kernelDigest,
			InitramfsDigest: "sha256:initramfs",
			RootfsDigest:    rootfsDigest,
			ConfigDigest:    configDigest,
		}},
		RuntimeState: workerapi.CheckpointRuntimeState{
			ConfigArtifact:      workerapi.CheckpointArtifact{Digest: manifestDigest, MediaType: cas.CheckpointRuntimeConfigMediaType},
			VMStateArtifact:     workerapi.CheckpointArtifact{Digest: vmStateDigest, MediaType: cas.CheckpointVMStateMediaType},
			ScratchDiskArtifact: workerapi.CheckpointArtifact{Digest: scratchDigest, MediaType: cas.CheckpointScratchDiskMediaType},
			MemoryArtifacts:     []workerapi.CheckpointArtifact{{Digest: memoryDigest, MediaType: cas.CheckpointMemoryMediaType}},
			Config:              json.RawMessage(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		},
		WorkspaceState: workerapi.CheckpointWorkspaceState{
			Base: workerapi.CheckpointWorkspaceBase{ArtifactDigest: "sha256:workspace", MountPath: "/workspace"},
		},
	}
}

func workerClientCapabilities() workerapi.Capabilities {
	return workerapi.Capabilities{
		Runtime: capacityapi.RuntimeProfile{
			ID: "sha256:runtime", Arch: "arm64", Contract: capacityapi.RuntimeContract,
			KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs",
		},
		MaxVCPUs:                  2,
		MaxMemoryMiB:              2048,
		VMMilliCPU:                2000,
		VMMemoryMiB:               2048,
		GuestEphemeralDiskBytes:   32768 << 20,
		VMGuestEphemeralDiskBytes: 32768 << 20,
		ExecutionSlotsAvailable:   1,
	}
}
