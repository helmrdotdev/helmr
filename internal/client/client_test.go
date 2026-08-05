package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestClientErrorUsesServerMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(api.HTTPErrorResponse{Error: api.HTTPError{
			Code:    "bad_source",
			Message: "bad source",
		}})
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
		if _, err := New(raw); err == nil || !strings.Contains(err.Error(), "must be an origin without credentials, path, query, or fragment") {
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
		http.Redirect(w, r, "http://helmr.example/v1/tasks/deploy/start", http.StatusTemporaryRedirect)
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
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/deploy/start" {
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
		if request.Workspace.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	started, err := client.StartTask(context.Background(), "deploy", api.StartTaskRequest{
		Payload:   json.RawMessage(`{"env":"prod"}`),
		Workspace: api.WorkspaceIDTarget{ID: workspaceID},
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
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/deploy/start" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(api.HTTPErrorResponse{Error: api.HTTPError{
			Code:    "conflict",
			Message: "already started differently",
		}})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StartTask(context.Background(), "deploy", api.StartTaskRequest{}, EnvironmentScopeOptions{})
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict || !strings.Contains(httpErr.Message, "already started differently") {
		t.Fatalf("err = %#v, want 409 httpclient.Error", err)
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
		case "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/cancel":
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
	if got := strings.Join(paths, ","); got != "POST /v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/cancel" {
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
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments" {
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
		api.CreateDeploymentRequest{
			IdempotencyKey: "deploy-1"},
		sourcePath,
		EnvironmentScopeOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("response = %+v", response)
	}
	if metadata.IdempotencyKey != "deploy-1" ||
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
		api.CreateDeploymentRequest{
			IdempotencyKey: "deploy-retry"},
		sourcePath,
		EnvironmentScopeOptions{},
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
		case "/v1/runs":
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
		case "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs":
			_ = json.NewEncoder(w).Encode(api.RunLogPage{
				Logs: []api.RunLogRecord{{
					ID: "log-cursor", Kind: "stdout", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
					AttemptNumber: 1,
					ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
				}},
				NextCursor: "cursor-next",
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
		logs.NextCursor != "cursor-next" {
		t.Fatalf("logs = %+v", logs)
	}
	if got := strings.Join(paths, ","); got != "/v1/runs?cursor=cursor-1&limit=25&status=running&status=waiting,/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" {
		t.Fatalf("paths = %s", got)
	}
}

func TestRevokeSecretUsesExplicitOperation(t *testing.T) {
	var request api.RevokeSecretRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/secrets/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/revoke" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SecretResponse{
			ID:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
			Name:   "API_TOKEN",
			Status: "revoked",
		})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.RevokeSecret(context.Background(), "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", "revoke-1")
	if err != nil {
		t.Fatal(err)
	}
	if request.IdempotencyKey != "revoke-1" || snapshot.Status != "revoked" {
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
		api.CreateDeploymentRequest{
			IdempotencyKey: "deploy-1"},
		sourcePath,
		EnvironmentScopeOptions{},
	); err == nil || !strings.Contains(err.Error(), "project and environment are required") {
		t.Fatalf("CreateDeployment err = %v", err)
	}
}

func TestListRunLogsSendsCursorAndFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" ||
			r.URL.Query().Get("cursor") != "cursor-previous" ||
			r.URL.Query().Get("limit") != "25" ||
			strings.Join(r.URL.Query()["level"], ",") != "warn,error" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		observed := int64(2)
		bytes := int64(6)
		_ = json.NewEncoder(w).Encode(api.RunLogPage{Logs: []api.RunLogRecord{{
			ID:               "log-cursor",
			RunID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
			AttemptNumber:    1,
			Kind:             "stdout",
			ContentBase64:    base64.StdEncoding.EncodeToString([]byte("hello\n")),
			Bytes:            &bytes,
			ObservedSequence: &observed,
			At:               time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
		}}, NextCursor: "cursor-next"})
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
			Cursor: "cursor-previous", Limit: 25, Levels: []string{"warn", "error"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Logs) != 1 || page.Logs[0].ID != "log-cursor" ||
		page.NextCursor != "cursor-next" {
		t.Fatalf("page = %+v", page)
	}
}
