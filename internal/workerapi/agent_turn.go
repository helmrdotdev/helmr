package workerapi

import (
	"encoding/json"
	"time"
)

type AgentTurnRequest struct {
	Session             RuntimeSession `json:"session"`
	AttachmentSequence  int64          `json:"attachment_sequence"`
	AuthorityGeneration int64          `json:"authority_generation"`
}

type SlackInputSource struct {
	TeamID    string `json:"teamId"`
	ChannelID string `json:"channelId"`
	ThreadTS  string `json:"threadTs"`
	MessageTS string `json:"messageTs"`
	UserID    string `json:"userId"`
}

type AgentTurnSource struct {
	Slack              *SlackInputSource `json:"slack,omitempty"`
	Kind               string            `json:"kind"`
	RequesterSessionID string            `json:"requesterSessionId,omitempty"`
	OriginTurnID       string            `json:"originTurnId,omitempty"`
}

type AgentTurnDispatch struct {
	TurnID    string          `json:"turn_id"`
	Sequence  int64           `json:"sequence"`
	CreatedAt time.Time       `json:"created_at"`
	Input     json.RawMessage `json:"input"`
	Source    AgentTurnSource `json:"source"`
}

type AgentTurnReceipt struct {
	Session            RuntimeSession  `json:"session"`
	AttachmentSequence int64           `json:"attachment_sequence"`
	TurnID             string          `json:"turn_id"`
	Outcome            json.RawMessage `json:"outcome"`
}

type AgentTurnAcknowledgment struct {
	Sequence int64 `json:"sequence"`
}
