package api

import (
	"encoding/json"
	"time"
)

type CreateWaitpointRequest struct {
	ProjectID                string          `json:"project_id,omitempty"`
	EnvironmentID            string          `json:"environment_id,omitempty"`
	Request                  json.RawMessage `json:"request,omitempty"`
	DisplayText              string          `json:"display_text,omitempty"`
	ExpiresAt                time.Time       `json:"expires_at"`
	IdempotencyKey           string          `json:"idempotency_key,omitempty"`
	IdempotencyKeyExpiresAt  *time.Time      `json:"idempotency_key_expires_at,omitempty"`
	IdempotencyKeyTTLSeconds *int32          `json:"idempotency_key_ttl_seconds,omitempty"`
}

type WaitpointResponse struct {
	ID            string          `json:"id"`
	ProjectID     string          `json:"project_id"`
	EnvironmentID string          `json:"environment_id"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	Request       json.RawMessage `json:"request"`
	DisplayText   string          `json:"display_text"`
	ExpiresAt     *time.Time      `json:"expires_at"`
	CreatedAt     time.Time       `json:"created_at"`
}
