package api

import (
	"encoding/json"
	"time"
)

type CreateTokenRequest struct {
	ProjectID      string          `json:"project_id,omitempty"`
	EnvironmentID  string          `json:"environment_id,omitempty"`
	Timeout        json.RawMessage `json:"timeout,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type TokenResponse struct {
	ID                string          `json:"id"`
	Status            string          `json:"status,omitempty"`
	CallbackURL       string          `json:"callback_url,omitempty"`
	PublicAccessToken string          `json:"public_access_token,omitempty"`
	TimeoutAt         *time.Time      `json:"timeout_at"`
	Data              json.RawMessage `json:"data,omitempty"`
	Tags              []string        `json:"tags,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
}

type ListTokensResponse struct {
	Tokens     []TokenResponse `json:"tokens"`
	NextCursor *string         `json:"next_cursor,omitempty"`
}

type CompleteTokenRequest struct {
	Data json.RawMessage `json:"data,omitempty"`
}

type CompleteTokenResponse struct {
	Status string        `json:"status"`
	Token  TokenResponse `json:"token"`
}
