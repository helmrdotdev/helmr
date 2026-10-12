package api

import (
	"encoding/json"
	"time"
)

// DeploymentEvent is one retained Deployment lifecycle event.
type DeploymentEvent struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Message    string          `json:"message"`
	At         time.Time       `json:"at"`
	Attributes json.RawMessage `json:"attributes"`
}

type DeploymentEventPage struct {
	Events     []DeploymentEvent `json:"events"`
	NextCursor *string           `json:"next_cursor,omitempty"`
}
