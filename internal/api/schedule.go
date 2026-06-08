package api

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var scheduleIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type CreateScheduleRequest struct {
	ProjectID        string             `json:"project_id,omitempty"`
	EnvironmentID    string             `json:"environment_id,omitempty"`
	DeduplicationKey string             `json:"deduplication_key,omitempty"`
	ExternalID       string             `json:"external_id,omitempty"`
	Task             string             `json:"task"`
	Cron             string             `json:"cron"`
	Timezone         string             `json:"timezone,omitempty"`
	Options          ScheduleRunOptions `json:"options"`
	Active           *bool              `json:"active,omitempty"`
}

type ScheduleRunOptions struct {
	DeploymentID       string          `json:"deployment_id,omitempty"`
	Version            string          `json:"version,omitempty"`
	Queue              *RunQueueOption `json:"queue,omitempty"`
	ConcurrencyKey     string          `json:"concurrency_key,omitempty"`
	Priority           int32           `json:"priority,omitempty"`
	TTL                string          `json:"ttl,omitempty"`
	MaxDurationSeconds int32           `json:"max_duration_seconds,omitempty"`
}

func (o ScheduleRunOptions) CreateRunOptions() CreateRunOptions {
	return CreateRunOptions{
		DeploymentID:       o.DeploymentID,
		Version:            o.Version,
		Queue:              o.Queue,
		ConcurrencyKey:     o.ConcurrencyKey,
		Priority:           o.Priority,
		TTL:                o.TTL,
		MaxDurationSeconds: o.MaxDurationSeconds,
	}
}

type ScheduleResponse struct {
	ID               string     `json:"id"`
	Type             string     `json:"type"`
	ProjectID        string     `json:"project_id"`
	EnvironmentID    string     `json:"environment_id"`
	Task             string     `json:"task"`
	DeduplicationKey string     `json:"deduplication_key,omitempty"`
	ExternalID       string     `json:"external_id,omitempty"`
	Cron             string     `json:"cron"`
	Timezone         string     `json:"timezone"`
	Active           bool       `json:"active"`
	Status           string     `json:"status"`
	LastError        string     `json:"last_error,omitempty"`
	NextScheduledAt  *time.Time `json:"next_scheduled_at,omitempty"`
	LastScheduledAt  *time.Time `json:"last_scheduled_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type ListSchedulesResponse struct {
	Schedules []ScheduleResponse `json:"schedules"`
}

func ValidateScheduleID(id string) error {
	if !scheduleIDPattern.MatchString(id) {
		return fmt.Errorf("schedule id %q must match %s", id, scheduleIDPattern.String())
	}
	return nil
}

func NormalizeTimezone(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "UTC"
	}
	if strings.EqualFold(value, "utc") {
		return "UTC"
	}
	return value
}
