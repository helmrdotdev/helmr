package api

import (
	"encoding/json"
	"time"
)

type DiagnosticEvent struct {
	ID             string          `json:"id"`
	DeploymentID   *string         `json:"deployment_id,omitempty"`
	Trace          TraceContext    `json:"trace"`
	Category       string          `json:"category"`
	Severity       string          `json:"severity"`
	Source         string          `json:"source"`
	Kind           string          `json:"kind"`
	Message        string          `json:"message"`
	At             time.Time       `json:"at"`
	OccurredAt     time.Time       `json:"occurred_at"`
	RedactionClass string          `json:"redaction_class"`
	Attributes     json.RawMessage `json:"attributes"`
}

type DiagnosticEventPage struct {
	Events     []DiagnosticEvent `json:"events"`
	NextCursor *string           `json:"next_cursor,omitempty"`
}
