package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
)

type TriggerCron struct {
	Pattern  string `json:"pattern"`
	Timezone string `json:"timezone"`
}

type TriggerResponse struct {
	Agent        string          `json:"agent"`
	ID           string          `json:"id"`
	DeploymentID string          `json:"deployment_id"`
	TriggerKey   string          `json:"trigger_key"`
	Cron         TriggerCron     `json:"cron"`
	Input        json.RawMessage `json:"input"`
	ActiveFrom   time.Time       `json:"active_from"`
	ActiveUntil  *time.Time      `json:"active_until,omitempty"`
	NextFireAt   *time.Time      `json:"next_fire_at,omitempty"`
}

type ListTriggersResponse struct {
	Triggers   []TriggerResponse `json:"triggers"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func ValidateTriggerID(id string) error {
	if err := ids.Validate(id); err != nil {
		return fmt.Errorf("invalid trigger ID: %w", err)
	}
	return nil
}
