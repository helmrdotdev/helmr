package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
)

func TestResolveWorkerInstanceCredentialUsesEnrollment(t *testing.T) {
	tempDir := t.TempDir()
	originalBuilder := buildWorkerEnrollmentRequest
	buildWorkerEnrollmentRequest = func(_ context.Context, groupID string, nonce string) (api.WorkerEnrollmentRequest, error) {
		if groupID != "run-workers" || nonce != "fresh-nonce" {
			t.Fatalf("builder group=%q nonce=%q", groupID, nonce)
		}
		return api.WorkerEnrollmentRequest{
			WorkerGroupID: groupID, Nonce: nonce,
			InstanceIdentityDocument: json.RawMessage(`{"instanceId":"i-managed"}`),
			SignedSTSRequest:         api.SignedHTTPRequest{Method: http.MethodPost, URL: "https://sts.us-east-1.amazonaws.com/"},
		}, nil
	}
	t.Cleanup(func() { buildWorkerEnrollmentRequest = originalBuilder })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/enrollment/challenge":
			var request api.WorkerEnrollmentChallengeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerGroupID != "run-workers" {
				t.Fatalf("challenge = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEnrollmentChallengeResponse{Nonce: "fresh-nonce", WorkerGroupID: "run-workers"})
		case "/api/worker/enrollment":
			var request api.WorkerEnrollmentRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerGroupID != "run-workers" || request.Nonce != "fresh-nonce" {
				t.Fatalf("enrollment = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.WorkerEnrollmentResponse{
				WorkerInstanceID: "00000000-0000-0000-0000-000000000402",
				WorkerGroupID:    "run-workers", WorkerInstanceSecret: "managed-secret",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	credential, err := resolveWorkerInstanceCredential(context.Background(), config.Worker{
		ControlURL: server.URL, WorkerGroupID: "run-workers", WorkerRoles: []string{"run"},
	}, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if credential.WorkerInstanceID != "00000000-0000-0000-0000-000000000402" || credential.WorkerInstanceSecret != "managed-secret" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestResolveWorkerInstanceCredentialSerializesEnrollment(t *testing.T) {
	tempDir := t.TempDir()
	originalBuilder := buildWorkerEnrollmentRequest
	buildWorkerEnrollmentRequest = func(_ context.Context, groupID string, nonce string) (api.WorkerEnrollmentRequest, error) {
		return api.WorkerEnrollmentRequest{WorkerGroupID: groupID, Nonce: nonce}, nil
	}
	t.Cleanup(func() { buildWorkerEnrollmentRequest = originalBuilder })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/enrollment/challenge":
			_ = json.NewEncoder(w).Encode(api.WorkerEnrollmentChallengeResponse{Nonce: "nonce", WorkerGroupID: "run-workers"})
		case "/api/worker/enrollment":
			requests.Add(1)
			_ = json.NewEncoder(w).Encode(api.WorkerEnrollmentResponse{
				WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
				WorkerGroupID:    "run-workers", WorkerInstanceSecret: "worker-secret",
			})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := config.Worker{ControlURL: server.URL, WorkerGroupID: "run-workers", WorkerRoles: []string{"run"}}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	credentials := make(chan workerCredentialFile, 2)
	for range 2 {
		wg.Go(func() {
			credential, err := resolveWorkerInstanceCredential(context.Background(), cfg, tempDir)
			if err != nil {
				errs <- err
				return
			}
			credentials <- credential
		})
	}
	wg.Wait()
	close(errs)
	close(credentials)
	for err := range errs {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("enrollment requests = %d, want 1", requests.Load())
	}
	if len(credentials) != 2 {
		t.Fatalf("credentials = %d, want 2", len(credentials))
	}
	for credential := range credentials {
		if credential.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || credential.WorkerInstanceSecret != "worker-secret" {
			t.Fatalf("credential = %+v", credential)
		}
	}
}

func TestResolveWorkerControlCredentialReadsStoredWorkerInstanceID(t *testing.T) {
	tempDir := t.TempDir()
	if err := writeWorkerInstanceSecret(workerCredentialPath(tempDir, ""), workerCredentialFile{
		WorkerInstanceID:     "00000000-0000-0000-0000-000000000401",
		WorkerInstanceSecret: "worker-secret",
	}); err != nil {
		t.Fatal(err)
	}

	credential, err := resolveWorkerControlCredential(config.WorkerControl{}, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if credential.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || credential.WorkerInstanceSecret != "worker-secret" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestReadWorkerInstanceCredentialRejectsSymlinkAndBroadMode(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "target")
	if err := writeWorkerInstanceSecret(target, workerCredentialFile{WorkerInstanceID: "worker", WorkerInstanceSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tempDir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkerInstanceCredential(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkerInstanceCredential(target); err == nil || !strings.Contains(err.Error(), "mode 0600") {
		t.Fatalf("broad mode error = %v", err)
	}
}

func TestResolveAuthenticatedWorkerCredentialReenrollsAfterUnauthorized(t *testing.T) {
	tempDir := t.TempDir()
	path := workerCredentialPath(tempDir, "")
	if err := writeWorkerInstanceSecret(path, workerCredentialFile{
		WorkerInstanceID: "00000000-0000-0000-0000-000000000401", WorkerInstanceSecret: "rejected-secret",
	}); err != nil {
		t.Fatal(err)
	}
	originalBuilder := buildWorkerEnrollmentRequest
	buildWorkerEnrollmentRequest = func(_ context.Context, groupID string, nonce string) (api.WorkerEnrollmentRequest, error) {
		return api.WorkerEnrollmentRequest{WorkerGroupID: groupID, Nonce: nonce}, nil
	}
	t.Cleanup(func() { buildWorkerEnrollmentRequest = originalBuilder })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/enrollment/challenge":
			_ = json.NewEncoder(w).Encode(api.WorkerEnrollmentChallengeResponse{Nonce: "replacement-nonce", WorkerGroupID: "build-workers"})
		case "/api/worker/enrollment":
			_ = json.NewEncoder(w).Encode(api.WorkerEnrollmentResponse{
				WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
				WorkerGroupID:    "build-workers", WorkerInstanceSecret: "replacement-secret",
			})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()

	var attempts int
	credential, err := resolveAuthenticatedWorkerCredential(context.Background(), config.Worker{
		ControlURL: server.URL, WorkerGroupID: "build-workers", WorkerRoles: []string{"build"},
	}, tempDir, func(candidate workerCredentialFile) error {
		attempts++
		if candidate.WorkerInstanceSecret == "rejected-secret" {
			return &client.HTTPError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}
		}
		if candidate.WorkerInstanceSecret != "replacement-secret" {
			t.Fatalf("unexpected candidate secret %q", candidate.WorkerInstanceSecret)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("authentication attempts = %d, want 2", attempts)
	}
	if credential.WorkerInstanceSecret != "replacement-secret" {
		t.Fatalf("credential secret = %q", credential.WorkerInstanceSecret)
	}
	stored, err := readWorkerInstanceCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkerInstanceSecret != "replacement-secret" {
		t.Fatalf("stored credential secret = %q", stored.WorkerInstanceSecret)
	}
}

func TestResolveAuthenticatedWorkerCredentialPreservesNonUnauthorizedCredential(t *testing.T) {
	tempDir := t.TempDir()
	path := workerCredentialPath(tempDir, "")
	rejected := workerCredentialFile{
		WorkerInstanceID: "00000000-0000-0000-0000-000000000401", WorkerInstanceSecret: "preserved-secret",
	}
	if err := writeWorkerInstanceSecret(path, rejected); err != nil {
		t.Fatal(err)
	}
	_, err := resolveAuthenticatedWorkerCredential(context.Background(), config.Worker{}, tempDir, func(workerCredentialFile) error {
		return &client.HTTPError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
	})
	if err == nil {
		t.Fatal("expected authentication error")
	}
	stored, readErr := readWorkerInstanceCredential(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if stored.WorkerInstanceID != rejected.WorkerInstanceID || stored.WorkerInstanceSecret != rejected.WorkerInstanceSecret {
		t.Fatalf("stored credential = %+v", stored)
	}
}
