package api

import (
	"encoding/json"
	"time"

	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type CreateComputerRequest struct {
	Key            *string                       `json:"key,omitempty"`
	Secrets        []secretbinding.Binding `json:"secrets,omitempty"`
	IdempotencyKey string                        `json:"idempotency_key,omitempty"`
}

type ComputerStatus string

const (
	ComputerStatusAvailable ComputerStatus = "available"
	ComputerStatusDeleted   ComputerStatus = "deleted"
	ComputerStatusDeleting  ComputerStatus = "deleting"
)

type ComputerResidency string

type ComputerSnapshot struct {
	Residency      ComputerResidency             `json:"residency"`
	Error          json.RawMessage               `json:"error,omitempty"`
	ID             string                        `json:"id"`
	Key            *string                       `json:"key,omitempty"`
	SandboxID      string                        `json:"sandbox_id"`
	DeploymentID   string                        `json:"deployment_id"`
	Status         ComputerStatus                `json:"status"`
	Secrets        []secretbinding.Binding `json:"secrets"`
	LastActivityAt time.Time                     `json:"last_activity_at"`
	CreatedAt      time.Time                     `json:"created_at"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

type ComputerListItem struct {
	Residency      ComputerResidency `json:"residency"`
	Error          json.RawMessage   `json:"error,omitempty"`
	ID             string            `json:"id"`
	Key            *string           `json:"key,omitempty"`
	SandboxID      string            `json:"sandbox_id"`
	DeploymentID   string            `json:"deployment_id"`
	Status         ComputerStatus    `json:"status"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ListComputersResponse struct {
	Computers  []ComputerListItem `json:"computers"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type ComputerMember struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	RunID     string    `json:"run_id,omitempty"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

type ComputerMembersQuery struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int32  `json:"limit,omitempty"`
}

type ListComputerMembersResponse struct {
	Members    []ComputerMember `json:"members"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type DeleteComputerRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type DeleteComputerReceipt struct {
	ComputerID string `json:"computer_id"`
}
