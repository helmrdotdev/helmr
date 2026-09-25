package session

import (
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

// Target is an environment-scoped address, not an authorization capability.
type Target struct {
	EnvironmentID uuid.UUID
	SessionID     uuid.UUID
}

type OperationError struct {
	Code          string
	RetainedAfter int64
}

func (e *OperationError) Error() string { return e.Code }

type AdmissionMode string

const (
	SendMessageOrEnqueue AdmissionMode = "send"
	EnqueueOnly          AdmissionMode = "enqueue"
	ExactMessage         AdmissionMode = "message"
)

type AdmissionRequest struct {
	Target
	Mode           AdmissionMode
	TurnID         uuid.UUID
	SourceRunID    uuid.UUID
	Data           json.RawMessage
	IdempotencyKey string
}

// Code is a durable rejection. Callers commit it, then map it to transport error.
type AdmissionReceipt struct {
	ID        uuid.UUID  `json:"id"`
	Kind      string     `json:"kind,omitempty"`
	TurnID    uuid.UUID  `json:"turn_id"`
	MessageID *uuid.UUID `json:"message_id,omitempty"`
	Code      string     `json:"code,omitempty"`
}

type ControlRequest struct {
	Target
	IdempotencyKey string
}

type ResumeRequest struct {
	ControlRequest
	HoldID uuid.UUID
}

type InterruptRequest struct {
	ControlRequest
	TurnID uuid.UUID
}

type ControlReceipt struct {
	ID        uuid.UUID  `json:"id"`
	SessionID uuid.UUID  `json:"session_id"`
	TurnID    *uuid.UUID `json:"turn_id"`
	HoldID    *uuid.UUID `json:"hold_id,omitempty"`
	Status    string     `json:"status"`
	Code      string     `json:"code,omitempty"`
}

type EventPage struct {
	Records       []db.ListSessionEventsRow
	NextAfter     int64
	HasMore       bool
	RetainedAfter int64
}

type TurnView struct {
	Turn            db.SessionTurn
	AcceptsMessages bool
	TerminalEvent   *db.SessionEvent
}
