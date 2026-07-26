package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRunListCommandUsesSnapshotPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query()["status"]; !slices.Equal(got, []string{"running", "waiting"}) ||
			r.URL.Query().Get("cursor") != "cursor-1" ||
			r.URL.Query().Get("limit") != "25" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListRunSnapshotsResponse{
			Runs: []api.RunSnapshotResponse{{
				ID:                   "run_1234567890abcdef",
				Status:               api.RunStatusRunning,
				Entrypoint:           api.RunEntrypointResponse{Kind: "task", ID: "deploy"},
				CurrentAttemptNumber: 2,
			}},
			NextCursor: "cursor-2",
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"run", "list",
		"--status", "running,waiting",
		"--cursor", "cursor-1",
		"--limit", "25",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task:deploy", "running", "2", "Next cursor: cursor-2"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
}

func TestRunGetCommandPrintsSnapshotSemantics(t *testing.T) {
	terminalAt := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.RunSnapshotResponse{
			ID:                   "run-1",
			Status:               api.RunStatusSucceeded,
			Entrypoint:           api.RunEntrypointResponse{Kind: "task", ID: "deploy"},
			Deployment:           api.RunDeploymentResponse{ID: "dep-1", Version: "20260726-test"},
			WorkspaceID:          "ws-1",
			CurrentAttemptNumber: 1,
			Cause:                api.RunCauseResponse{Type: "direct"},
			CreatedAt:            terminalAt.Add(-time.Minute),
			TerminalAt:           &terminalAt,
			TerminalReasonCode:   "completed",
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "get", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Entrypoint:  task deploy",
		"Deployment:  dep-1 (20260726-test)",
		"Workspace:   ws-1",
		"Reason:      completed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
}

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
