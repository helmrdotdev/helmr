package api

import (
	"encoding/json"
)

type Task struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
}

type ListTasksResponse struct {
	DeploymentID string `json:"deployment_id"`
	Tasks        []Task `json:"tasks"`
	NextCursor   string `json:"next_cursor,omitempty"`
}

type Actor struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
}

type ListActorsResponse struct {
	DeploymentID string  `json:"deployment_id"`
	Actors       []Actor `json:"actors"`
	NextCursor   string  `json:"next_cursor,omitempty"`
}

type Sandbox struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
}

type ListSandboxesResponse struct {
	DeploymentID string    `json:"deployment_id"`
	Sandboxes    []Sandbox `json:"sandboxes"`
	NextCursor   string    `json:"next_cursor,omitempty"`
}

type StartTaskRequest struct {
	Payload        json.RawMessage        `json:"payload,omitempty"`
	IdempotencyKey string                 `json:"idempotency_key,omitempty"`
	Workspace      WorkspaceIDTarget      `json:"workspace"`
	Queue          string                 `json:"queue,omitempty"`
	ConcurrencyKey *string                `json:"concurrency_key,omitempty"`
	Priority       int32                  `json:"priority,omitempty"`
	TTL            string                 `json:"ttl,omitempty"`
	Retry          *StartActorRetryPolicy `json:"retry,omitempty"`
	Metadata       json.RawMessage        `json:"metadata,omitempty"`
	Tags           []string               `json:"tags,omitempty"`
}

type StartTaskResponse struct {
	RunID string `json:"run_id"`
}
