package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
	limit, cursor, taskID, err := parseScheduleListQuery(
		request,
		"project-1",
		"environment-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil || taskID != nil ||
		cursor.TaskDeclaredID != "scheduled-maintenance" ||
		cursor.ScheduleID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36" {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, _, err := parseScheduleListQuery(
		request,
		"project-1",
		"environment-2",
	); err == nil {
		t.Fatal("cross-environment cursor succeeded")
	}
}

func TestScheduleListQueryRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, target := range []string{
		"/v1/schedules?task=unexpected",
		"/v1/schedules?limit=0",
		"/v1/schedules?limit=101",
		"/v1/schedules?cursor=invalid",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, _, _, err := parseScheduleListQuery(
			request,
			"project-1",
			"environment-1",
		); err == nil {
			t.Fatalf("parseScheduleListQuery(%q) succeeded", target)
		}
	}
}

func TestScheduleListQueryAcceptsExactTaskLookup(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/schedules?task_id=scheduled-maintenance", nil)
	limit, cursor, taskID, err := parseScheduleListQuery(request, "project-1", "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 1 || cursor != nil || taskID == nil || *taskID != "scheduled-maintenance" {
		t.Fatalf("limit=%d cursor=%+v taskID=%v", limit, cursor, taskID)
	}
}

func TestScheduleResponseProjectsTimedDeclaration(t *testing.T) {
	now := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	scheduleID := uuid.NewV7()
	row := db.Schedule{
		ID:                   pgvalue.UUID(scheduleID),
		TaskDeclaredID:       "daily-report",
		CronPattern:          "0 9 * * *",
		Timezone:             "UTC",
		CronSemanticsVersion: "robfig-cron-v3.0.1/standard-5-field",
		Generation:           4,
		Status:               "errored",
		EffectiveFrom:        pgvalue.Timestamptz(now),
		NextFireAt:           pgvalue.Timestamptz(now.Add(time.Hour)),
		LastFailure:          []byte(`{"code":"future_schedule_failure","message":"diagnosis","details":{"custom":1}}`),
		CreatedAt:            pgvalue.Timestamptz(now.Add(-time.Hour)),
		UpdatedAt:            pgvalue.Timestamptz(now),
	}
	response, err := scheduleResponse(row)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != scheduleID.String() ||
		response.TaskID != row.TaskDeclaredID ||
		response.Status != api.ScheduleStatusErrored ||
		response.LastFailure == nil ||
		response.LastFailure.Code != "future_schedule_failure" ||
		response.LastFailure.Message != "diagnosis" ||
		string(response.LastFailure.Details) != `{"custom":1}` {
		t.Fatalf("response = %+v", response)
	}
}
