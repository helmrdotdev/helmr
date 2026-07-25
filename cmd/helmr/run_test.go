package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRunCancelCommandCancelsRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/run-1/cancel" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.ContentLength > 0 {
			t.Fatalf("cancel request body length = %d", r.ContentLength)
		}
		_ = json.NewEncoder(w).Encode(api.RunSnapshotResponse{
			ID: "run-1", Status: "cancelled",
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "cancel", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	expected := strings.Join([]string{
		"run_id: run-1",
		"run_status: cancelled",
		"",
	}, "\n")
	if out.String() != expected {
		t.Fatalf("output = %q, want %q", out.String(), expected)
	}
}
