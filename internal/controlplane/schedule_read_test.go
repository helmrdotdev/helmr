package controlplane

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
		ScheduleID:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/schedules?limit=25&cursor="+raw,
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
		cursor.ScheduleID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36" {
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
		"/v1/schedules?task=legacy",
		"/v1/schedules?limit=0",
		"/v1/schedules?limit=101",
		"/v1/schedules?cursor=invalid",
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
