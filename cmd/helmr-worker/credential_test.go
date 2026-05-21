package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/config"
)

func TestResolveWorkerInstanceCredentialKeepsBootstrapTokenOnClientConfigFailure(t *testing.T) {
	tempDir := t.TempDir()
	tokenPath := filepath.Join(tempDir, "registration-token")
	if err := os.WriteFile(tokenPath, []byte("registration-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resolveWorkerInstanceCredential(context.Background(), config.Worker{
		ControlURL:               "http://helmr.example",
		WorkerResourceID:         "host-1",
		WorkerBootstrapTokenPath: tokenPath,
	}, tempDir)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(tokenPath); statErr != nil {
		t.Fatalf("bootstrap token should remain after config failure: %v", statErr)
	}
}

func TestResolveWorkerInstanceCredentialRemovesBootstrapTokenAfterSavingCredential(t *testing.T) {
	tempDir := t.TempDir()
	tokenPath := filepath.Join(tempDir, "registration-token")
	if err := os.WriteFile(tokenPath, []byte("registration-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/worker/register" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		var request api.WorkerRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ResourceID != "host-1" || request.BootstrapToken != "registration-token" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.WorkerRegisterResponse{
			WorkerInstanceID:     "00000000-0000-0000-0000-000000000401",
			WorkerInstanceSecret: "worker-secret",
		})
	}))
	defer server.Close()

	credential, err := resolveWorkerInstanceCredential(context.Background(), config.Worker{
		ControlURL:               server.URL,
		WorkerResourceID:         "host-1",
		WorkerBootstrapTokenPath: tokenPath,
	}, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if credential.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || credential.WorkerInstanceSecret != "worker-secret" {
		t.Fatalf("credential = %+v", credential)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("bootstrap token should be removed after saving credential, stat err = %v", err)
	}
	stored, err := readWorkerInstanceCredential(workerCredentialPath(tempDir, ""))
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkerInstanceID != "00000000-0000-0000-0000-000000000401" || stored.WorkerInstanceSecret != "worker-secret" {
		t.Fatalf("stored = %+v", stored)
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
