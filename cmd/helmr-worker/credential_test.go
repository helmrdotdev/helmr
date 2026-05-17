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

func TestResolveWorkerCredentialKeepsRegistrationTokenOnClientConfigFailure(t *testing.T) {
	tempDir := t.TempDir()
	tokenPath := filepath.Join(tempDir, "registration-token")
	if err := os.WriteFile(tokenPath, []byte("registration-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resolveWorkerCredential(context.Background(), config.Worker{
		ControlURL:                      "http://helmr.example",
		WorkerID:                        "host-1",
		WorkerPoolRegistrationTokenPath: tokenPath,
	}, tempDir)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(tokenPath); statErr != nil {
		t.Fatalf("registration token should remain after config failure: %v", statErr)
	}
}

func TestResolveWorkerCredentialRemovesRegistrationTokenAfterSavingCredential(t *testing.T) {
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
		if request.ResourceName != "host-1" || request.RegistrationToken != "registration-token" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.WorkerRegisterResponse{
			WorkerID:     "worker-generated-1",
			WorkerSecret: "worker-secret",
		})
	}))
	defer server.Close()

	credential, err := resolveWorkerCredential(context.Background(), config.Worker{
		ControlURL:                      server.URL,
		WorkerID:                        "host-1",
		WorkerPoolRegistrationTokenPath: tokenPath,
	}, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if credential.WorkerID != "worker-generated-1" || credential.WorkerSecret != "worker-secret" {
		t.Fatalf("credential = %+v", credential)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("registration token should be removed after saving credential, stat err = %v", err)
	}
	stored, err := readWorkerCredential(workerCredentialPath(tempDir, ""))
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkerID != "worker-generated-1" || stored.WorkerSecret != "worker-secret" {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestResolveWorkerControlCredentialReadsStoredWorkerID(t *testing.T) {
	tempDir := t.TempDir()
	if err := writeWorkerSecret(workerCredentialPath(tempDir, ""), workerCredentialFile{
		WorkerID:     "worker-generated-1",
		WorkerSecret: "worker-secret",
	}); err != nil {
		t.Fatal(err)
	}

	credential, err := resolveWorkerControlCredential(config.WorkerControl{WorkerID: "host-1"}, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if credential.WorkerID != "worker-generated-1" || credential.WorkerSecret != "worker-secret" {
		t.Fatalf("credential = %+v", credential)
	}
}
