package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestClientErrorUsesServerMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad source"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateRun(context.Background(), api.CreateRunRequest{TaskID: "deploy"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad source") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRejectsBaseURLQueryAndFragment(t *testing.T) {
	for _, raw := range []string{"https://helmr.example?x=1", "https://helmr.example/#fragment"} {
		if _, err := New(raw); err == nil || !strings.Contains(err.Error(), "must not include query or fragment") {
			t.Fatalf("New(%q) err = %v", raw, err)
		}
	}
}

func TestNewRejectsPlainHTTPNonLoopback(t *testing.T) {
	_, err := New("http://helmr.example")
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewAllowsPlainHTTPLoopback(t *testing.T) {
	for _, raw := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := New(raw); err != nil {
			t.Fatalf("New(%q) err = %v", raw, err)
		}
	}
}

func TestClientRejectsPlainHTTPNonLoopbackRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://helmr.example/api/runs", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateRun(context.Background(), api.CreateRunRequest{TaskID: "deploy"})
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateRun(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["workspace"]; !ok {
			t.Fatalf("request JSON missing workspace: %s", body)
		}
		if _, ok := raw["source"]; ok {
			t.Fatalf("request JSON included source: %s", body)
		}
		var request api.CreateRunRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if request.TaskID != "deploy" || request.Workspace.Ref != "0123456789abcdef0123456789abcdef01234567" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.RunResponse{
			ID:        "run-1",
			TaskID:    request.TaskID,
			Status:    "queued",
			CreatedAt: now,
			UpdatedAt: now,
		})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := client.CreateRun(context.Background(), api.CreateRunRequest{
		TaskID:    "deploy",
		Payload:   json.RawMessage(`{"env":"prod"}`),
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "0123456789abcdef0123456789abcdef01234567"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "run-1" || run.Status != "queued" {
		t.Fatalf("run = %+v", run)
	}
}

func TestDeviceCodeFlowClient(t *testing.T) {
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("authorization"); got != "" {
			t.Fatalf("auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/auth/device/start":
			_ = json.NewEncoder(w).Encode(api.DeviceStartResponse{
				DeviceCode:              "device-token",
				UserCode:                "ABCD-EFGH",
				VerificationURI:         "https://helmr.example.test/auth/device",
				VerificationURIComplete: "https://helmr.example.test/auth/device?code=ABCD-EFGH",
				ExpiresInSeconds:        600,
				IntervalSeconds:         5,
			})
		case "/api/auth/device/token":
			var request api.DeviceTokenRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.DeviceCode != "device-token" {
				t.Fatalf("request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.DeviceTokenResponse{
				AccessToken: "lms_test",
				TokenType:   "bearer",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	start, err := client.StartDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if start.UserCode != "ABCD-EFGH" || start.IntervalSeconds != 5 {
		t.Fatalf("start = %+v", start)
	}
	token, err := client.ExchangeDeviceCode(context.Background(), start.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "lms_test" || token.TokenType != "bearer" {
		t.Fatalf("token = %+v", token)
	}
	if got := strings.Join(paths, ","); got != "/api/auth/device/start,/api/auth/device/token" {
		t.Fatalf("paths = %s", got)
	}
}

func TestListRunsOptionsAndGetRunLogs(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/runs":
			if r.URL.Query().Get("status") != "all" || r.URL.Query().Get("limit") != "25" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.ListRunsResponse{Runs: []api.RunResponse{{
				ID:        "run-1",
				TaskID:    "deploy",
				Status:    "succeeded",
				CreatedAt: now,
				UpdatedAt: now,
			}}})
		case "/api/runs/run-1/logs":
			_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
				StdoutBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
				StderrBase64: base64.StdEncoding.EncodeToString([]byte("warn\n")),
				Cursor:       "0",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := client.ListRuns(context.Background(), ListRunsOptions{Status: "all", Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].ID != "run-1" {
		t.Fatalf("runs = %+v", runs)
	}
	logs, err := client.GetRunLogs(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if logs.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte("hello\n")) || logs.StderrBase64 != base64.StdEncoding.EncodeToString([]byte("warn\n")) {
		t.Fatalf("logs = %+v", logs)
	}
	if got := strings.Join(paths, ","); got != "/api/runs?limit=25&status=all,/api/runs/run-1/logs" {
		t.Fatalf("paths = %s", got)
	}
}

func TestWorkerLifecycleClient(t *testing.T) {
	claim := api.WorkerClaim{
		ID:        "00000000-0000-0000-0000-000000000001",
		RunID:     "00000000-0000-0000-0000-000000000002",
		WorkerID:  "worker-1",
		ExpiresAt: time.Date(2026, 5, 8, 12, 5, 0, 0, time.UTC),
	}
	paths := []string{}
	workerToken := "worker-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/worker/auth/token":
			if got := r.Header.Get("authorization"); got != "" {
				t.Fatalf("worker token request auth = %s", got)
			}
			var request api.WorkerTokenRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerID != "worker-1" || request.WorkerSecret != "worker-secret" {
				t.Fatalf("worker token request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerTokenResponse{
				Token:            workerToken,
				ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/api/worker/executions/claim":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerClaimRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Capabilities.RuntimeArch != "arm64" {
				t.Fatalf("claim capabilities = %+v", request.Capabilities)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerClaimResponse{
				Claim: &claim,
				Run: &api.WorkerRun{
					ID:                 claim.RunID,
					TaskID:             "deploy",
					Payload:            json.RawMessage(`{}`),
					Secrets:            api.ResolvedSecrets{},
					TaskSource:         api.TaskSourceArtifact{Digest: "sha256:" + strings.Repeat("a", 64)},
					Workspace:          api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "0123456789abcdef0123456789abcdef01234567"},
					MaxDurationSeconds: 3600,
				},
			})
		case "/api/worker/activate":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerActivateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Capabilities.RuntimeArch != "arm64" {
				t.Fatalf("activate capabilities = %+v", request.Capabilities)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerID: "worker-1", Status: api.WorkerStatusActive})
		case "/api/worker/drain":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerID: "worker-1", Status: api.WorkerStatusDraining, ActiveExecutions: 1})
		case "/api/worker/status":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerID: "worker-1", Status: api.WorkerStatusDraining, ActiveExecutions: 1})
		case "/api/worker/executions/start":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStartResponse{RunID: claim.RunID, Status: "running"})
		case "/api/worker/executions/renew":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerRenewResponse{Claim: claim})
		case "/api/worker/executions/logs":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerAppendLogRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			content, err := base64.StdEncoding.DecodeString(request.ContentBase64)
			if err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.Stream != api.WorkerLogStreamStdout || request.ObservedSeq != 7 || string(content) != "hello\n" {
				t.Fatalf("log request = %+v content=%q", request, content)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEventResponse{RunID: claim.RunID})
		case "/api/worker/executions/log-entries":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerRecordLogEntryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.Entry != "building" {
				t.Fatalf("log entry request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEventResponse{RunID: claim.RunID})
		case "/api/worker/executions/events":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerEmitEventRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.EventType != "deploy.progress" || string(request.Content) != `{"step":1}` {
				t.Fatalf("event request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEventResponse{RunID: claim.RunID})
		case "/api/worker/executions/release":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerReleaseResponse{RunID: claim.RunID, Status: "succeeded"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("worker-1", "worker-secret"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := client.ClaimRun(context.Background(), api.WorkerCapabilities{
		RuntimeArch:    "arm64",
		RuntimeABI:     "helmr.firecracker.snapshot.v0",
		KernelDigest:   "sha256:kernel",
		RootfsDigest:   "sha256:rootfs",
		CNIProfile:     "helmr/v1",
		MaxVCPUs:       2,
		MaxMemoryMiB:   2048,
		SlotsAvailable: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Claim == nil || claimed.Run == nil {
		t.Fatalf("claimed = %+v", claimed)
	}
	if status, err := client.ActivateWorker(context.Background(), workerClientCapabilities()); err != nil || status.Status != api.WorkerStatusActive {
		t.Fatalf("activate status = %+v err=%v", status, err)
	}
	if status, err := client.DrainWorker(context.Background()); err != nil || status.Status != api.WorkerStatusDraining || status.ActiveExecutions != 1 {
		t.Fatalf("drain status = %+v err=%v", status, err)
	}
	if status, err := client.GetWorkerStatus(context.Background()); err != nil || status.Status != api.WorkerStatusDraining || status.ActiveExecutions != 1 {
		t.Fatalf("worker status = %+v err=%v", status, err)
	}
	if _, err := client.StartRun(context.Background(), *claimed.Claim); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RenewRun(context.Background(), *claimed.Claim); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppendLog(context.Background(), *claimed.Claim, api.WorkerLogStreamStdout, 7, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RecordLogEntry(context.Background(), *claimed.Claim, "building"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmitEvent(context.Background(), *claimed.Claim, "deploy.progress", json.RawMessage(`{"step":1}`)); err != nil {
		t.Fatal(err)
	}
	exitCode := int32(0)
	if _, err := client.ReleaseRun(context.Background(), *claimed.Claim, api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got != "/api/worker/auth/token,/api/worker/executions/claim,/api/worker/activate,/api/worker/drain,/api/worker/status,/api/worker/executions/start,/api/worker/executions/renew,/api/worker/executions/logs,/api/worker/executions/log-entries,/api/worker/executions/events,/api/worker/executions/release" {
		t.Fatalf("paths = %s", got)
	}
}

func TestWorkerRegistrationControlClient(t *testing.T) {
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/worker/register":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s", r.Method)
			}
			var request api.WorkerRegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ResourceName != "host-1" || request.RegistrationToken != "lms_worker_register_test" {
				t.Fatalf("register request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerRegisterResponse{
				WorkerID:     "worker-generated-1",
				WorkerSecret: "worker-secret",
			})
		case "/api/worker/credentials/worker/1":
			if r.Method != http.MethodDelete {
				t.Fatalf("method = %s", r.Method)
			}
			if got := r.Header.Get("authorization"); got != "Bearer control-token" {
				t.Fatalf("auth = %s", got)
			}
			if got := r.URL.EscapedPath(); got != "/api/worker/credentials/worker%2F1" {
				t.Fatalf("escaped path = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.RevokeWorkerCredentialsResponse{Revoked: 1})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	control, err := New(server.URL, WithHTTPClient(server.Client()), WithBearerToken("control-token"))
	if err != nil {
		t.Fatal(err)
	}
	registered, err := control.RegisterWorker(context.Background(), "lms_worker_register_test", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if registered.WorkerID != "worker-generated-1" || registered.WorkerSecret != "worker-secret" {
		t.Fatalf("registered = %+v", registered)
	}
	revoked, err := control.RevokeWorkerCredentials(context.Background(), "worker/1")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Revoked != 1 {
		t.Fatalf("revoked = %+v", revoked)
	}
	if got := strings.Join(paths, ","); got != "POST /api/worker/register,DELETE /api/worker/credentials/worker/1" {
		t.Fatalf("paths = %s", got)
	}
}

func TestWorkerWaitpointClient(t *testing.T) {
	claim := api.WorkerClaim{
		ID:        "00000000-0000-0000-0000-000000000001",
		RunID:     "00000000-0000-0000-0000-000000000002",
		WorkerID:  "worker-1",
		ExpiresAt: time.Date(2026, 5, 8, 12, 5, 0, 0, time.UTC),
	}
	kernelDigest := "sha256:kernel"
	rootfsDigest := "sha256:rootfs"
	configDigest := "sha256:runtime-config"
	vmStateDigest := "sha256:state"
	memoryDigest := "sha256:memory"
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/worker/auth/token" {
			_ = json.NewEncoder(w).Encode(api.WorkerTokenResponse{Token: "worker-token", ExpiresInSeconds: int64(time.Hour / time.Second)})
			return
		}
		if got := r.Header.Get("authorization"); got != "Bearer worker-token" {
			t.Fatalf("worker auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/worker/executions/waitpoints":
			var request api.WorkerCreateWaitpointRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.CorrelationID != "corr-1" || request.Kind != api.WorkerWaitpointKindApproval || string(request.Request) != `{"prompt":"ship?"}` {
				t.Fatalf("create waitpoint request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerCreateWaitpointResponse{RunID: claim.RunID, WaitpointID: "waitpoint-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/executions/waitpoints/decision":
			var request api.WorkerWaitpointDecisionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.WaitpointID != "waitpoint-1" {
				t.Fatalf("decision request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerWaitpointDecisionResponse{
				RunID:                 claim.RunID,
				WaitpointID:           "waitpoint-1",
				Resolved:              true,
				Kind:                  "approved",
				ResolutionPayloadJSON: json.RawMessage(`{"approved":true}`),
			})
		case "/api/worker/executions/checkpoints/ready":
			var request api.WorkerCheckpointReadyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.WaitpointID != "waitpoint-1" || request.CheckpointID != "checkpoint-1" || request.ActiveDurationMs != 123 {
				t.Fatalf("checkpoint ready request = %+v", request)
			}
			if request.Manifest.KernelDigest == nil || *request.Manifest.KernelDigest != kernelDigest || request.Manifest.RootfsDigest == nil || *request.Manifest.RootfsDigest != rootfsDigest {
				t.Fatalf("checkpoint manifest = %+v", request.Manifest)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerCreateWaitpointResponse{RunID: claim.RunID, WaitpointID: "waitpoint-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/executions/checkpoints/failed":
			var request api.WorkerCheckpointFailedRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Claim.ID != claim.ID || request.WaitpointID != "waitpoint-1" || request.CheckpointID != "checkpoint-1" || request.Error != "snapshot failed" {
				t.Fatalf("checkpoint failed request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerCreateWaitpointResponse{RunID: claim.RunID, WaitpointID: "waitpoint-1", CheckpointID: "checkpoint-1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("worker-1", "worker-secret"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateWaitpoint(context.Background(), api.WorkerCreateWaitpointRequest{
		Claim:         claim,
		CorrelationID: "corr-1",
		Kind:          api.WorkerWaitpointKindApproval,
		Request:       json.RawMessage(`{"prompt":"ship?"}`),
		DisplayText:   "ship?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.WaitpointID != "waitpoint-1" || created.CheckpointID != "checkpoint-1" {
		t.Fatalf("created = %+v", created)
	}
	decision, err := client.GetWaitpointDecision(context.Background(), api.WorkerWaitpointDecisionRequest{Claim: claim, WaitpointID: "waitpoint-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Resolved || string(decision.ResolutionPayloadJSON) != `{"approved":true}` {
		t.Fatalf("decision = %+v", decision)
	}
	ready, err := client.MarkCheckpointReady(context.Background(), api.WorkerCheckpointReadyRequest{
		Claim:            claim,
		WaitpointID:      "waitpoint-1",
		CheckpointID:     "checkpoint-1",
		ActiveDurationMs: 123,
		Manifest: api.WorkerCheckpointManifest{
			RuntimeBackend:      "firecracker",
			RuntimeArch:         "arm64",
			RuntimeABI:          "helmr.firecracker.snapshot.v0",
			KernelDigest:        &kernelDigest,
			RootfsDigest:        &rootfsDigest,
			RuntimeConfigDigest: &configDigest,
			VMStateDigest:       &vmStateDigest,
			MemoryDigests:       []string{memoryDigest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready = %+v", ready)
	}
	failed, err := client.MarkCheckpointFailed(context.Background(), api.WorkerCheckpointFailedRequest{
		Claim:        claim,
		WaitpointID:  "waitpoint-1",
		CheckpointID: "checkpoint-1",
		Error:        "snapshot failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed = %+v", failed)
	}
	if got := strings.Join(paths, ","); got != "/api/worker/auth/token,/api/worker/executions/waitpoints,/api/worker/executions/waitpoints/decision,/api/worker/executions/checkpoints/ready,/api/worker/executions/checkpoints/failed" {
		t.Fatalf("paths = %s", got)
	}
}

func workerClientCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		RuntimeArch:    "arm64",
		RuntimeABI:     "helmr.firecracker.snapshot.v0",
		KernelDigest:   "sha256:kernel",
		RootfsDigest:   "sha256:rootfs",
		CNIProfile:     "helmr/v1",
		MaxVCPUs:       2,
		MaxMemoryMiB:   2048,
		SlotsAvailable: 1,
	}
}
