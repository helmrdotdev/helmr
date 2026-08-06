package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWriteRunTable(t *testing.T) {
	var out bytes.Buffer
	runs := []api.RunListItem{{
		ID:                   "1234567890abcdef",
		Status:               "running",
		Entrypoint:           api.RunEntrypointResponse{Kind: "task", ID: "build"},
		CurrentAttemptNumber: 2,
	}}

	writeRunTable(&out, runs)

	got := out.String()
	if !strings.Contains(got, "RUN ID") || !strings.Contains(got, "1234567890ab") ||
		!strings.Contains(got, "task:build") || !strings.Contains(got, "2") {
		t.Fatalf("writeRunTable() = %q", got)
	}
}

func TestWriteRunDetails(t *testing.T) {
	var out bytes.Buffer
	startedAt := time.Date(2026, 5, 10, 1, 2, 4, 0, time.UTC)
	terminalAt := time.Date(2026, 5, 10, 1, 3, 3, 0, time.UTC)
	run := api.RunSnapshotResponse{
		ID:                   "run-1",
		Status:               "succeeded",
		Entrypoint:           api.RunEntrypointResponse{Kind: "task", ID: "build"},
		Deployment:           api.DeploymentReference{ID: "dep-1", Version: "20260510-test"},
		WorkspaceID:          "ws-1",
		CurrentAttemptNumber: 1,
		Cause:                api.RunCauseResponse{Type: "direct"},
		CreatedAt:            time.Date(2026, 5, 10, 1, 2, 3, 0, time.UTC),
		StartedAt:            &startedAt,
		TerminalAt:           &terminalAt,
	}

	writeRunDetails(&out, run)

	got := out.String()
	for _, want := range []string{
		"ID:          run-1",
		"Entrypoint:  task build",
		"Deployment:  dep-1 (20260510-test)",
		"Workspace:   ws-1",
		"Status:      succeeded",
		"Attempt:     1",
		"Cause:       direct",
		"Created:     2026-05-10T01:02:03Z",
		"Terminal:    2026-05-10T01:03:03Z",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("writeRunDetails() = %q, missing %q", got, want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("1234567890abcdef"); got != "1234567890ab" {
		t.Fatalf("ShortID() = %q", got)
	}
	if got := shortID("short"); got != "short" {
		t.Fatalf("ShortID(short) = %q", got)
	}
}
