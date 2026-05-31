package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRunTable(t *testing.T) {
	var out bytes.Buffer
	runs := []api.RunResponse{{
		ID:     "1234567890abcdef",
		TaskID: "build",
		Status: "running",
		PendingWaitpoint: &api.PendingWaitpoint{
			Kind:        "token",
			WaitpointID: "wait-1",
		},
	}}

	RunTable(&out, runs)

	got := out.String()
	if !strings.Contains(got, "RUN ID") || !strings.Contains(got, "1234567890ab") || !strings.Contains(got, "token:wait-1") {
		t.Fatalf("RunTable() = %q", got)
	}
}

func TestRunDetails(t *testing.T) {
	var out bytes.Buffer
	exitCode := int32(0)
	run := api.RunResponse{
		ID:        "run-1",
		TaskID:    "build",
		Status:    "succeeded",
		ExitCode:  &exitCode,
		CreatedAt: time.Date(2026, 5, 10, 1, 2, 3, 0, time.UTC),
		UpdatedAt: time.Date(2026, 5, 10, 1, 3, 3, 0, time.UTC),
	}

	RunDetails(&out, run)

	got := out.String()
	for _, want := range []string{
		"ID:       run-1",
		"Task:     build",
		"Status:   succeeded",
		"Exit:     0",
		"Created:  2026-05-10T01:02:03Z",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("RunDetails() = %q, missing %q", got, want)
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
