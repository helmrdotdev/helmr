package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// SessionDataRequest contains input for one exact Turn.
// The endpoint, never application data, determines routing.
type SessionDataRequest struct {
	Data           json.RawMessage `json:"data"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SessionMessageReceipt struct {
	ID        string `json:"id"`
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type CloseSessionRequest struct {
	ExpectedScope  string `json:"expected_scope,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type CancelSessionRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type InterruptSessionRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type SessionInterruptReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	HoldID    string `json:"hold_id"`
	Status    string `json:"status"`
}

type ResumeSessionRequest struct {
	HoldID         string `json:"hold_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type SessionCloseReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}
type SessionCancelReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}
type SessionResumeReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	HoldID    string `json:"hold_id"`
	Status    string `json:"status"`
}

func ValidateSessionDataRequest(request SessionDataRequest) error {
	if len(request.Data) == 0 {
		return errors.New("data is required")
	}
	if _, err := jsoncanon.Transform(request.Data); err != nil {
		return fmt.Errorf("data must be unambiguous I-JSON: %w", err)
	}
	return nil
}

type InterruptTurnRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type TurnInterruptReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Status    string `json:"status"`
}
