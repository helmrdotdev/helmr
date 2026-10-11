package api

import (
	"encoding/json"
	"time"
)

type AgentDefinition struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
}

type ListAgentsResponse struct {
	DeploymentID string               `json:"deployment_id"`
	Agents       []DefinitionListItem `json:"agents"`
	NextCursor   string               `json:"next_cursor,omitempty"`
}

type StartAgentRequest struct {
	Slack          json.RawMessage `json:"slack,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	ComputerID     string          `json:"computer_id,omitempty"`
	SessionKey     *string         `json:"session_key,omitempty"`
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

type StartAgentResponse struct {
	TurnAdmission
	Created bool `json:"created"`
}

// AgentSession is a retained conversation, independent of a physical process.
type AgentSession struct {
	ID                 string                   `json:"id"`
	AgentID            string                   `json:"agent_id"`
	DeploymentID       string                   `json:"deployment_id"`
	ComputerID         string                   `json:"computer_id"`
	RootSessionID      string                   `json:"root_session_id"`
	SlackChannelID     *string                  `json:"slack_channel_id,omitempty"`
	ParentSessionID    *string                  `json:"parent_session_id"`
	RequesterSessionID *string                  `json:"requester_session_id"`
	InitialTurn        *AgentSessionInitialTurn `json:"initial_turn"`
	Key                *string                  `json:"key,omitempty"`
	Status             string                   `json:"status"`
	CreatedAt          time.Time                `json:"created_at"`
	Holds              []AgentSessionHold       `json:"holds"`
}
type AgentSessionInitialTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type AgentSessionHold struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Scope     string    `json:"scope"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}
type AgentTurnError struct {
	Code    string  `json:"code"`
	Message *string `json:"message,omitempty"`
}
type AgentTurn struct {
	PayloadExpiredAt *time.Time      `json:"payload_expired_at,omitempty"`
	Response         json.RawMessage `json:"response,omitempty"`
	Error            *AgentTurnError `json:"error,omitempty"`
	ID               string          `json:"id"`
	SessionID        string          `json:"session_id"`
	Sequence         int64           `json:"sequence"`
	Status           string          `json:"status"`
	Input            json.RawMessage `json:"input,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	TerminalAt       *time.Time      `json:"terminal_at,omitempty"`
	CompletionSaveID *string         `json:"completion_save_id,omitempty"`
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
