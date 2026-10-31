package api

import (
	"encoding/json"
	"time"
)

type AgentDefinition struct {
	RuntimeMcp   json.RawMessage `json:"runtime_mcp,omitempty"`
	InputKind    string          `json:"input_kind"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	TemplateID   string          `json:"template,omitempty"`
	ID           string          `json:"agent"`
	DeploymentID string          `json:"deployment_id"`
}

type AgentDefinitionListItem struct {
	RuntimeMcp  json.RawMessage `json:"runtime_mcp,omitempty"`
	InputKind   string          `json:"input_kind"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	TemplateID  string          `json:"template,omitempty"`
	ID          string          `json:"agent"`
}

type ListAgentDefinitionsResponse struct {
	DeploymentID     string                    `json:"deployment_id"`
	AgentDefinitions []AgentDefinitionListItem `json:"agents"`
	NextCursor       string                    `json:"next_cursor,omitempty"`
}

type CreateSessionRequest struct {
	Agent          string          `json:"agent"`
	Slack          json.RawMessage `json:"slack,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	ComputerID     string          `json:"computer_id,omitempty"`
	Key            *string         `json:"key,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type EnqueueSessionRequest struct {
	Input          json.RawMessage `json:"input,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// TurnAdmission is the durable receipt for one accepted Turn.
type TurnAdmission struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Sequence  int64  `json:"sequence"`
}

type CreateSessionResponse struct {
	SessionID string         `json:"session_id"`
	Turn      *TurnAdmission `json:"turn"`
}

// AgentSession is a retained conversation, independent of a physical process.
type AgentSession struct {
	RelatedVia           string                   `json:"related_via,omitempty"`
	Permissions          []string                 `json:"permissions,omitempty"`
	AgentDefinitionID    string                   `json:"agent"`
	TemplateDefinitionID string                   `json:"template,omitempty"`
	Activity             AgentSessionActivity     `json:"activity"`
	CloseAfterIdleMs     *int64                   `json:"close_after_idle_ms"`
	IdleSince            *time.Time               `json:"idle_since"`
	ID                   string                   `json:"id"`
	DeploymentID         string                   `json:"deployment_id"`
	ComputerID           *string                  `json:"computer_id"`
	RootSessionID        *string                  `json:"root_session_id"`
	SlackChannelID       *string                  `json:"slack_channel_id,omitempty"`
	ParentSessionID      *string                  `json:"parent_session_id"`
	RequesterSessionID   *string                  `json:"requester_session_id"`
	InitialTurn          *AgentSessionInitialTurn `json:"initial_turn"`
	Key                  *string                  `json:"key,omitempty"`
	Status               string                   `json:"status"`
	CreatedAt            time.Time                `json:"created_at"`
	Holds                []AgentSessionHold       `json:"holds"`
}
type AgentSessionInitialTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type AgentRecoveryGap struct {
	HoldID          *string    `json:"holdId"`
	ComputerID      string     `json:"computerId"`
	LeaseEpoch      int64      `json:"leaseEpoch"`
	RecoverySaveID  *string    `json:"recoverySaveId"`
	RestoredAt      *time.Time `json:"restoredAt"`
	AffectedTurnIDs []string   `json:"affectedTurnIds"`
}

// AgentSessionHold is a hold in effect for a Session. ID and SessionID are null
// for a hold on an ancestor the caller cannot read. CanRelease reports whether
// the caller may resume it by posting to SessionID.
type AgentSessionHold struct {
	RecoveryGap *AgentRecoveryGap `json:"recovery_gap,omitempty"`
	Kind        string            `json:"kind"`
	ID          *string           `json:"id"`
	SessionID   *string           `json:"session_id"`
	CreatedAt   time.Time         `json:"created_at"`
	CanRelease  bool              `json:"can_release"`
}
type AgentTurnError struct {
	Code    string  `json:"code"`
	Message *string `json:"message,omitempty"`
}
type AgentTurn struct {
	Stop             *AgentProcessStop `json:"stop,omitempty"`
	PayloadExpiredAt *time.Time        `json:"payload_expired_at,omitempty"`
	Response         json.RawMessage   `json:"response,omitempty"`
	Error            *AgentTurnError   `json:"error,omitempty"`
	ID               string            `json:"id"`
	SessionID        string            `json:"session_id"`
	Sequence         int64             `json:"sequence"`
	Status           string            `json:"status"`
	Input            json.RawMessage   `json:"input,omitempty"`
	Result           json.RawMessage   `json:"result,omitempty"`
	StartedAt        *time.Time        `json:"started_at,omitempty"`
	TerminalAt       *time.Time        `json:"terminal_at,omitempty"`
}
type AgentSessionsPage struct {
	Sessions   []AgentSession `json:"sessions"`
	NextCursor string         `json:"next_cursor,omitempty"`
}
type AgentTurnsPage struct {
	Turns      []AgentTurn `json:"turns"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

type AgentSessionEvent struct {
	SessionID string          `json:"session_id"`
	TurnID    *string         `json:"turn_id"`
	Sequence  int64           `json:"sequence"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}
type AgentSessionEventPage struct {
	Records       []AgentSessionEvent `json:"records"`
	NextAfter     int64               `json:"next_after"`
	HasMore       bool                `json:"has_more"`
	RetainedAfter int64               `json:"retained_after"`
}

type AgentSessionActivity struct {
	Process *AgentProcessActivity `json:"process"`
}
type AgentProcessActivity struct {
	Epoch                  int64             `json:"epoch"`
	Status                 string            `json:"status"`
	Applied                bool              `json:"applied"`
	ReconstructionRequired bool              `json:"reconstruction_required"`
	Stop                   *AgentProcessStop `json:"stop"`
}
type AgentProcessStop struct {
	ProcessEpoch int64     `json:"process_epoch"`
	TurnID       *string   `json:"turn_id"`
	State        string    `json:"state"`
	Deadline     time.Time `json:"deadline"`
}
