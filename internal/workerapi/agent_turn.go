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

type AgentTurnSource struct {
	Kind               string `json:"kind"`
	RequesterSessionID string `json:"requesterSessionId,omitempty"`
	OriginTurnID       string `json:"originTurnId,omitempty"`
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
