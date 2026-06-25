package api

import (
	"encoding/json"
	"time"
)

type StreamResponse struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Name      string          `json:"name"`
	Direction string          `json:"direction"`
	Sequence  int64           `json:"sequence"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type ListSessionStreamsResponse struct {
	Streams []StreamResponse `json:"streams"`
}

type AppendStreamRecordRequest struct {
	Data           json.RawMessage `json:"data"`
	ContentType    string          `json:"content_type,omitempty"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type StreamRecordResponse struct {
	ID            string          `json:"id"`
	StreamID      string          `json:"stream_id"`
	Sequence      int64           `json:"sequence"`
	Data          json.RawMessage `json:"data"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	ContentType   string          `json:"content_type"`
	CreatedAt     time.Time       `json:"created_at"`
}

type AppendStreamRecordResponse struct {
	Record            StreamRecordResponse `json:"record"`
	IdempotencyStatus string               `json:"idempotency_status"`
}

type ListStreamRecordsResponse struct {
	Records []StreamRecordResponse `json:"records"`
}

type ReadStreamRecordResponse struct {
	Record *StreamRecordResponse `json:"record"`
}

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

type PublicAccessTokenScopeRequest struct {
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	Stream        string `json:"stream"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type CreatePublicAccessTokenRequest struct {
	Scope     PublicAccessTokenScopeRequest `json:"scope"`
	ExpiresAt *time.Time                    `json:"expires_at,omitempty"`
	MaxUses   *int32                        `json:"max_uses,omitempty"`
}

type PublicAccessTokenScopeResponse struct {
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	Stream        string `json:"stream"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type PublicAccessTokenResponse struct {
	ID                string                         `json:"id"`
	PublicAccessToken string                         `json:"public_access_token,omitempty"`
	Scope             PublicAccessTokenScopeResponse `json:"scope"`
	ExpiresAt         time.Time                      `json:"expires_at"`
	MaxUses           *int32                         `json:"max_uses,omitempty"`
	CreatedAt         time.Time                      `json:"created_at"`
}
