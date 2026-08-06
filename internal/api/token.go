package api

import (
	"encoding/json"
	"time"
)

type CreateTokenRequest struct {
	Timeout        string          `json:"timeout,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type TokenStatus string

const (
	TokenStatusPending   TokenStatus = "pending"
	TokenStatusCompleted TokenStatus = "completed"
	TokenStatusExpired   TokenStatus = "expired"
	TokenStatusCancelled TokenStatus = "cancelled"
)

type TokenResponse struct {
	ID                string          `json:"id"`
	Status            TokenStatus     `json:"status"`
	CallbackURL       string          `json:"callback_url,omitempty"`
	PublicAccessToken string          `json:"public_access_token,omitempty"`
	TimeoutAt         time.Time       `json:"timeout_at"`
	Result            json.RawMessage `json:"result,omitempty"`
	Tags              []string        `json:"tags"`
	Metadata          json.RawMessage `json:"metadata"`
	CompletedAt       *time.Time      `json:"completed_at,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type TokenListItem struct {
	ID          string      `json:"id"`
	Status      TokenStatus `json:"status"`
	Tags        []string    `json:"tags"`
	TimeoutAt   time.Time   `json:"timeout_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type ListTokensResponse struct {
	Tokens     []TokenListItem `json:"tokens"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type CompleteTokenRequest struct {
	Result         json.RawMessage `json:"result"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type CancelTokenRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
