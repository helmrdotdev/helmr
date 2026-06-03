package schedule

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
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
	got := Jitter(ids.New(), 30*time.Second)
	if got < 0 || got >= 30*time.Second {
		t.Fatalf("jitter outside window: %s", got)
	}
}

func TestRunRequestFromTriggerCandidateBuildsScheduledPayload(t *testing.T) {
	scheduleID := ids.New()
	instanceID := ids.New()
	projectID := ids.New()
	environmentID := ids.New()
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	lastScheduledAt := scheduledAt.Add(-24 * time.Hour)
	request, err := RunRequestFromTriggerCandidateAt(db.GetScheduleTriggerCandidateRow{
		ScheduleID:      ids.ToPG(scheduleID),
		InstanceID:      ids.ToPG(instanceID),
		ProjectID:       ids.ToPG(projectID),
		EnvironmentID:   ids.ToPG(environmentID),
		TaskID:          "daily-report",
		ExternalID:      pgtype.Text{String: "customer-1", Valid: true},
		Cron:            "0 9 * * *",
		Timezone:        "Asia/Tokyo",
		SecretBindings:  []byte(`{}`),
		RunOptions:      []byte(`{}`),
		Generation:      1,
		NextScheduledAt: pgtype.Timestamptz{Time: scheduledAt, Valid: true},
		LastScheduledAt: pgtype.Timestamptz{Time: lastScheduledAt, Valid: true},
	}, scheduledAt.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Timestamp     string   `json:"timestamp"`
		LastTimestamp string   `json:"lastTimestamp"`
		Timezone      string   `json:"timezone"`
		ScheduleID    string   `json:"scheduleId"`
		ExternalID    string   `json:"externalId"`
		Upcoming      []string `json:"upcoming"`
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Timestamp != "2026-06-02T00:00:00Z" || payload.LastTimestamp != "2026-06-01T00:00:00Z" {
		t.Fatalf("timestamps = %+v", payload)
	}
	if payload.Timezone != "Asia/Tokyo" || payload.ScheduleID != scheduleID.String() || payload.ExternalID != "customer-1" {
		t.Fatalf("identity = %+v", payload)
	}
	if len(payload.Upcoming) != 5 || payload.Upcoming[0] != "2026-06-03T00:00:00Z" {
		t.Fatalf("upcoming = %+v", payload.Upcoming)
	}
}

func TestRunRequestFromTriggerCandidateSkipsPastUpcomingSlots(t *testing.T) {
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	request, err := RunRequestFromTriggerCandidateAt(db.GetScheduleTriggerCandidateRow{
		ScheduleID:      ids.ToPG(ids.New()),
		InstanceID:      ids.ToPG(ids.New()),
		ProjectID:       ids.ToPG(ids.New()),
		EnvironmentID:   ids.ToPG(ids.New()),
		TaskID:          "daily-report",
		Cron:            "0 9 * * *",
		Timezone:        "Asia/Tokyo",
		SecretBindings:  []byte(`{}`),
		RunOptions:      []byte(`{}`),
		Generation:      1,
		NextScheduledAt: pgtype.Timestamptz{Time: scheduledAt, Valid: true},
	}, scheduledAt.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Upcoming []string `json:"upcoming"`
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Upcoming) != 5 || payload.Upcoming[0] != "2026-06-05T00:00:00Z" {
		t.Fatalf("upcoming = %+v", payload.Upcoming)
	}
}
