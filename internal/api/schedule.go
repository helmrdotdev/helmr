package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
)

type ScheduleCron struct {
	Pattern  string `json:"pattern"`
	Timezone string `json:"timezone"`
}

type ScheduleResponse struct {
	ID           string          `json:"id"`
	AgentID      string          `json:"agent_id"`
	DeploymentID string          `json:"deployment_id"`
	TriggerKey   string          `json:"trigger_key"`
	Cron         ScheduleCron    `json:"cron"`
	Input        json.RawMessage `json:"input"`
	ActiveFrom   time.Time       `json:"active_from"`
	ActiveUntil  *time.Time      `json:"active_until,omitempty"`
	NextFireAt   *time.Time      `json:"next_fire_at,omitempty"`
}

type ListSchedulesResponse struct {
	Schedules  []ScheduleResponse `json:"schedules"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func ValidateScheduleID(id string) error {
	if err := ids.Validate(id); err != nil {
		return fmt.Errorf("invalid schedule ID: %w", err)
	}
	return nil
}
