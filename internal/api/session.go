package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// SessionMessageRequest contains a message for one exact Turn.
// The endpoint, never application data, determines routing.
type SessionMessageRequest struct {
	Message        json.RawMessage `json:"message"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SessionMessageReceipt struct {
	ID        string `json:"id"`
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type CloseSessionRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// SessionClosePreview counts the Sessions a close would close: the Session and
// the Sessions it owns that are not closed yet.
type SessionClosePreview struct {
	Sessions int64 `json:"sessions"`
}

// ComputerHibernatePreview counts the open Sessions a hibernate request would
// pause.
type ComputerHibernatePreview struct {
	Sessions int64 `json:"sessions"`
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

// SessionCloseReceipt reports the Session status after a close: closing while
// accepted work drains, or closed.
type SessionCloseReceipt struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

// SessionCancelReceipt reports the Session status after a cancel, which is
// always closed.
type SessionCancelReceipt struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}
type SessionResumeReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	HoldID    string `json:"hold_id"`
	Status    string `json:"status"`
}

func ValidateSessionMessageRequest(request SessionMessageRequest) error {
	if len(request.Message) == 0 {
		return errors.New("message is required")
	}
	if _, err := jsoncanon.Transform(request.Message); err != nil {
		return fmt.Errorf("message must be unambiguous I-JSON: %w", err)
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
