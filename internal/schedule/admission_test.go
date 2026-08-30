package schedule

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestBuildAdmissionProducesStablePlatformInput(t *testing.T) {
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	lastScheduledAt := scheduledAt.Add(-24 * time.Hour)
	value := db.Schedule{
		ID:                   pgvalue.UUID(uuid.NewV7()),
		CronPattern:          "0 9 * * *",
		Timezone:             "Asia/Tokyo",
		CronSemanticsVersion: CronSemanticsVersion,
		NextFireAt:           pgvalue.TimestamptzUTCZeroInvalid(scheduledAt),
		LastFireAt:           pgvalue.TimestamptzUTCZeroInvalid(lastScheduledAt),
	}
	admission, err := BuildAdmissionAt(value, scheduledAt)
	if err != nil {
		t.Fatal(err)
	}
	if want := scheduledAt.Add(24 * time.Hour); !admission.NextFireAt.Equal(want) {
		t.Fatalf("next fire = %s, want %s", admission.NextFireAt, want)
	}
	var payload scheduledTaskInput
	if err := json.Unmarshal(admission.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ScheduledAt != "2026-06-02T00:00:00Z" ||
		payload.LastScheduledAt != "2026-06-01T00:00:00Z" ||
		payload.Timezone != "Asia/Tokyo" ||
		payload.ScheduleID != pgvalue.UUIDString(value.ID) {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Upcoming) != upcomingCount || payload.Upcoming[0] != "2026-06-03T00:00:00Z" {
		t.Fatalf("upcoming = %+v", payload.Upcoming)
	}
}

func TestBuildAdmissionSkipsMissedInstants(t *testing.T) {
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	admission, err := BuildAdmissionAt(db.Schedule{
		ID:                   pgvalue.UUID(uuid.NewV7()),
		CronPattern:          "0 9 * * *",
		Timezone:             "Asia/Tokyo",
		CronSemanticsVersion: CronSemanticsVersion,
		NextFireAt:           pgvalue.TimestamptzUTCZeroInvalid(scheduledAt),
	}, scheduledAt.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC); !admission.NextFireAt.Equal(want) {
		t.Fatalf("next fire = %s, want %s", admission.NextFireAt, want)
	}
	var payload scheduledTaskInput
	if err := json.Unmarshal(admission.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ScheduledAt != scheduledAt.Format(time.RFC3339Nano) {
		t.Fatalf("scheduledAt = %s", payload.ScheduledAt)
	}
	if payload.Upcoming[0] != admission.NextFireAt.Format(time.RFC3339Nano) {
		t.Fatalf("upcoming = %+v", payload.Upcoming)
	}
}

func TestBuildAdmissionRejectsUnknownSemantics(t *testing.T) {
	_, err := BuildAdmission(db.Schedule{
		CronSemanticsVersion: "other",
		NextFireAt:           pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err == nil {
		t.Fatal("unknown cron contract was accepted")
	}
}
