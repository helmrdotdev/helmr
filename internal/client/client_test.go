package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
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
	_, err = client.StartSession(context.Background(), "deploy", api.SessionStartRequest{})
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

func TestClientSendsPinnedVersionHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(api.APIVersionHeader); got != api.CurrentAPIVersion {
			t.Fatalf("%s = %q", api.APIVersionHeader, got)
		}
		if got := r.Header.Get(api.ClientVersionHeader); got != "0.2.3-test" {
			t.Fatalf("%s = %q", api.ClientVersionHeader, got)
		}
		if got := r.Header.Get(api.CLIVersionHeader); got != "0.2.3-test" {
			t.Fatalf("%s = %q", api.CLIVersionHeader, got)
		}
		if got := r.Header.Get(api.SDKVersionHeader); got != "" {
			t.Fatalf("%s = %q", api.SDKVersionHeader, got)
		}
		_ = json.NewEncoder(w).Encode(api.SessionStartResponse{Run: api.RunResponse{
			ID:     "run-1",
			TaskID: "deploy",
			Status: "queued",
		}})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithClientIdentity("cli", "0.2.3-test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartSession(context.Background(), "deploy", api.SessionStartRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestClientSendsSDKVersionHeaderForSDKIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(api.ClientVersionHeader); got != "1.2.3-sdk" {
			t.Fatalf("%s = %q", api.ClientVersionHeader, got)
		}
		if got := r.Header.Get(api.SDKVersionHeader); got != "1.2.3-sdk" {
			t.Fatalf("%s = %q", api.SDKVersionHeader, got)
		}
		if got := r.Header.Get(api.CLIVersionHeader); got != "" {
			t.Fatalf("%s = %q", api.CLIVersionHeader, got)
		}
		_ = json.NewEncoder(w).Encode(api.SessionStartResponse{Run: api.RunResponse{
			ID:     "run-1",
			TaskID: "deploy",
			Status: "queued",
		}})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithClientIdentity("sdk", "1.2.3-sdk"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartSession(context.Background(), "deploy", api.SessionStartRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsPlainHTTPNonLoopbackRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://helmr.example/api/sessions", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StartSession(context.Background(), "deploy", api.SessionStartRequest{})
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartSession(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions" {
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
		if _, ok := raw["source"]; ok {
			t.Fatalf("request JSON included source: %s", body)
		}
		var request api.SessionStartRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SessionStartResponse{Run: api.RunResponse{
			ID:        "run-1",
			TaskID:    "deploy",
			Status:    "queued",
			CreatedAt: now,
			UpdatedAt: now,
		}})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	started, err := client.StartSession(context.Background(), "deploy", api.SessionStartRequest{
		Payload: json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.ID != "run-1" || started.Run.Status != "queued" {
		t.Fatalf("started = %+v", started)
	}
}

func TestStartSessionReturnsAcceptedHTTPError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"error":"accepted elsewhere"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StartSession(context.Background(), "deploy", api.SessionStartRequest{})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusAccepted || !strings.Contains(httpErr.Message, "accepted elsewhere") {
		t.Fatalf("err = %#v, want 202 HTTPError", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestStartSessionUsesSessionScopedRoute(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/project-1/environments/env-1/sessions" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		var request api.SessionStartRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ProjectID != "" || request.EnvironmentID != "" {
			t.Fatalf("scoped route leaked body scope: %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.SessionStartResponse{Run: api.RunResponse{
			ID:        "run-1",
			TaskID:    "deploy",
			Status:    "queued",
			CreatedAt: now,
			UpdatedAt: now,
		}})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithSessionScopedRoutes())
	if err != nil {
		t.Fatal(err)
	}
	started, err := client.StartSession(context.Background(), "deploy", api.SessionStartRequest{
		ProjectID:     "project-1",
		EnvironmentID: "env-1",
		Payload:       json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.ID != "run-1" || started.Run.Status != "queued" {
		t.Fatalf("started = %+v", started)
	}
}

func TestRunOperations(t *testing.T) {
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/runs/run-1/cancel":
			var request api.CancelRunRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Reason != "cleanup" || !request.Force || request.IdempotencyKey != "cancel-1" {
				t.Fatalf("cancel request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.CancelRunResponse{
				Run:       api.RunResponse{ID: "run-1", Status: "cancelled"},
				Operation: api.RunOperationResponse{ID: "op-1", RunID: "run-1", Kind: "cancel", Status: "applied"},
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
	cancelled, err := client.CancelRun(context.Background(), "run-1", api.CancelRunRequest{
		Reason:         "cleanup",
		Force:          true,
		IdempotencyKey: "cancel-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Run.Status != "cancelled" || cancelled.Operation.Kind != "cancel" {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	if got := strings.Join(paths, ","); got != "POST /api/runs/run-1/cancel" {
		t.Fatalf("paths = %s", got)
	}
}

func TestCreateDeploymentSendsContentHash(t *testing.T) {
	source := []byte("deployment archive")
	sourcePath := t.TempDir() + "/deployment-source.tar"
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	var metadata api.CreateDeploymentRequest
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			switch part.FormName() {
			case "metadata":
				if err := json.NewDecoder(part).Decode(&metadata); err != nil {
					t.Fatal(err)
				}
			case "deployment_source":
				uploaded, err = io.ReadAll(part)
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unexpected field %q", part.FormName())
			}
			_ = part.Close()
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateDeployment(context.Background(), api.CreateDeploymentRequest{}, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "deployment-1" {
		t.Fatalf("response = %+v", response)
	}
	if metadata.ProjectID != "" || metadata.EnvironmentID != "" || metadata.ContentHash != sha256sum.DigestBytes(source) {
		t.Fatalf("metadata = %+v", metadata)
	}
	if !bytes.Equal(uploaded, source) {
		t.Fatalf("uploaded = %q", uploaded)
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
				AccessToken: "helmr_session_test",
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
	if token.AccessToken != "helmr_session_test" || token.TokenType != "bearer" {
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
			if r.URL.Query().Get("status") != "all" || r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("session_id") != "session-1" {
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
	runs, err := client.ListRuns(context.Background(), ListRunsOptions{Status: "all", Limit: 25, SessionID: "session-1"})
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
	if got := strings.Join(paths, ","); got != "/api/runs?limit=25&session_id=session-1&status=all,/api/runs/run-1/logs" {
		t.Fatalf("paths = %s", got)
	}
}

func TestSessionScopedClientRequiresEnvironmentScope(t *testing.T) {
	client, err := New("https://helmr.example.test", WithSessionScopedRoutes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRuns(context.Background()); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("ListRuns err = %v", err)
	}
	if _, err := client.GetRun(context.Background(), "run-1"); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("GetRun err = %v", err)
	}
	if _, err := client.ListSecrets(context.Background()); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("ListSecrets err = %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(sourcePath, []byte("deployment source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateDeployment(context.Background(), api.CreateDeploymentRequest{}, sourcePath); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("CreateDeployment err = %v", err)
	}
}

func TestFollowRunLogsSendsCursorAndDecodesChunks(t *testing.T) {
	var gotLastEventID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/logs" || r.URL.Query().Get("follow") != "1" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		gotLastEventID = r.Header.Get("Last-Event-ID")
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "id: tc1.eyJzIjo5fQ\n")
		_, _ = io.WriteString(w, "event: run_log\n")
		_, _ = io.WriteString(w, "data: ")
		_ = json.NewEncoder(w).Encode(api.RunLogChunk{
			ID:            "tc1.eyJzIjo5fQ",
			RunID:         "run-1",
			AttemptNumber: 1,
			Stream:        "stdout",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
			Bytes:         6,
			ObservedSeq:   2,
			At:            time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
		})
		_, _ = io.WriteString(w, "\n")
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	var chunks []api.RunLogChunk
	if err := client.FollowRunLogs(context.Background(), "run-1", "tc1.eyJzIjo4fQ", func(chunk api.RunLogChunk) error {
		chunks = append(chunks, chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if gotLastEventID != "tc1.eyJzIjo4fQ" {
		t.Fatalf("last event id = %q", gotLastEventID)
	}
	if len(chunks) != 1 || chunks[0].ID != "tc1.eyJzIjo5fQ" || chunks[0].ContentBase64 != base64.StdEncoding.EncodeToString([]byte("hello\n")) {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestWorkerLifecycleClient(t *testing.T) {
	claim := api.WorkerRunLease{
		ID: "00000000-0000-0000-0000-000000000001", RunID: "00000000-0000-0000-0000-000000000002",
		WorkerGroupID: "run-us-east-1", WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
		WorkerEpoch: 1, LeaseSequence: 1, RuntimeInstanceID: "00000000-0000-0000-0000-000000000501",
		NetworkSlotID: "00000000-0000-0000-0000-000000000601", NetworkSlotGeneration: 1,
		AttemptNumber: 1, ProtocolVersion: api.CurrentWorkerProtocolVersion,
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
			if request.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || request.WorkerInstanceSecret != "worker-secret" || request.ServiceID != "00000000-0000-0000-0000-000000000901" || request.ProtocolVersion != api.CurrentWorkerProtocolVersion || !request.SupportsRun || request.SupportsBuild {
				t.Fatalf("worker token request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerTokenResponse{
				Token:            workerToken,
				ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/api/worker/leases/lease":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(body)) != "{}" {
				t.Fatalf("run claim body = %s, want empty authority request", body)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerRunLeaseResponse{
				Lease: &claim,
				Run: &api.WorkerRun{
					ID:                    claim.RunID,
					TaskID:                "deploy",
					Payload:               json.RawMessage(`{}`),
					Secrets:               api.ResolvedSecrets{},
					DeploymentSource:      api.DeploymentSourceArtifact{Digest: "sha256:" + strings.Repeat("a", 64)},
					WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
					Requirements:          workerClientRequirements(),
					MaxDurationSeconds:    3600,
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
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: api.WorkerStatusActive})
		case "/api/worker/drain":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: api.WorkerStatusDraining, ActiveExecutions: 1})
		case "/api/worker/drain/complete":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerDrainCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() || len(request.Inventory) != 0 {
				t.Fatalf("worker drain completion = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: api.WorkerStatusDisabled})
		case "/api/worker/status":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: api.WorkerStatusDraining, ActiveExecutions: 1})
		case "/api/worker/fence":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerFenceRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ReasonCode != "termination_drain_failed" {
				t.Fatalf("fence reason = %q", request.ReasonCode)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/worker/leases/start":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStartResponse{RunID: claim.RunID, Status: "running"})
		case "/api/worker/leases/renew":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerRenewResponse{Lease: claim})
		case "/api/worker/leases/logs":
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
			if request.Lease.ID != claim.ID || request.Stream != api.WorkerLogStreamStdout || request.ObservedSeq != 7 || string(content) != "hello\n" {
				t.Fatalf("log request = %+v content=%q", request, content)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEventResponse{RunID: claim.RunID})
		case "/api/worker/leases/log-entries":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request api.WorkerRecordLogEntryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.Entry != "building" {
				t.Fatalf("log entry request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEventResponse{RunID: claim.RunID})
		case "/api/worker/leases/release":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerReleaseResponse{RunID: claim.RunID, Status: "succeeded"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithWorkerService("00000000-0000-0000-0000-000000000901", api.CurrentWorkerProtocolVersion, true, false))
	if err != nil {
		t.Fatal(err)
	}
	leased, err := client.LeaseRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if leased.Lease == nil || leased.Run == nil {
		t.Fatalf("leased = %+v", leased)
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
	if status, err := client.CompleteWorkerDrain(context.Background(), api.WorkerDrainCompletionRequest{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        time.Now().UTC(),
		Inventory:         []string{},
	}); err != nil || status.Status != api.WorkerStatusDisabled {
		t.Fatalf("complete worker drain status = %+v, err = %v", status, err)
	}
	if _, err := client.StartRun(context.Background(), *leased.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RenewRun(context.Background(), *leased.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppendLog(context.Background(), *leased.Lease, api.WorkerLogStreamStdout, 7, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RecordLogEntry(context.Background(), *leased.Lease, "building"); err != nil {
		t.Fatal(err)
	}
	exitCode := int32(0)
	if _, err := client.ReleaseRun(context.Background(), *leased.Lease, api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode}); err != nil {
		t.Fatal(err)
	}
	if err := client.FenceWorker(context.Background(), "termination_drain_failed"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got != "/api/worker/auth/token,/api/worker/leases/lease,/api/worker/activate,/api/worker/drain,/api/worker/status,/api/worker/drain/complete,/api/worker/leases/start,/api/worker/leases/renew,/api/worker/leases/logs,/api/worker/leases/log-entries,/api/worker/leases/release,/api/worker/fence" {
		t.Fatalf("paths = %s", got)
	}
}

func TestCompleteWorkerDrainRetriesTheIdenticalProofAfterAmbiguousResponse(t *testing.T) {
	attempts := 0
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/auth/token":
			_ = json.NewEncoder(w).Encode(api.WorkerTokenResponse{Token: "worker-token", ExpiresInSeconds: 3600})
		case "/api/worker/drain/complete":
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
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{Status: api.WorkerStatusDisabled})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("worker", "secret"), WithWorkerService("service", api.CurrentWorkerProtocolVersion, true, false))
	if err != nil {
		t.Fatal(err)
	}
	request := api.WorkerDrainCompletionRequest{
		InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0",
		ObservedAt: time.Now().UTC(), Inventory: []string{},
	}
	status, err := client.CompleteWorkerDrain(context.Background(), request)
	if err != nil || status.Status != api.WorkerStatusDisabled {
		t.Fatalf("status = %+v, err = %v", status, err)
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
		case "/api/worker/auth/token":
			tokenRequests++
			_ = json.NewEncoder(w).Encode(api.WorkerTokenResponse{
				Token: fmt.Sprintf("worker-token-%d", tokenRequests), ExpiresInSeconds: 3600,
			})
		case "/api/worker/activate":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			activateBodies = append(activateBodies, body)
			if r.Header.Get("authorization") == "Bearer worker-token-1" {
				http.Error(w, `{"error":"stale token"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{Status: api.WorkerStatusActive})
		case "/api/worker/status":
			statusRequests++
			if statusRequests == 1 {
				http.Error(w, `{"error":"stale group claims"}`, http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("authorization"); got != "Bearer worker-token-3" {
				t.Fatalf("refreshed status authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerStatusResponse{Status: api.WorkerStatusActive})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()),
		WithWorkerAuth("00000000-0000-0000-0000-000000000401", "worker-secret"),
		WithWorkerService("00000000-0000-0000-0000-000000000901", api.CurrentWorkerProtocolVersion, true, false))
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
	claim := api.WorkerRunLease{
		ID: "00000000-0000-0000-0000-000000000001", RunID: "00000000-0000-0000-0000-000000000002",
		WorkerGroupID: "run-us-east-1", WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
		WorkerEpoch: 1, LeaseSequence: 1, RuntimeInstanceID: "00000000-0000-0000-0000-000000000501",
		NetworkSlotID: "00000000-0000-0000-0000-000000000601", NetworkSlotGeneration: 1,
		AttemptNumber: 1, ProtocolVersion: api.CurrentWorkerProtocolVersion,
		ExpiresAt: time.Date(2026, 5, 8, 12, 5, 0, 0, time.UTC),
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
		if r.URL.Path == "/api/worker/auth/token" {
			_ = json.NewEncoder(w).Encode(api.WorkerTokenResponse{Token: "worker-token", ExpiresInSeconds: int64(time.Hour / time.Second)})
			return
		}
		if got := r.Header.Get("authorization"); got != "Bearer worker-token" {
			t.Fatalf("worker auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/worker/leases/run-waits":
			var request api.WorkerCreateRunWaitRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.CorrelationID != "corr-1" || request.Kind != api.WorkerRunWaitKindToken || string(request.Params) != `{"prompt":"ship?"}` {
				t.Fatalf("create run wait = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerCreateRunWaitResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1"})
		case "/api/worker/leases/run-waits/poll":
			var request api.WorkerRunWaitPollRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RunWaitID != "run-wait-id-1" {
				t.Fatalf("poll run wait request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerRunWaitPollResponse{
				RunID: claim.RunID, RunWaitID: request.RunWaitID, Status: "resume_requested",
				RequestVersion: 7, ResumeKind: "completed", ResumePayload: json.RawMessage(`{"approved":true}`),
			})
		case "/api/worker/leases/run-waits/resume-ack":
			var request api.WorkerRunWaitResumeAckRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RunWaitID != "run-wait-id-1" || request.ResumeRequestVersion != 7 {
				t.Fatalf("resume ack request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerRunWaitResumeAckResponse{
				RunID: claim.RunID, RunWaitID: request.RunWaitID, ResumeRequestVersion: request.ResumeRequestVersion,
			})
		case "/api/worker/leases/checkpoints/ready":
			var request api.WorkerCheckpointReadyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RequestVersion != 42 || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" || request.ActiveDurationMs != 123 {
				t.Fatalf("checkpoint ready request = %+v", request)
			}
			if request.Manifest.RecoveryPoint.Runtime.KernelDigest != kernelDigest || request.Manifest.RecoveryPoint.Runtime.RootfsDigest != rootfsDigest {
				t.Fatalf("checkpoint manifest = %+v", request.Manifest)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerCheckpointResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/leases/restores/ack":
			var request api.WorkerAcknowledgeRestoreRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" {
				t.Fatalf("restore attach request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerAcknowledgeRestoreResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/leases/checkpoints/failed":
			var request api.WorkerCheckpointFailedRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RequestVersion != 43 || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" || request.Error != "snapshot failed" {
				t.Fatalf("checkpoint failed request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerCheckpointResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithWorkerService("00000000-0000-0000-0000-000000000901", api.CurrentWorkerProtocolVersion, true, false))
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateRunWait(context.Background(), api.WorkerCreateRunWaitRequest{
		Lease:         claim,
		CorrelationID: "corr-1",
		Kind:          api.WorkerRunWaitKindToken,
		Params:        json.RawMessage(`{"prompt":"ship?"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.RunWaitID != "run-wait-id-1" {
		t.Fatalf("created = %+v", created)
	}
	polled, err := client.PollRunWait(context.Background(), api.WorkerRunWaitPollRequest{Lease: claim, RunWaitID: "run-wait-id-1"})
	if err != nil || polled.RequestVersion != 7 || polled.ResumeKind != "completed" {
		t.Fatalf("polled = %+v, err = %v", polled, err)
	}
	resumeAck, err := client.AcknowledgeRunWaitResume(context.Background(), api.WorkerRunWaitResumeAckRequest{
		Lease: claim, RunWaitID: "run-wait-id-1", ResumeRequestVersion: 7,
	})
	if err != nil || resumeAck.ResumeRequestVersion != 7 {
		t.Fatalf("resume ack = %+v, err = %v", resumeAck, err)
	}
	ready, err := client.MarkCheckpointReady(context.Background(), api.WorkerCheckpointReadyRequest{
		Lease:            claim,
		RequestVersion:   42,
		RunWaitID:        "run-wait-id-1",
		CheckpointID:     "checkpoint-1",
		ActiveDurationMs: 123,
		Manifest:         testClientCheckpointManifest(kernelDigest, rootfsDigest, configDigest, manifestDigest, vmStateDigest, scratchDigest, memoryDigest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready = %+v", ready)
	}
	acknowledged, err := client.AcknowledgeRestore(context.Background(), api.WorkerAcknowledgeRestoreRequest{
		Lease:        claim,
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledged = %+v", acknowledged)
	}
	failed, err := client.MarkCheckpointFailed(context.Background(), api.WorkerCheckpointFailedRequest{
		Lease:          claim,
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
	if got := strings.Join(paths, ","); got != "/api/worker/auth/token,/api/worker/leases/run-waits,/api/worker/leases/run-waits/poll,/api/worker/leases/run-waits/resume-ack,/api/worker/leases/checkpoints/ready,/api/worker/leases/restores/ack,/api/worker/leases/checkpoints/failed" {
		t.Fatalf("paths = %s", got)
	}
}

func testClientCheckpointManifest(kernelDigest string, rootfsDigest string, configDigest string, manifestDigest string, vmStateDigest string, scratchDigest string, memoryDigest string) api.WorkerCheckpointManifest {
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{Runtime: api.WorkerCheckpointRuntime{
			Backend:         "firecracker",
			ID:              "sha256:runtime",
			Arch:            "arm64",
			ABI:             "helmr.firecracker.snapshot.v0",
			KernelDigest:    kernelDigest,
			InitramfsDigest: "sha256:initramfs",
			RootfsDigest:    rootfsDigest,
			ConfigDigest:    configDigest,
		}},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      api.WorkerCheckpointArtifact{Digest: manifestDigest, MediaType: cas.CheckpointRuntimeConfigMediaType},
			VMStateArtifact:     api.WorkerCheckpointArtifact{Digest: vmStateDigest, MediaType: cas.CheckpointVMStateMediaType},
			ScratchDiskArtifact: api.WorkerCheckpointArtifact{Digest: scratchDigest, MediaType: cas.CheckpointScratchDiskMediaType},
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{{Digest: memoryDigest, MediaType: cas.CheckpointMemoryMediaType}},
			Config:              json.RawMessage(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{
			Base: api.WorkerCheckpointWorkspaceBase{ArtifactDigest: "sha256:workspace", MountPath: "/workspace"},
		},
	}
}

func workerClientCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		ProtocolVersion:         api.CurrentWorkerProtocolVersion,
		RuntimeID:               "sha256:runtime",
		RuntimeArch:             "arm64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "helmr/v0",
		MaxVCPUs:                2,
		MaxMemoryMiB:            2048,
		VMMilliCPU:              2000,
		VMMemoryMiB:             2048,
		MaxDiskMiB:              20480,
		VMMaxDiskMiB:            20480,
		ScratchBytes:            20480 << 20,
		VMMaxScratchBytes:       20480 << 20,
		ExecutionSlotsAvailable: 1,
		Network: api.WorkerNetworkCapabilities{
			Internet:      true,
			BlockInternet: true,
			DenyCIDRs:     true,
		},
	}
}

func workerClientRequirements() compute.RunRuntimeRequirements {
	return compute.RunRuntimeRequirements{
		Resources: compute.ResourceVector{
			MilliCPU:  1000,
			MemoryMiB: 512,
			DiskMiB:   1024,
			Slots:     1,
		},
		Runtime: compute.RuntimeSelector{
			ID:              "sha256:runtime",
			Arch:            "arm64",
			ABI:             "helmr.firecracker.snapshot.v0",
			KernelDigest:    "sha256:kernel",
			InitramfsDigest: "sha256:initramfs",
			RootfsDigest:    "sha256:rootfs",
			CNIProfile:      "helmr/v0",
		},
		Network: compute.DefaultNetworkPolicy(),
	}
}
