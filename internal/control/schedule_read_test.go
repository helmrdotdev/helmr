package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScheduleListCursorRoundTripAndScope(t *testing.T) {
	raw, err := encodeScheduleListCursor(scheduleListCursor{
		ProjectID:      "project-1",
		EnvironmentID:  "environment-1",
		TaskDeclaredID: "scheduled-maintenance",
		ScheduleID:     "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/schedules?limit=25&cursor="+raw,
		nil,
	)
	limit, cursor, err := parseScheduleListQuery(
		request,
		"project-1",
		"environment-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil ||
		cursor.TaskDeclaredID != "scheduled-maintenance" ||
		cursor.ScheduleID != "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, err := parseScheduleListQuery(
		request,
		"project-1",
		"environment-2",
	); err == nil {
		t.Fatal("cross-environment cursor succeeded")
	}
}

func TestScheduleListQueryRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, target := range []string{
		"/api/schedules?task=legacy",
		"/api/schedules?limit=0",
		"/api/schedules?limit=101",
		"/api/schedules?cursor=invalid",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, _, err := parseScheduleListQuery(
			request,
			"project-1",
			"environment-1",
		); err == nil {
			t.Fatalf("parseScheduleListQuery(%q) succeeded", target)
		}
	}
}
