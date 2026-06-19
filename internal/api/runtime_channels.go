package api

import (
	"encoding/json"
	"time"
)

type ListRunWaitpointsResponse struct {
	Waitpoints []PendingWaitpoint `json:"waitpoints"`
	NextCursor *string            `json:"next_cursor,omitempty"`
}

type AppendChannelRecordRequest struct {
	Data            json.RawMessage `json:"data"`
	CorrelationID   string          `json:"correlation_id,omitempty"`
	ExternalEventID string          `json:"external_event_id,omitempty"`
}

type ChannelRecordResponse struct {
	ID            string          `json:"id"`
	ChannelID     string          `json:"channel_id"`
	Sequence      int64           `json:"sequence"`
	Data          json.RawMessage `json:"data"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	ContentType   string          `json:"content_type"`
	CreatedAt     time.Time       `json:"created_at"`
}

type AppendChannelRecordResponse struct {
	Record            ChannelRecordResponse `json:"record"`
	IdempotencyStatus string                `json:"idempotency_status"`
}

type ListChannelRecordsResponse struct {
	Records []ChannelRecordResponse `json:"records"`
}

type PublicAccessTokenScopeRequest struct {
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	Channel       string `json:"channel,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type CreatePublicAccessTokenRequest struct {
	Scope     PublicAccessTokenScopeRequest `json:"scope"`
	ExpiresAt *time.Time                    `json:"expires_at,omitempty"`
	MaxUses   *int32                        `json:"max_uses,omitempty"`
}

type PublicAccessTokenResponse struct {
	ID                string                        `json:"id"`
	PublicAccessToken string                        `json:"public_access_token"`
	Scope             PublicAccessTokenScopeRequest `json:"scope"`
	ExpiresAt         time.Time                     `json:"expires_at"`
	MaxUses           *int32                        `json:"max_uses,omitempty"`
	CreatedAt         time.Time                     `json:"created_at"`
}
