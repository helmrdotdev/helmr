package api

import (
	"encoding/json"
	"time"
)

// AgentAsk retains exact question identity even after its payload expires.
type AgentAsk struct {
	ID                  string          `json:"id"`
	SessionID           string          `json:"session_id"`
	TurnID              string          `json:"turn_id"`
	Status              string          `json:"status"`
	CreatedAt           time.Time       `json:"created_at"`
	Prompt              json.RawMessage `json:"prompt,omitempty"`
	AnswerControl       json.RawMessage `json:"answer_control,omitempty"`
	Answer              json.RawMessage `json:"answer,omitempty"`
	PayloadExpired      bool            `json:"payload_expired,omitempty"`
	RespondedAt         *time.Time      `json:"responded_at,omitempty"`
	CancelledAt         *time.Time      `json:"cancelled_at,omitempty"`
	RespondedByUserID   *string         `json:"responded_by_user_id,omitempty"`
	RespondedByAPIKeyID *string         `json:"responded_by_api_key_id,omitempty"`
}

type AgentAsksPage struct {
	Asks       []AgentAsk `json:"asks"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type RespondAgentAskRequest struct {
	Answer     json.RawMessage `json:"answer"`
	ResponseID string          `json:"response_id"`
}
