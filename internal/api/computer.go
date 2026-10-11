package api

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"time"

	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type CreateComputerRequest struct {
	DeleteAfterUnused lifetime.Policy           `json:"delete_after_unused_ms,omitzero"`
	Key               *string                   `json:"key,omitempty"`
	Secrets           []secretbinding.Reference `json:"secrets,omitempty"`
	IdempotencyKey    string                    `json:"idempotency_key,omitempty"`
}

type ComputerStatus string

const (
	ComputerStatusAvailable ComputerStatus = "available"
	ComputerStatusDeleted   ComputerStatus = "deleted"
	ComputerStatusDeleting  ComputerStatus = "deleting"
)

type ComputerResidency string

type ComputerSnapshot struct {
	DeleteAfterUnusedMs *int64                    `json:"delete_after_unused_ms"`
	UnusedSince         *time.Time                `json:"unused_since"`
	Residency           ComputerResidency         `json:"residency"`
	Error               json.RawMessage           `json:"error,omitempty"`
	ID                  string                    `json:"id"`
	Key                 *string                   `json:"key,omitempty"`
	DefinitionID        string                    `json:"definition_id"`
	DeploymentID        string                    `json:"deployment_id"`
	Status              ComputerStatus            `json:"status"`
	Secrets             []secretbinding.Reference `json:"secrets"`
	LastActivityAt      time.Time                 `json:"last_activity_at"`
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
}

type ComputerListItem struct {
	DeleteAfterUnusedMs *int64            `json:"delete_after_unused_ms"`
	UnusedSince         *time.Time        `json:"unused_since"`
	Residency           ComputerResidency `json:"residency"`
	Error               json.RawMessage   `json:"error,omitempty"`
	ID                  string            `json:"id"`
	Key                 *string           `json:"key,omitempty"`
	DefinitionID        string            `json:"definition_id"`
	DeploymentID        string            `json:"deployment_id"`
	Status              ComputerStatus    `json:"status"`
	LastActivityAt      time.Time         `json:"last_activity_at"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type ListComputersResponse struct {
	Computers  []ComputerListItem `json:"computers"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type ComputerMember struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
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

type UpdateComputerRequest struct {
	DeleteAfterUnused lifetime.Policy `json:"delete_after_unused_ms"`
}
