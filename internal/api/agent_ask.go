package api

import (
	"encoding/json"
	"time"
)

// AgentAsk retains exact question identity even after its payload expires.
type AgentAsk struct {
	ID                 string          `json:"id"`
	SessionID          string          `json:"session_id"`
	TurnID             string          `json:"turn_id"`
	Status             string          `json:"status"`
	CreatedAt          time.Time       `json:"created_at"`
	Question           json.RawMessage `json:"question,omitempty"`
	Answer             json.RawMessage `json:"answer,omitempty"`
	PayloadExpired     bool            `json:"payload_expired,omitempty"`
	AnsweredAt         *time.Time      `json:"answered_at,omitempty"`
	CancelledAt        *time.Time      `json:"cancelled_at,omitempty"`
	AnsweredByUserID   *string         `json:"answered_by_user_id,omitempty"`
	AnsweredByAPIKeyID *string         `json:"answered_by_api_key_id,omitempty"`
}

type AgentAsksPage struct {
	Asks       []AgentAsk `json:"asks"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type AnswerAgentAskRequest struct {
	Answer   json.RawMessage `json:"answer"`
	AnswerID string          `json:"answer_id"`
}
