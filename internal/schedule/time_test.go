package schedule

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNextCronTimeUsesTimezone(t *testing.T) {
	next, err := NextCronTime("0 9 * * *", "Asia/Tokyo", time.Date(2026, 6, 1, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestRetryDelayCapsAtOneHour(t *testing.T) {
	if got := RetryDelay(2); got != 4*time.Minute {
		t.Fatalf("retry delay = %s", got)
	}
	if got := RetryDelay(100); got != time.Hour {
		t.Fatalf("capped retry delay = %s", got)
	}
}

func TestJitterStaysWithinWindow(t *testing.T) {
	got := Jitter(uuid.Must(uuid.NewV7()), 30*time.Second)
	if got < 0 || got >= 30*time.Second {
		t.Fatalf("jitter outside window: %s", got)
	}
}

func TestRunRequestFromTriggerCandidateBuildsScheduledPayload(t *testing.T) {
	scheduleID := uuid.Must(uuid.NewV7())
	instanceID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	lastFireAt := scheduledAt.Add(-24 * time.Hour)
	request, err := RunRequestFromTriggerCandidateAt(db.GetScheduleTriggerCandidateRow{
		ScheduleID:    pgvalue.UUID(scheduleID),
		InstanceID:    pgvalue.UUID(instanceID),
		ProjectID:     pgvalue.UUID(projectID),
		EnvironmentID: pgvalue.UUID(environmentID),
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "daily-report",
		ExternalID:    pgtype.Text{String: "customer-1", Valid: true},
		Cron:          "0 9 * * *",
		Timezone:      "Asia/Tokyo",
		RunOptions:    []byte(`{}`),
		Generation:    1,
		NextFireAt:    pgtype.Timestamptz{Time: scheduledAt, Valid: true},
		LastFireAt:    pgtype.Timestamptz{Time: lastFireAt, Valid: true},
	}, scheduledAt.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Timestamp     string   `json:"timestamp"`
		LastTimestamp string   `json:"lastTimestamp"`
		Timezone      string   `json:"timezone"`
		ScheduleID    string   `json:"scheduleId"`
		ScheduleType  string   `json:"scheduleType"`
		ExternalID    string   `json:"externalId"`
		Upcoming      []string `json:"upcoming"`
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Timestamp != "2026-06-02T00:00:00Z" || payload.LastTimestamp != "2026-06-01T00:00:00Z" {
		t.Fatalf("timestamps = %+v", payload)
	}
	if payload.Timezone != "Asia/Tokyo" || payload.ScheduleID != scheduleID.String() || payload.ScheduleType != "imperative" || payload.ExternalID != "customer-1" {
		t.Fatalf("identity = %+v", payload)
	}
	if len(payload.Upcoming) != 5 || payload.Upcoming[0] != "2026-06-03T00:00:00Z" {
		t.Fatalf("upcoming = %+v", payload.Upcoming)
	}
}

func TestRunRequestFromTriggerCandidateUpcomingIsStableAcrossRetries(t *testing.T) {
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	row := db.GetScheduleTriggerCandidateRow{
		ScheduleID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		InstanceID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ScheduleType:  db.TaskScheduleTypeDeclarative,
		TaskID:        "daily-report",
		Cron:          "0 9 * * *",
		Timezone:      "Asia/Tokyo",
		RunOptions:    []byte(`{}`),
		Generation:    1,
		NextFireAt:    pgtype.Timestamptz{Time: scheduledAt, Valid: true},
	}
	first, err := RunRequestFromTriggerCandidateAt(row, scheduledAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := RunRequestFromTriggerCandidateAt(row, scheduledAt.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Payload) != string(retry.Payload) {
		t.Fatalf("payload changed across retry:\nfirst=%s\nretry=%s", first.Payload, retry.Payload)
	}
	var payload struct {
		Upcoming []string `json:"upcoming"`
	}
	if err := json.Unmarshal(retry.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Upcoming) != 5 || payload.Upcoming[0] != "2026-06-03T00:00:00Z" {
		t.Fatalf("upcoming = %+v", payload.Upcoming)
	}
}
