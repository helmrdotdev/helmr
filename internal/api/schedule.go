package api

import "time"

type ScheduleWorkspace struct {
	ID  string `json:"id,omitempty"`
	Key string `json:"key,omitempty"`
}

type ScheduleCron struct {
	Pattern  string `json:"pattern"`
	Timezone string `json:"timezone"`
}

type ScheduleError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ScheduleResponse struct {
	ID            string            `json:"id"`
	Task          string            `json:"task"`
	Workspace     ScheduleWorkspace `json:"workspace"`
	Cron          ScheduleCron      `json:"cron"`
	Status        string            `json:"status"`
	Generation    int64             `json:"generation"`
	EffectiveFrom time.Time         `json:"effective_from"`
	NextFireAt    *time.Time        `json:"next_fire_at,omitempty"`
	LastFireAt    *time.Time        `json:"last_fire_at,omitempty"`
	LastError     *ScheduleError    `json:"last_error,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type ListSchedulesResponse struct {
	Schedules []ScheduleResponse `json:"schedules"`
}
