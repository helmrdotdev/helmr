package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var scheduleIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ScheduleWorkspace struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	SHA        string `json:"sha,omitempty"`
	Subpath    string `json:"subpath,omitempty"`
}

type CreateScheduleRequest struct {
	ProjectID        string            `json:"project_id,omitempty"`
	EnvironmentID    string            `json:"environment_id,omitempty"`
	DeduplicationKey string            `json:"deduplication_key,omitempty"`
	ExternalID       string            `json:"external_id,omitempty"`
	Task             string            `json:"task"`
	Cron             string            `json:"cron"`
	Timezone         string            `json:"timezone,omitempty"`
	SecretBindings   SecretBindings    `json:"secret_bindings,omitempty"`
	Workspace        ScheduleWorkspace `json:"workspace"`
	Options          CreateRunOptions  `json:"options,omitempty"`
	Active           *bool             `json:"active,omitempty"`
}

type ScheduleResponse struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	ProjectID        string          `json:"project_id"`
	EnvironmentID    string          `json:"environment_id"`
	Task             string          `json:"task"`
	DeduplicationKey string          `json:"deduplication_key"`
	ExternalID       string          `json:"external_id,omitempty"`
	Cron             string          `json:"cron"`
	Timezone         string          `json:"timezone"`
	Active           bool            `json:"active"`
	Status           string          `json:"status"`
	LastError        string          `json:"last_error,omitempty"`
	Workspace        json.RawMessage `json:"workspace,omitempty"`
	NextScheduledAt  *time.Time      `json:"next_scheduled_at,omitempty"`
	LastScheduledAt  *time.Time      `json:"last_scheduled_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
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
