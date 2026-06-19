package api

import (
	"encoding/json"
	"time"
)

type CreateWaitpointTokenRequest struct {
	TimeoutAt        *time.Time      `json:"timeout_at,omitempty"`
	TimeoutInSeconds *int32          `json:"timeout_in_seconds,omitempty"`
	Tags             []string        `json:"tags,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
}

type WaitpointTokenResponse struct {
	ID                string          `json:"id"`
	Status            string          `json:"status,omitempty"`
	CallbackURL       string          `json:"callback_url"`
	PublicAccessToken string          `json:"public_access_token,omitempty"`
	TimeoutAt         *time.Time      `json:"timeout_at"`
	Data              json.RawMessage `json:"data,omitempty"`
	Tags              []string        `json:"tags,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
}

type ListWaitpointTokensResponse struct {
	Tokens     []WaitpointTokenResponse `json:"tokens"`
	NextCursor *string                  `json:"next_cursor,omitempty"`
}

type CompleteWaitpointTokenRequest struct {
	Data json.RawMessage `json:"data,omitempty"`
}
