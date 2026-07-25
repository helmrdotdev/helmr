package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

const testScheduleID = "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestScheduleReadsUseAPIKeyRoutes(t *testing.T) {
	testScheduleReads(
		t,
		false,
		"/api/schedules",
		EnvironmentScopeOptions{},
	)
}

func TestScheduleReadsUseSessionRoutes(t *testing.T) {
	testScheduleReads(
		t,
		true,
		"/api/projects/project-1/environments/env-1/schedules",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func testScheduleReads(
	t *testing.T,
	session bool,
	collectionPath string,
	scope EnvironmentScopeOptions,
) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case collectionPath:
			if r.Method != http.MethodGet ||
				r.URL.Query().Get("cursor") != "sc1.cursor" ||
				r.URL.Query().Get("limit") != "25" {
				t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
			}
			_ = json.NewEncoder(w).Encode(api.ListSchedulesResponse{
				Schedules:  []api.ScheduleResponse{{ID: testScheduleID, Task: "nightly", Status: "active"}},
				NextCursor: "sc1.next",
			})
		case collectionPath + "/" + testScheduleID:
			if r.Method != http.MethodGet {
				t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
			}
			_ = json.NewEncoder(w).Encode(api.ScheduleResponse{
				ID: testScheduleID, Task: "nightly", Status: "active",
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	control, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	page, err := control.ListSchedules(context.Background(), ListSchedulesOptions{
		Cursor: "sc1.cursor", Limit: 25, EnvironmentScopeOptions: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Schedules) != 1 || page.NextCursor != "sc1.next" {
		t.Fatalf("page = %+v", page)
	}
	schedule, err := control.GetSchedule(context.Background(), testScheduleID, scope)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ID != testScheduleID || schedule.Task != "nightly" {
		t.Fatalf("schedule = %+v", schedule)
	}
}

func TestScheduleReadsValidateInput(t *testing.T) {
	control, err := New("http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.ListSchedules(
		context.Background(),
		ListSchedulesOptions{Limit: 101},
	); err == nil {
		t.Fatal("ListSchedules() accepted an oversized limit")
	}
	if _, err := control.GetSchedule(
		context.Background(),
		"schedule",
		EnvironmentScopeOptions{},
	); err == nil {
		t.Fatal("GetSchedule() accepted an invalid ID")
	}
}
