package workerapi

import "encoding/json"

type AgentMessageRequest struct {
	Session             RuntimeSession `json:"session"`
	AttachmentSequence  int64          `json:"attachment_sequence"`
	AuthorityGeneration int64          `json:"authority_generation"`
	TurnID              string         `json:"turn_id"`
}
type AgentMessageDispatch struct {
	TurnID    string          `json:"turn_id"`
	MessageID string          `json:"message_id"`
	Input     json.RawMessage `json:"input"`
}
type AgentMessageReceipt struct {
	Session            RuntimeSession `json:"session"`
	AttachmentSequence int64          `json:"attachment_sequence"`
	TurnID             string         `json:"turn_id"`
	MessageID          string         `json:"message_id"`
	RejectionReason    string         `json:"rejection_reason,omitempty"`
}
