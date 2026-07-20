package schedule

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const upcomingCount = 5

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
	ScheduleType    string   `json:"scheduleType"`
	Key             string   `json:"key,omitempty"`
	Upcoming        []string `json:"upcoming"`
}

func BuildAdmission(value db.Schedule) (Admission, error) {
	return BuildAdmissionAt(value, time.Now().UTC())
}

func BuildAdmissionAt(value db.Schedule, now time.Time) (Admission, error) {
	if value.CronContractVersion != CronContractVersion {
		return Admission{}, &AdmissionError{
			Code:    ErrorGenerationInvalid,
			Message: fmt.Sprintf("unsupported cron contract %q", value.CronContractVersion),
		}
	}
	if !value.NextFireAt.Valid {
		return Admission{}, &AdmissionError{
			Code:    ErrorGenerationInvalid,
			Message: "schedule has no pending instant",
		}
	}
	if value.Source != "declarative" && value.Source != "imperative" {
		return Admission{}, &AdmissionError{
			Code:    ErrorGenerationInvalid,
			Message: fmt.Sprintf("unsupported schedule source %q", value.Source),
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
			Code:    ErrorInputInvalid,
			Message: err.Error(),
		}
	}
	nextFireAt := upcoming[0]
	encodedUpcoming := make([]string, 0, upcomingCount)
	for _, instant := range upcoming {
		encodedUpcoming = append(encodedUpcoming, instant.Format(time.RFC3339Nano))
	}
	input := scheduledTaskInput{
		ScheduledAt:  scheduledAt.Format(time.RFC3339Nano),
		Timezone:     value.Timezone,
		ScheduleID:   value.PublicID,
		ScheduleType: value.Source,
		Key:          value.Key,
		Upcoming:     encodedUpcoming,
	}
	if value.LastFireAt.Valid {
		input.LastScheduledAt = value.LastFireAt.Time.UTC().Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return Admission{}, &AdmissionError{
			Code:    ErrorInputInvalid,
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
