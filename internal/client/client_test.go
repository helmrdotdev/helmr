package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestUploadDeploymentBundleObjectRejectsNonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s", r.Method)
		}
		if r.ContentLength != 6 {
			t.Fatalf("content length = %d", r.ContentLength)
		}
		for name, want := range map[string]string{
			"Content-Type":                 "application/vnd.helmr.deployment-program.v0+squashfs",
			"If-None-Match":                "*",
			"X-Amz-Checksum-Sha256":        "checksum",
			"X-Amz-Sdk-Checksum-Algorithm": "SHA256",
			"X-Amz-Tagging":                "helmr-expirable=true",
		} {
			if got := r.Header.Get(name); got != want {
				t.Fatalf("header %s = %q, want %q", name, got, want)
			}
		}
		http.Error(w, "rejected", http.StatusForbidden)
	}))
	defer server.Close()
	object := t.TempDir() + "/object"
	if err := os.WriteFile(object, []byte("object"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New("http://localhost", WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	err = client.UploadDeploymentBundleObject(t.Context(), api.DeploymentBundleUpload{
		Method: http.MethodPut, URL: server.URL,
		Headers: map[string]string{
			"Content-Length":               "6",
			"Content-Type":                 "application/vnd.helmr.deployment-program.v0+squashfs",
			"If-None-Match":                "*",
			"X-Amz-Checksum-Sha256":        "checksum",
			"X-Amz-Sdk-Checksum-Algorithm": "SHA256",
			"X-Amz-Tagging":                "helmr-expirable=true",
		},
	}, object, nil)
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("error = %v", err)
	}
}

func TestUploadDeploymentBundleObjectRequiresExactContentLength(t *testing.T) {
	object := t.TempDir() + "/object"
	if err := os.WriteFile(object, []byte("object"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New("http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	for name, headers := range map[string]map[string]string{
		"missing":   nil,
		"invalid":   {"Content-Length": "invalid"},
		"mismatch":  {"Content-Length": "7"},
		"duplicate": {"Content-Length": "6", "content-length": "6"},
	} {
		t.Run(name, func(t *testing.T) {
			err := client.UploadDeploymentBundleObject(t.Context(), api.DeploymentBundleUpload{
				Method:  http.MethodPut,
				URL:     "http://localhost/upload",
				Headers: headers,
			}, object, nil)
			if err == nil {
				t.Fatal("UploadDeploymentBundleObject returned nil error")
			}
		})
	}
}

func TestUploadDeploymentBundleObjectScrubsMalformedPresignedURL(t *testing.T) {
	const secret = "presigned-secret-sentinel"
	object := t.TempDir() + "/object"
	if err := os.WriteFile(object, []byte("object"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New("http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	err = client.UploadDeploymentBundleObject(t.Context(), api.DeploymentBundleUpload{
		Method: http.MethodPut,
		URL:    "http://localhost/%zz?X-Amz-Signature=" + secret,
		Headers: map[string]string{
			"Content-Length": "6",
		},
	}, object, nil)
	if err == nil ||
		!errors.Is(err, ErrDeploymentObjectUploadNotAttempted) ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

func TestFinalizeDeploymentBundleConsumesTypedEventStream(t *testing.T) {
	bundleDigest := "sha256:" + strings.Repeat("a", 64)
	objectDigest := "sha256:" + strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployment-bundles/finalize" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Accept = %q", r.Header.Get("Accept"))
		}
		var request api.FinalizeDeploymentBundleRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.BundleDigest != bundleDigest || request.IdempotencyKey != "deploy-test" {
			t.Fatalf("request = %+v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w,
			"event: started\ndata: {\"bundle_digest\":\""+bundleDigest+"\"}\n\n"+
				"event: ping\ndata: {}\n\n"+
				"event: object_verified\ndata: {\"digest\":\""+objectDigest+"\"}\n\n"+
				"event: complete\ndata: {\"id\":\"deployment-1\",\"version\":\"v1\",\"bundle_digest\":\""+bundleDigest+"\",\"created_at\":\"2026-08-15T00:00:00Z\"}\n\n",
		)
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	var verified []string
	created, err := client.FinalizeDeploymentBundle(t.Context(), api.FinalizeDeploymentBundleRequest{
		IdempotencyKey: "deploy-test", BundleDigest: bundleDigest,
	}, EnvironmentScopeOptions{}, func(object api.DeploymentBundleFinalizeObject) error {
		verified = append(verified, object.Digest)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "deployment-1" || !slices.Equal(verified, []string{objectDigest}) {
		t.Fatalf("created = %+v, verified = %v", created, verified)
	}
}

func TestFinalizeDeploymentBundleDoesNotReconnectAfterTruncatedStream(t *testing.T) {
	bundleDigest := "sha256:" + strings.Repeat("a", 64)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: started\ndata: {\"bundle_digest\":\""+bundleDigest+"\"}\n\n")
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FinalizeDeploymentBundle(t.Context(), api.FinalizeDeploymentBundleRequest{
		IdempotencyKey: "deploy-test", BundleDigest: bundleDigest,
	}, EnvironmentScopeOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "ended without a terminal event") {
		t.Fatalf("error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestFinalizeDeploymentBundleReturnsTypedTerminalError(t *testing.T) {
	bundleDigest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: started\ndata: {\"bundle_digest\":\""+bundleDigest+"\"}\n\n"+
				"event: error\ndata: {\"code\":\"invalid_deployment_object\",\"message\":\"deployment object failed verification\"}\n\n",
		)
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FinalizeDeploymentBundle(t.Context(), api.FinalizeDeploymentBundleRequest{
		IdempotencyKey: "deploy-test", BundleDigest: bundleDigest,
	}, EnvironmentScopeOptions{}, nil)
	var terminal *DeploymentFinalizeError
	if !errors.As(err, &terminal) || terminal.Code != "invalid_deployment_object" {
		t.Fatalf("error = %#v", err)
	}
}

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

func TestExecuteComputerAdmissionAndIndependentRetrieval(t *testing.T) {
	const computerID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	const commandID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			requests := 0
			prefix := "/v1"
			if scoped {
				prefix = "/api/projects/project/environments/environment"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					if r.Method != "POST" || r.URL.Path != prefix+"/computers/"+computerID+"/exec" {
						t.Errorf("admission %s %s", r.Method, r.URL.Path)
					}
					writeTestJSON(t, w, 202, api.CommandReceipt{CommandID: commandID})
					return
				}
				if r.Method != "GET" || r.URL.Path != prefix+"/commands/"+commandID {
					t.Errorf("retrieve %s %s", r.Method, r.URL.Path)
				}
				exitCode := int32(17)
				writeTestJSON(t, w, 200, api.CommandInfo{ID: commandID, ComputerID: computerID, Status: "exited", Outcome: &api.CommandOutcome{CommandID: commandID, Kind: "exited", TerminalAt: time.Now(), ExitCode: &exitCode}})
			}))
			defer server.Close()
			options := []Option{WithHTTPClient(server.Client())}
			if scoped {
				options = append(options, WithSessionScopedRoutes())
			}
			client, err := New(server.URL, options...)
			if err != nil {
				t.Fatal(err)
			}
			scope := ComputerScopeOptions{}
			if scoped {
				scope = ComputerScopeOptions{ProjectID: "project", EnvironmentID: "environment"}
			}
			receipt, err := client.ExecuteComputer(t.Context(), computerID, api.ExecuteComputerRequest{}, scope)
			if err != nil || receipt.CommandID != commandID || requests != 1 {
				t.Fatalf("admission %+v %v requests=%d", receipt, err, requests)
			}
			info, err := client.RetrieveCommand(t.Context(), commandID, scope)
			if err != nil || info.Outcome == nil || *info.Outcome.ExitCode != 17 || requests != 2 {
				t.Fatalf("retrieve %+v %v requests=%d", info, err, requests)
			}
		})
	}
}

func TestRetrieveCommandRejectsIDDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, 200, api.CommandInfo{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37"})
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RetrieveCommand(t.Context(), "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36", ComputerScopeOptions{})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("error = %v", err)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
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
		if request.Computer.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	computerID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	started, err := client.StartTask(context.Background(), "deploy", api.StartTaskRequest{
		Payload:  json.RawMessage(`{"env":"prod"}`),
		Computer: api.ComputerIDTarget{ID: computerID},
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
	for _, scoped := range []bool{false, true} {
		for _, actor := range []bool{false, true} {
			name := "task"
			if actor {
				name = "actor"
			}
			t.Run(name+scopeName(scoped), func(t *testing.T) {
				scope, prefix := sessionRouteScope(scoped)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != prefix+"/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/cancel" {
						t.Errorf("request = %s %s", r.Method, r.URL.Path)
					}
					var request api.CancelRunRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if ids.Validate(request.IdempotencyKey) != nil {
						t.Errorf("idempotency key = %q", request.IdempotencyKey)
					}
					if actor {
						_ = json.NewEncoder(w).Encode(api.ActorRunCancellationReceipt{ID: "cancel-1", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", SessionID: testSessionID, HoldID: testHoldID, Status: "accepted"})
					} else {
						_ = json.NewEncoder(w).Encode(api.RunSnapshotResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: "cancelled"})
					}
				}))
				defer server.Close()
				c := sessionTestClient(t, server, scoped)
				result, err := c.CancelRun(context.Background(), "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", api.CancelRunRequest{}, RunScopeOptions(scope))
				if err != nil {
					t.Fatal(err)
				}
				if actor {
					if result.Task != nil || result.Actor == nil || result.Actor.ID != "cancel-1" || result.Actor.RunID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" || result.Actor.SessionID != testSessionID || result.Actor.HoldID != testHoldID || result.Actor.Status != "accepted" {
						t.Fatalf("result = %+v", result)
					}
				} else if result.Actor != nil || result.Task == nil || result.Task.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" || result.Task.Status != "cancelled" {
					t.Fatalf("result = %+v", result)
				}
			})
		}
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
	observedSequence := int64(1)
	byteCount := int64(len("hello\n"))
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/v1/runs":
			if got := r.URL.Query()["status"]; !slices.Equal(got, []string{"running", "waiting"}) ||
				!slices.Equal(r.URL.Query()["kind"], []string{"actor"}) ||
				r.URL.Query().Get("session_id") != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
				r.URL.Query().Get("cursor") != "cursor-1" ||
				r.URL.Query().Get("limit") != "25" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.ListRunsResponse{
				Runs: []api.RunListItem{{
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
					AttemptNumber: 1, ObservedSequence: &observedSequence, Bytes: &byteCount, At: now,
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
		Statuses:  []string{"running", "waiting"},
		Kinds:     []string{"actor"},
		SessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
		Cursor:    "cursor-1",
		Limit:     25,
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
	if got := strings.Join(paths, ","); got != "/v1/runs?cursor=cursor-1&kind=actor&limit=25&session_id=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33&status=running&status=waiting,/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" {
		t.Fatalf("paths = %s", got)
	}
}

func TestListProjectsOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/api/projects?cursor=cursor-1&limit=50" {
			t.Fatalf("request URI = %s", r.URL.RequestURI())
		}
		_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{NextCursor: "cursor-2"})
	}))
	defer server.Close()

	controlPlane, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	page, err := controlPlane.ListProjects(context.Background(), ListProjectsOptions{Cursor: "cursor-1", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "cursor-2" {
		t.Fatalf("page = %+v", page)
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
