package schedule

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const upcomingCount = 5

type ErrorCode string

const (
	ErrorUnsupportedCronVersion  ErrorCode = "unsupported_cron_version"
	ErrorInvalidSchedule         ErrorCode = "invalid_schedule"
	ErrorInvalidCron             ErrorCode = "invalid_cron"
	ErrorTaskNotFound            ErrorCode = "task_not_found"
	ErrorProgramUnavailable      ErrorCode = "program_unavailable"
	ErrorInvalidDefinition       ErrorCode = "invalid_definition"
	ErrorSecretSelectionMismatch ErrorCode = "secret_selection_mismatch"
	ErrorSandboxNotFound         ErrorCode = "sandbox_not_found"
)

type AdmissionError struct {
	Code    ErrorCode
	Message string
}

func (e *AdmissionError) Error() string {
	return e.Message
}

type Admission struct {
	Schedule    db.Schedule
	ScheduledAt time.Time
	NextFireAt  time.Time
	Payload     json.RawMessage
}

type scheduledTaskInput struct {
	ScheduledAt     string   `json:"scheduledAt"`
	LastScheduledAt string   `json:"lastScheduledAt,omitempty"`
	Timezone        string   `json:"timezone"`
	ScheduleID      string   `json:"scheduleId"`
	Upcoming        []string `json:"upcoming"`
}

func BuildAdmission(value db.Schedule) (Admission, error) {
	return BuildAdmissionAt(value, time.Now().UTC())
}

func BuildAdmissionAt(value db.Schedule, now time.Time) (Admission, error) {
	if value.CronSemanticsVersion != CronSemanticsVersion {
		return Admission{}, &AdmissionError{
			Code:    ErrorUnsupportedCronVersion,
			Message: fmt.Sprintf("unsupported cron semantics %q", value.CronSemanticsVersion),
		}
	}
	if !value.NextFireAt.Valid {
		return Admission{}, &AdmissionError{
			Code:    ErrorInvalidSchedule,
			Message: "schedule has no pending instant",
		}
	}
	scheduledAt := value.NextFireAt.Time.UTC()
	anchor := scheduledAt
	if now := now.UTC(); now.After(anchor) {
		anchor = now
	}
	upcoming, err := NextCronTimes(value.CronPattern, value.Timezone, anchor, upcomingCount)
	if err != nil {
		return Admission{}, &AdmissionError{
			Code:    ErrorInvalidCron,
			Message: err.Error(),
		}
	}
	nextFireAt := upcoming[0]
	encodedUpcoming := make([]string, 0, upcomingCount)
	for _, instant := range upcoming {
		encodedUpcoming = append(encodedUpcoming, instant.Format(time.RFC3339Nano))
	}
	input := scheduledTaskInput{
		ScheduledAt: scheduledAt.Format(time.RFC3339Nano),
		Timezone:    value.Timezone,
		ScheduleID:  pgvalue.UUIDString(value.ID),
		Upcoming:    encodedUpcoming,
	}
	if value.LastFireAt.Valid {
		input.LastScheduledAt = value.LastFireAt.Time.UTC().Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return Admission{}, &AdmissionError{
			Code:    ErrorInvalidSchedule,
			Message: fmt.Sprintf("encode scheduled Task input: %v", err),
		}
	}
	return Admission{
		Schedule:    value,
		ScheduledAt: scheduledAt,
		NextFireAt:  nextFireAt,
		Payload:     payload,
	}, nil
}
