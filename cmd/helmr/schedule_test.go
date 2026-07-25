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

func TestScheduleListAndGet(t *testing.T) {
	const scheduleID = "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/schedules":
			if r.URL.Query().Get("cursor") != "sc1.cursor" ||
				r.URL.Query().Get("limit") != "25" {
				t.Fatalf("query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.ListSchedulesResponse{
				Schedules: []api.ScheduleResponse{{
					ID: scheduleID, Task: "nightly", Status: "active",
				}},
				NextCursor: "sc1.next",
			})
		case "/api/schedules/" + scheduleID:
			_ = json.NewEncoder(w).Encode(api.ScheduleResponse{
				ID: scheduleID, Task: "nightly", Status: "active",
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"schedule", "list", "--cursor", "sc1.cursor", "--limit", "25"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != scheduleID+"\tnightly\tactive\nnext_cursor: sc1.next\n" {
		t.Fatalf("list output = %q", out.String())
	}

	out.Reset()
	cmd = newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"schedule", "get", scheduleID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"schedule_id: " + scheduleID,
		"task: nightly",
		"status: active",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("get output = %q, missing %q", out.String(), expected)
		}
	}
}

func TestScheduleListRejectsExplicitZeroLimit(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://127.0.0.1")
	t.Setenv(helmrAPIKeyEnv, "test-key")
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"schedule", "list", "--limit", "0"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--limit must be in [1,100]") {
		t.Fatalf("error = %v", err)
	}
}
