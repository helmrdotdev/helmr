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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
	_, err = client.StartTask(
		context.Background(),
		"deploy",
		api.StartTaskRequest{},
		EnvironmentScopeOptions{},
	)
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

func TestClientSendsAPIVersionHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(api.APIVersionHeader); got != api.CurrentAPIVersion {
			t.Fatalf("%s = %q", api.APIVersionHeader, got)
		}
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartTask(context.Background(), "deploy", api.StartTaskRequest{}, EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsPlainHTTPNonLoopbackRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://helmr.example/api/tasks/deploy/start", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StartTask(context.Background(), "deploy", api.StartTaskRequest{}, EnvironmentScopeOptions{})
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks/deploy/start" {
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
		var request api.StartTaskRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if request.Workspace.ID == nil || *request.Workspace.ID != "ws_1" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := "ws_1"
	started, err := client.StartTask(context.Background(), "deploy", api.StartTaskRequest{
		Payload:   json.RawMessage(`{"env":"prod"}`),
		Workspace: api.WorkspaceTarget{ID: &workspaceID},
	}, EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if started.RunID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" {
		t.Fatalf("started = %+v", started)
	}
}

func TestStartTaskReturnsHTTPError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks/deploy/start" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already started differently"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StartTask(context.Background(), "deploy", api.StartTaskRequest{}, EnvironmentScopeOptions{})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict || !strings.Contains(httpErr.Message, "already started differently") {
		t.Fatalf("err = %#v, want 409 HTTPError", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestStartTaskUsesEnvironmentScopedRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/project-1/environments/env-1/tasks/deploy/start" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		var request api.StartTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithSessionScopedRoutes())
	if err != nil {
		t.Fatal(err)
	}
	started, err := client.StartTask(context.Background(), "deploy", api.StartTaskRequest{
		Payload: json.RawMessage(`{"env":"prod"}`),
	}, EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"})
	if err != nil {
		t.Fatal(err)
	}
	if started.RunID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" {
		t.Fatalf("started = %+v", started)
	}
}

func TestRunOperations(t *testing.T) {
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/cancel":
			if r.ContentLength > 0 {
				t.Fatalf("cancel request body length = %d", r.ContentLength)
			}
			_ = json.NewEncoder(w).Encode(api.RunSnapshotResponse{
				ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: "cancelled",
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
	cancelled, err := client.CancelRun(context.Background(), "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" || cancelled.Status != "cancelled" {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	if got := strings.Join(paths, ","); got != "POST /api/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/cancel" {
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
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateDeployment(
		context.Background(),
		api.CreateDeploymentRequest{IdempotencyKey: "deploy-1"},
		sourcePath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("response = %+v", response)
	}
	if metadata.ProjectID != "" || metadata.EnvironmentID != "" ||
		metadata.IdempotencyKey != "deploy-1" ||
		metadata.ContentHash != sha256sum.DigestBytes(source) {
		t.Fatalf("metadata = %+v", metadata)
	}
	if !bytes.Equal(uploaded, source) {
		t.Fatalf("uploaded = %q", uploaded)
	}
}

func TestCreateDeploymentRetriesLostResponseWithSameKey(t *testing.T) {
	source := []byte("deployment archive")
	sourcePath := filepath.Join(t.TempDir(), "deployment-source.tar")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		var metadata api.CreateDeploymentRequest
		if err := json.Unmarshal([]byte(r.FormValue("metadata")), &metadata); err != nil {
			t.Fatal(err)
		}
		mutex.Lock()
		keys = append(keys, metadata.IdempotencyKey)
		attempt := len(keys)
		mutex.Unlock()
		if attempt == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateDeployment(
		context.Background(),
		api.CreateDeploymentRequest{IdempotencyKey: "deploy-retry"},
		sourcePath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("response = %+v", response)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if !slices.Equal(keys, []string{"deploy-retry", "deploy-retry"}) {
		t.Fatalf("idempotency keys = %v", keys)
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

func TestListRunsOptionsAndListRunLogs(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/runs":
			if got := r.URL.Query()["status"]; !slices.Equal(got, []string{"running", "waiting"}) ||
				r.URL.Query().Get("cursor") != "cursor-1" ||
				r.URL.Query().Get("limit") != "25" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.ListRunSnapshotsResponse{
				Runs: []api.RunSnapshotResponse{{
					ID:         "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
					Status:     "succeeded",
					Entrypoint: api.RunEntrypointResponse{Kind: "task", ID: "deploy"},
					CreatedAt:  now,
				}},
				NextCursor: "cursor-2",
			})
		case "/api/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs":
			_ = json.NewEncoder(w).Encode(api.RunLogPage{
				Logs: []api.RunLogRecord{{
					ID: "rt1.log", Kind: "stdout", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
					AttemptNumber: 1,
					ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
				}},
				NextCursor: "rt1.next",
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
	runs, err := client.ListRuns(context.Background(), ListRunsOptions{
		Statuses: []string{"running", "waiting"},
		Cursor:   "cursor-1",
		Limit:    25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" ||
		runs.Runs[0].Entrypoint.ID != "deploy" || runs.NextCursor != "cursor-2" {
		t.Fatalf("runs = %+v", runs)
	}
	logs, err := client.ListRunLogs(context.Background(), "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs.Logs) != 1 ||
		logs.Logs[0].ContentBase64 != base64.StdEncoding.EncodeToString([]byte("hello\n")) ||
		logs.NextCursor != "rt1.next" {
		t.Fatalf("logs = %+v", logs)
	}
	if got := strings.Join(paths, ","); got != "/api/runs?cursor=cursor-1&limit=25&status=running&status=waiting,/api/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" {
		t.Fatalf("paths = %s", got)
	}
}

func TestRevokeSecretUsesExplicitOperation(t *testing.T) {
	var request api.RevokeSecretRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/secrets/by-name/API_TOKEN/revoke" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SecretResponse{
			ID:    "secret-1",
			Name:  "API_TOKEN",
			State: "revoked",
		})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.RevokeSecret(context.Background(), "API_TOKEN", "revoke-1")
	if err != nil {
		t.Fatal(err)
	}
	if request.IdempotencyKey != "revoke-1" || snapshot.State != "revoked" {
		t.Fatalf("request = %+v snapshot = %+v", request, snapshot)
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
	if _, err := client.GetRun(context.Background(), "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("GetRun err = %v", err)
	}
	if _, err := client.ListSecrets(context.Background()); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("ListSecrets err = %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(sourcePath, []byte("deployment source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateDeployment(
		context.Background(),
		api.CreateDeploymentRequest{IdempotencyKey: "deploy-1"},
		sourcePath,
	); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("CreateDeployment err = %v", err)
	}
}

func TestListRunLogsSendsCursorAndFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" ||
			r.URL.Query().Get("cursor") != "rt1.previous" ||
			r.URL.Query().Get("limit") != "25" ||
			strings.Join(r.URL.Query()["level"], ",") != "warn,error" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		observed := int64(2)
		bytes := int64(6)
		_ = json.NewEncoder(w).Encode(api.RunLogPage{Logs: []api.RunLogRecord{{
			ID:               "rt1.log",
			RunID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
			AttemptNumber:    1,
			Kind:             "stdout",
			ContentBase64:    base64.StdEncoding.EncodeToString([]byte("hello\n")),
			Bytes:            &bytes,
			ObservedSequence: &observed,
			At:               time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
		}}, NextCursor: "rt1.next"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListRunLogs(
		context.Background(),
		"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
		ListRunLogsOptions{
			Cursor: "rt1.previous", Limit: 25, Levels: []string{"warn", "error"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Logs) != 1 || page.Logs[0].ID != "rt1.log" ||
		page.NextCursor != "rt1.next" {
		t.Fatalf("page = %+v", page)
	}
}

func TestWorkerLifecycleClient(t *testing.T) {
	claim := workerapi.RunLease{
		ID: "00000000-0000-0000-0000-000000000001", RunID: "00000000-0000-0000-0000-000000000002",
		WorkerGroupID: "run-us-east-1", WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
		WorkerEpoch: 1, LeaseSequence: 1, RuntimeInstanceID: "00000000-0000-0000-0000-000000000501",
		AttemptNumber: 1, ProtocolVersion: workerapi.CurrentProtocolVersion,
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
			var request workerapi.TokenRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || request.WorkerInstanceSecret != "worker-secret" || request.ServiceID != "00000000-0000-0000-0000-000000000901" || request.ProtocolVersion != workerapi.CurrentProtocolVersion || !request.SupportsRun || request.SupportsBuild {
				t.Fatalf("worker token request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
				Token:            workerToken,
				ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/api/worker/leases/discover":
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
		case "/api/worker/activate":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.ActivateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Capabilities.RuntimeArch != "arm64" {
				t.Fatalf("activate capabilities = %+v", request.Capabilities)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusActive})
		case "/api/worker/drain":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusDraining, ActiveExecutions: 1})
		case "/api/worker/drain/complete":
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
		case "/api/worker/status":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerInstanceID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusDraining, ActiveExecutions: 1})
		case "/api/worker/fence":
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
		case "/api/worker/leases/start":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StartResponse{RunID: claim.RunID, Status: "running"})
		case "/api/worker/leases/renew":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.RenewResponse{Lease: claim})
		case "/api/worker/leases/release":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerToken {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.ReleaseResponse{RunID: claim.RunID, Status: "succeeded"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithWorkerService("00000000-0000-0000-0000-000000000901", workerapi.CurrentProtocolVersion, true, false))
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
	if _, err := client.StartRun(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RenewRun(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	exitCode := int32(0)
	if _, err := client.ReleaseRun(context.Background(), claim, workerapi.ReleaseResult{Kind: "completed", ExitCode: &exitCode}); err != nil {
		t.Fatal(err)
	}
	if err := client.FenceWorker(context.Background(), "termination_drain_failed"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got != "/api/worker/auth/token,/api/worker/leases/discover,/api/worker/activate,/api/worker/drain,/api/worker/status,/api/worker/drain/complete,/api/worker/leases/start,/api/worker/leases/renew,/api/worker/leases/release,/api/worker/fence" {
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
			case "/api/worker/auth/token":
				_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
					Token:            "worker-token",
					ExpiresInSeconds: 3600,
				})
			case "/api/worker/leases/claim":
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
			case "/api/worker/leases/start":
				var request workerapi.RunStartRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Lease != receipt.Fence() || request.Fresh == nil ||
					request.Restore != nil || request.Attach != nil {
					t.Fatalf("start request = %+v", request)
				}
				_ = json.NewEncoder(w).Encode(
					workerapi.RunStartResponse{Lease: receipt.Fence()},
				)
			case "/api/worker/leases/entrypoint":
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
			case "/api/worker/leases/run-renew":
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
			case "/api/worker/leases/run-logs":
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
			case "/api/worker/leases/finalization/begin":
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
			case "/api/worker/leases/tasks/complete":
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
		WithWorkerAuth(
			"00000000-0000-0000-0000-000000000401",
			"worker-secret",
		),
		WithWorkerService(
			"00000000-0000-0000-0000-000000000901",
			workerapi.CurrentProtocolVersion,
			true,
			false,
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
		"/api/worker/auth/token,/api/worker/leases/claim,"+
			"/api/worker/leases/start,/api/worker/leases/entrypoint,/api/worker/leases/run-renew,"+
			"/api/worker/leases/finalization/begin,/api/worker/leases/tasks/complete,"+
			"/api/worker/leases/run-logs" {
		t.Fatalf("paths = %s", got)
	}
}

func TestCompleteWorkerDrainRetriesTheIdenticalProofAfterAmbiguousResponse(t *testing.T) {
	attempts := 0
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/auth/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "worker-token", ExpiresInSeconds: 3600})
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
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusTerminationReady})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("worker", "secret"), WithWorkerService("service", workerapi.CurrentProtocolVersion, true, false))
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
		case "/api/worker/auth/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "worker-token", ExpiresInSeconds: 3600})
		case "/api/worker/fence":
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
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("worker", "secret"), WithWorkerService("service", workerapi.CurrentProtocolVersion, true, false))
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
		case "/api/worker/auth/token":
			tokenRequests++
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
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
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
		case "/api/worker/status":
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
		WithWorkerAuth("00000000-0000-0000-0000-000000000401", "worker-secret"),
		WithWorkerService("00000000-0000-0000-0000-000000000901", workerapi.CurrentProtocolVersion, true, false))
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
		AttemptNumber: 1, WorkerProtocolVersion: workerapi.CurrentProtocolVersion,
		WorkspaceID:            "00000000-0000-0000-0000-000000000701",
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
		if r.URL.Path == "/api/worker/auth/token" {
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "worker-token", ExpiresInSeconds: int64(time.Hour / time.Second)})
			return
		}
		if got := r.Header.Get("authorization"); got != "Bearer worker-token" {
			t.Fatalf("worker auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/worker/leases/run-waits":
			var request workerapi.CreateRunWaitRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.CorrelationID != "corr-1" || request.Kind != workerapi.RunWaitKindToken || string(request.Params) != `{"prompt":"ship?"}` {
				t.Fatalf("create run wait = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.CreateRunWaitResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1"})
		case "/api/worker/leases/run-waits/poll":
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
		case "/api/worker/leases/run-waits/resume-ack":
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
		case "/api/worker/leases/checkpoints/ready":
			var request workerapi.CheckpointReadyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RequestVersion != 42 || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" {
				t.Fatalf("checkpoint ready request = %+v", request)
			}
			if request.Manifest.RecoveryPoint.Runtime.KernelDigest != kernelDigest || request.Manifest.RecoveryPoint.Runtime.RootfsDigest != rootfsDigest {
				t.Fatalf("checkpoint manifest = %+v", request.Manifest)
			}
			_ = json.NewEncoder(w).Encode(workerapi.CheckpointResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/leases/restores/ack":
			var request workerapi.AcknowledgeRestoreRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Lease.ID != claim.ID || request.RunWaitID != "run-wait-id-1" || request.CheckpointID != "checkpoint-1" {
				t.Fatalf("restore attach request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.AcknowledgeRestoreResponse{RunID: claim.RunID, RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"})
		case "/api/worker/leases/checkpoints/failed":
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

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithWorkerAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithWorkerService("00000000-0000-0000-0000-000000000901", workerapi.CurrentProtocolVersion, true, false))
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
		Manifest:       testClientCheckpointManifest(kernelDigest, rootfsDigest, configDigest, manifestDigest, vmStateDigest, scratchDigest, memoryDigest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready = %+v", ready)
	}
	acknowledged, err := client.AcknowledgeRestore(context.Background(), workerapi.AcknowledgeRestoreRequest{
		Lease: workerapi.RunLease{
			ID: claim.ID, RunID: claim.RunID, AttemptNumber: claim.AttemptNumber,
			LeaseSequence: claim.LeaseSequence, WorkerGroupID: claim.WorkerGroupID,
			WorkerInstanceID: claim.WorkerInstanceID, WorkerEpoch: claim.WorkerEpoch,
			ProtocolVersion: claim.WorkerProtocolVersion,
		},
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledged = %+v", acknowledged)
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
	if got := strings.Join(paths, ","); got != "/api/worker/auth/token,/api/worker/leases/run-waits,/api/worker/leases/run-waits/poll,/api/worker/leases/run-waits/resume-ack,/api/worker/leases/checkpoints/ready,/api/worker/leases/restores/ack,/api/worker/leases/checkpoints/failed" {
		t.Fatalf("paths = %s", got)
	}
}

func TestAcknowledgeRunResumeRelease(t *testing.T) {
	lease := workerapi.RunLeaseAssignment{ID: "lease-1", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", LeaseSequence: 3}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/auth/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{
				Token: "worker-token", ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/api/worker/leases/resume-release":
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
		WithWorkerAuth("worker-1", "worker-secret"),
		WithWorkerService("service-1", workerapi.CurrentProtocolVersion, true, false),
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
			ABI:             "helmr.firecracker.snapshot.v0",
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
		ProtocolVersion:           workerapi.CurrentProtocolVersion,
		RuntimeID:                 "sha256:runtime",
		RuntimeArch:               "arm64",
		RuntimeABI:                "helmr.firecracker.snapshot.v0",
		KernelDigest:              "sha256:kernel",
		InitramfsDigest:           "sha256:initramfs",
		RootfsDigest:              "sha256:rootfs",
		NetworkABI:                "helmr/v0",
		MaxVCPUs:                  2,
		MaxMemoryMiB:              2048,
		VMMilliCPU:                2000,
		VMMemoryMiB:               2048,
		GuestEphemeralDiskBytes:   32768 << 20,
		VMGuestEphemeralDiskBytes: 32768 << 20,
		ExecutionSlotsAvailable:   1,
	}
}
