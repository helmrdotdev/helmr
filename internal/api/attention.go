package api

import "time"

// Kinds of AttentionItem.
const (
	AttentionQuestion             = "question"
	AttentionHold                 = "hold"
	AttentionFailedTurn           = "failed_turn"
	AttentionSlackReauthorization = "slack_reauthorization"
)

// AttentionItem is one thing in an Environment waiting on a person. ID names
// the Question, hold, Turn or Slack connection; At is when it started waiting.
type AttentionItem struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Agent     string    `json:"agent"`
	SessionID *string   `json:"session_id,omitempty"`
	TurnID    *string   `json:"turn_id,omitempty"`
	// A Question's first text part, shortened to one line.
	Prompt *string `json:"prompt,omitempty"`
	// A hold's reason.
	Reason *string `json:"reason,omitempty"`
	// A failed Turn's sequence in its Session and error code.
	TurnSequence *int64  `json:"turn_sequence,omitempty"`
	ErrorCode    *string `json:"error_code,omitempty"`
	// The Slack workspace of a connection that lost its authorization.
	WorkspaceName *string `json:"workspace_name,omitempty"`
}

// AttentionCounts counts every waiting item of each kind, including items past
// the returned page. SlackReauthorization is present only for callers who
// manage Slack connections.
type AttentionCounts struct {
	Question             *int64 `json:"question,omitempty"`
	Hold                 *int64 `json:"hold,omitempty"`
	FailedTurn           *int64 `json:"failed_turn,omitempty"`
	SlackReauthorization *int64 `json:"slack_reauthorization,omitempty"`
}

// Attention lists what is waiting on a person in one Environment, oldest
// first across kinds, with at most a bounded number of items of each kind.
type Attention struct {
	Items  []AttentionItem `json:"items"`
	Counts AttentionCounts `json:"counts"`
}
