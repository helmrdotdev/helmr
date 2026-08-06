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

type ScheduleFailure struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

type ScheduleStatus string

const (
	ScheduleStatusActive   ScheduleStatus = "active"
	ScheduleStatusErrored  ScheduleStatus = "errored"
	ScheduleStatusArchived ScheduleStatus = "archived"
)

type ScheduleResponse struct {
	ID            string           `json:"id"`
	TaskID        string           `json:"task_id"`
	Cron          ScheduleCron     `json:"cron"`
	Status        ScheduleStatus   `json:"status"`
	Generation    int64            `json:"generation"`
	EffectiveFrom time.Time        `json:"effective_from"`
	NextFireAt    *time.Time       `json:"next_fire_at,omitempty"`
	LastFireAt    *time.Time       `json:"last_fire_at,omitempty"`
	LastFailure   *ScheduleFailure `json:"last_failure,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
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
