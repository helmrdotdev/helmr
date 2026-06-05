package api

import "time"

type PromoteRuntimeReleaseRequest struct {
	RuntimeID string `json:"runtime_id"`
}

type RuntimeReleaseResponse struct {
	RuntimeID  string    `json:"runtime_id"`
	SelectedAt time.Time `json:"selected_at"`
}
