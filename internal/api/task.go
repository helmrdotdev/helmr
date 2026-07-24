package api

import "encoding/json"

type StartTaskOptions struct {
	IdempotencyKey string                 `json:"idempotency_key,omitempty"`
	Workspace      WorkspaceTarget        `json:"workspace"`
	Queue          string                 `json:"queue,omitempty"`
	ConcurrencyKey *string                `json:"concurrency_key,omitempty"`
	Priority       int32                  `json:"priority,omitempty"`
	TTL            string                 `json:"ttl,omitempty"`
	Retry          *StartActorRetryPolicy `json:"retry,omitempty"`
	Metadata       json.RawMessage        `json:"metadata,omitempty"`
	Tags           []string               `json:"tags,omitempty"`
}

type StartTaskRequest struct {
	Payload json.RawMessage  `json:"payload,omitempty"`
	Options StartTaskOptions `json:"options"`
}

type StartTaskResponse struct {
	RunID string `json:"run_id"`
}
