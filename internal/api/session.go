package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// SessionDataRequest is the envelope for automatic send, enqueue and exact messages.
// The endpoint, never application data, determines routing.
type SessionDataRequest struct {
	Data           json.RawMessage `json:"data"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SessionAdmissionReceipt struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	TurnID    string  `json:"turn_id"`
	MessageID *string `json:"message_id,omitempty"`
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
type CancelSessionRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type InterruptTurnRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
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
type TurnInterruptReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	HoldID    string `json:"hold_id"`
	Status    string `json:"status"`
}
type SessionResumeReceipt struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	HoldID    string `json:"hold_id"`
	Status    string `json:"status"`
}

type CancelRunRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ActorRunCancellationReceipt acknowledges a stop outside an active Turn.
// Task cancellation continues to return its ordinary RunSnapshotResponse.
type ActorRunCancellationReceipt struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	HoldID    string `json:"hold_id"`
	Status    string `json:"status"`
}

type SessionStatus string

const (
	SessionStatusOpen    SessionStatus = "open"
	SessionStatusClosing SessionStatus = "closing"
	SessionStatusClosed  SessionStatus = "closed"
	SessionStatusFailed  SessionStatus = "failed"
)

type SessionFailure struct {
	Code    string                `json:"code"`
	Message string                `json:"message"`
	Details SessionFailureDetails `json:"details"`
}
type SessionFailureDetails struct {
	RunID string `json:"run_id,omitempty"`
}

type SessionDispatch struct {
	State  string  `json:"state"`
	HoldID *string `json:"hold_id,omitempty"`
	Reason *string `json:"reason,omitempty"`
}
type Session struct {
	CancelRequestedAt *time.Time      `json:"cancel_requested_at,omitempty"`
	ID                string          `json:"id"`
	ActorID           string          `json:"actor_id"`
	DeploymentID      string          `json:"deployment_id"`
	WorkspaceID       string          `json:"workspace_id"`
	Key               *string         `json:"key,omitempty"`
	Status            SessionStatus   `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	CurrentRunID      *string         `json:"current_run_id"`
	ActiveTurnID      *string         `json:"active_turn_id"`
	Dispatch          SessionDispatch `json:"dispatch"`
	Failure           *SessionFailure `json:"failure,omitempty"`
}
type ListSessionsResponse struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type SessionTurnSource struct {
	Type  string `json:"type"`
	RunID string `json:"run_id,omitempty"`
}
type SessionTurn struct {
	ID                 string            `json:"id"`
	SessionID          string            `json:"session_id"`
	Sequence           int64             `json:"sequence"`
	Input              json.RawMessage   `json:"input"`
	Source             SessionTurnSource `json:"source"`
	Status             string            `json:"status"`
	CreatedAt          time.Time         `json:"created_at"`
	InterruptRequested bool              `json:"interrupt_requested"`
	AcceptsMessages    bool              `json:"accepts_messages"`
	TerminalEventID    *string           `json:"terminal_event_id,omitempty"`
	WorkspaceVersionID *string           `json:"workspace_version_id,omitempty"`
	Result             json.RawMessage   `json:"result,omitempty"`
	Error              json.RawMessage   `json:"error,omitempty"`
}
type SessionEventProvenance struct {
	RunID         string `json:"run_id"`
	AttemptNumber int32  `json:"attempt_number"`
	RunGeneration int64  `json:"run_generation"`
	DeploymentID  string `json:"deployment_id"`
}
type SessionEvent struct {
	ID         string                  `json:"id"`
	SessionID  string                  `json:"session_id"`
	TurnID     *string                 `json:"turn_id"`
	Sequence   int64                   `json:"sequence"`
	CreatedAt  time.Time               `json:"created_at"`
	Kind       string                  `json:"kind"`
	Data       json.RawMessage         `json:"data"`
	Provenance *SessionEventProvenance `json:"provenance"`
}
type SessionEventPage struct {
	Records       []SessionEvent `json:"records"`
	NextAfter     int64          `json:"next_after"`
	HasMore       bool           `json:"has_more"`
	RetainedAfter int64          `json:"retained_after"`
}

func ValidateSessionStatus(status string) error {
	switch SessionStatus(status) {
	case SessionStatusOpen, SessionStatusClosing, SessionStatusClosed, SessionStatusFailed:
		return nil
	default:
		return fmt.Errorf("invalid session status %q", status)
	}
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
