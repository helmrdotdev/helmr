package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestReadWorkerEnrollmentToken(t *testing.T) {
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker-token")
	if err := os.WriteFile(path, []byte(token.Raw), 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := readWorkerEnrollmentToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != token.Raw {
		t.Fatalf("token = %q", got)
	}
}

func TestReadWorkerEnrollmentTokenRejectsUnsafeFiles(t *testing.T) {
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, []byte(token.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	wrongMode := filepath.Join(dir, "wrong-mode")
	if err := os.WriteFile(wrongMode, []byte(token.Raw), 0o644); err != nil {
		t.Fatal(err)
	}
	newline := filepath.Join(dir, "newline")
	if err := os.WriteFile(newline, []byte(token.Raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", 129)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{symlink, wrongMode, newline, large} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := readWorkerEnrollmentToken(path); err == nil {
				t.Fatal("unsafe token file accepted")
			}
		})
	}
}

// A control plane on another worker API version rejects the stored secret's
// host credential request with a 409, not a 401: the worker keeps its stored
// secret and neither re-enrolls nor retries.
func TestWorkerHostSecretSurvivesAPIVersionMismatch(t *testing.T) {
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"version mismatch"}}`, workerapi.APIVersionMismatchCode)
	}))
	defer server.Close()
	workDir := t.TempDir()
	stored := workerHostSecretFile{WorkerHostID: "host", WorkerHostSecret: "hlmr_wi_secret", CreatedAt: time.Now().UTC()}
	path := workerHostSecretPath(workDir, "")
	if err := writeWorkerHostSecret(path, stored); err != nil {
		t.Fatal(err)
	}
	cfg := config.Worker{ControlPlaneURL: server.URL}
	_, err := resolveAuthenticatedWorkerHostSecret(t.Context(), cfg, workDir, func(hostSecret workerHostSecretFile) error {
		client, err := workerclient.New(server.URL, workerclient.WithHTTPClient(server.Client()),
			workerclient.WithAuth(hostSecret.WorkerHostID, hostSecret.WorkerHostSecret), workerclient.WithService("service"))
		if err != nil {
			return err
		}
		return client.AuthenticateWorker(t.Context())
	})
	var mismatch *httpclient.Error
	if !errors.As(err, &mismatch) || mismatch.Code != workerapi.APIVersionMismatchCode {
		t.Fatalf("error = %v, want version mismatch", err)
	}
	if kept, err := readWorkerHostSecret(path); err != nil || kept.WorkerHostID != stored.WorkerHostID || kept.WorkerHostSecret != stored.WorkerHostSecret {
		t.Fatalf("stored host secret = %+v, err = %v; want it kept", kept, err)
	}
	if len(requests) != 1 || requests["/worker/v1/instance/credential"] != 1 {
		t.Fatalf("requests = %v, want one host credential request", requests)
	}
}
