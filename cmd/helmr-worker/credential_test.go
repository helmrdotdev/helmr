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

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const testWorkerEnrollmentSecret = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func TestResolveWorkerInstanceCredentialUsesEnrollment(t *testing.T) {
	tempDir := t.TempDir()
	enrollmentSecretFile := writeTestWorkerEnrollmentSecret(t, testWorkerEnrollmentSecret)
	originalBuilder := buildWorkerEnrollmentRequest
	buildWorkerEnrollmentRequest = func(groupID string, nonce string, supportsRun bool, supportsBuild bool, resourceID string, secret string) (workerapi.EnrollmentRequest, error) {
		if groupID != "run-workers" || nonce != "fresh-nonce" || !supportsRun || supportsBuild || resourceID != "host-1" || secret != testWorkerEnrollmentSecret {
			t.Fatalf("builder group=%q nonce=%q run=%t build=%t resource=%q secret=%q", groupID, nonce, supportsRun, supportsBuild, resourceID, secret)
		}
		return testWorkerEnrollmentRequest(groupID, nonce, supportsRun, supportsBuild), nil
	}
	t.Cleanup(func() { buildWorkerEnrollmentRequest = originalBuilder })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/enrollment/challenge":
			var request workerapi.EnrollmentChallengeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerGroupID != "run-workers" {
				t.Fatalf("challenge = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentChallengeResponse{Nonce: "fresh-nonce", WorkerGroupID: "run-workers"})
		case "/api/worker/enrollment":
			var request workerapi.EnrollmentIntent
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerGroupID != "run-workers" || request.Nonce != "fresh-nonce" || !request.SupportsRun || request.SupportsBuild || request.ProtocolVersion != workerapi.CurrentProtocolVersion {
				t.Fatalf("enrollment = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentResponse{
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
		WorkerResourceID: "host-1", WorkerEnrollmentSecretFile: enrollmentSecretFile,
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
	enrollmentSecretFile := writeTestWorkerEnrollmentSecret(t, testWorkerEnrollmentSecret)
	originalBuilder := buildWorkerEnrollmentRequest
	buildWorkerEnrollmentRequest = func(groupID string, nonce string, supportsRun bool, supportsBuild bool, _ string, _ string) (workerapi.EnrollmentRequest, error) {
		return testWorkerEnrollmentRequest(groupID, nonce, supportsRun, supportsBuild), nil
	}
	t.Cleanup(func() { buildWorkerEnrollmentRequest = originalBuilder })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/enrollment/challenge":
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentChallengeResponse{Nonce: "nonce", WorkerGroupID: "run-workers"})
		case "/api/worker/enrollment":
			requests.Add(1)
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentResponse{
				WorkerInstanceID: "00000000-0000-0000-0000-000000000401",
				WorkerGroupID:    "run-workers", WorkerInstanceSecret: "worker-secret",
			})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := config.Worker{
		ControlURL: server.URL, WorkerGroupID: "run-workers", WorkerRoles: []string{"run"},
		WorkerEnrollmentSecretFile: enrollmentSecretFile,
	}

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
	enrollmentSecretFile := writeTestWorkerEnrollmentSecret(t, testWorkerEnrollmentSecret)
	path := workerCredentialPath(tempDir, "")
	if err := writeWorkerInstanceSecret(path, workerCredentialFile{
		WorkerInstanceID: "00000000-0000-0000-0000-000000000401", WorkerInstanceSecret: "rejected-secret",
	}); err != nil {
		t.Fatal(err)
	}
	originalBuilder := buildWorkerEnrollmentRequest
	buildWorkerEnrollmentRequest = func(groupID string, nonce string, supportsRun bool, supportsBuild bool, _ string, _ string) (workerapi.EnrollmentRequest, error) {
		return testWorkerEnrollmentRequest(groupID, nonce, supportsRun, supportsBuild), nil
	}
	t.Cleanup(func() { buildWorkerEnrollmentRequest = originalBuilder })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/enrollment/challenge":
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentChallengeResponse{Nonce: "replacement-nonce", WorkerGroupID: "build-workers"})
		case "/api/worker/enrollment":
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentResponse{
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
		WorkerEnrollmentSecretFile: enrollmentSecretFile,
	}, tempDir, func(candidate workerCredentialFile) error {
		attempts++
		if candidate.WorkerInstanceSecret == "rejected-secret" {
			return &httpclient.Error{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}
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

func TestResolveWorkerInstanceCredentialRequiresUsableEnrollmentSecretOnlyWhenEnrollmentIsNeeded(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, err := resolveWorkerInstanceCredential(context.Background(), config.Worker{
			WorkerEnrollmentSecretFile: filepath.Join(t.TempDir(), "missing"),
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "HELMR_WORKER_ENROLLMENT_SECRET_FILE") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("noncanonical", func(t *testing.T) {
		secretFile := writeTestWorkerEnrollmentSecret(t, " "+testWorkerEnrollmentSecret+"\n")
		_, err := resolveWorkerInstanceCredential(context.Background(), config.Worker{
			WorkerEnrollmentSecretFile: secretFile,
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "canonical base64url-no-pad") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("stored credential", func(t *testing.T) {
		workDir := t.TempDir()
		want := workerCredentialFile{WorkerInstanceID: "worker", WorkerInstanceSecret: "secret"}
		if err := writeWorkerInstanceSecret(workerCredentialPath(workDir, ""), want); err != nil {
			t.Fatal(err)
		}
		got, err := resolveWorkerInstanceCredential(context.Background(), config.Worker{
			WorkerEnrollmentSecretFile: filepath.Join(t.TempDir(), "missing"),
		}, workDir)
		if err != nil {
			t.Fatal(err)
		}
		if got.WorkerInstanceID != want.WorkerInstanceID || got.WorkerInstanceSecret != want.WorkerInstanceSecret {
			t.Fatalf("credential = %+v", got)
		}
	})
}

func writeTestWorkerEnrollmentSecret(t *testing.T, secret string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker-enrollment-secret")
	if err := os.WriteFile(path, []byte(secret), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func testWorkerEnrollmentRequest(groupID string, nonce string, supportsRun bool, supportsBuild bool) workerapi.EnrollmentRequest {
	return workerapi.EnrollmentRequest{
		EnrollmentIntent: workerapi.EnrollmentIntent{
			WorkerGroupID: groupID, Nonce: nonce, SupportsRun: supportsRun, SupportsBuild: supportsBuild,
			ProtocolVersion: workerapi.CurrentProtocolVersion,
		},
		ResourceID: "host-1",
		Proof:      "proof",
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
		return &httpclient.Error{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
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
