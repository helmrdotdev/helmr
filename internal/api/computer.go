package api

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"time"

	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type CreateComputerRequest struct {
	Template          string                    `json:"template"`
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

type ComputerExecutionWait struct {
	Reason string `json:"reason"`
}

type ComputerExecutionControl struct {
	ID         string     `json:"id"`
	ComputerID string     `json:"computer_id"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	ErrorCode  *string    `json:"error_code,omitempty"`
	AcceptedAt time.Time  `json:"accepted_at"`
	SettledAt  *time.Time `json:"settled_at"`
}

type ComputerSnapshot struct {
	ExecutionControl    *ComputerExecutionControl `json:"execution_control"`
	ExecutionWait       *ComputerExecutionWait    `json:"execution_wait"`
	DeleteAfterUnusedMs *int64                    `json:"delete_after_unused_ms"`
	UnusedSince         *time.Time                `json:"unused_since"`
	Residency           ComputerResidency         `json:"residency"`
	Error               json.RawMessage           `json:"error,omitempty"`
	ID                  string                    `json:"id"`
	Key                 *string                   `json:"key,omitempty"`
	DefinitionID        string                    `json:"template"`
	DeploymentID        string                    `json:"deployment_id"`
	Status              ComputerStatus            `json:"status"`
	Secrets             []secretbinding.Reference `json:"secrets"`
	LastActivityAt      time.Time                 `json:"last_activity_at"`
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
}

type ComputerListItem struct {
	ExecutionControl    *ComputerExecutionControl `json:"execution_control"`
	ExecutionWait       *ComputerExecutionWait    `json:"execution_wait"`
	DeleteAfterUnusedMs *int64                    `json:"delete_after_unused_ms"`
	UnusedSince         *time.Time                `json:"unused_since"`
	Residency           ComputerResidency         `json:"residency"`
	Error               json.RawMessage           `json:"error,omitempty"`
	ID                  string                    `json:"id"`
	Key                 *string                   `json:"key,omitempty"`
	DefinitionID        string                    `json:"template"`
	DeploymentID        string                    `json:"deployment_id"`
	Status              ComputerStatus            `json:"status"`
	LastActivityAt      time.Time                 `json:"last_activity_at"`
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
}

type ListComputersResponse struct {
	Computers  []ComputerListItem `json:"computers"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// ComputerUsage describes work still occupying a Computer, including pending cleanup.
type ComputerUsage struct {
	ID         string    `json:"id"`
	UsageState string    `json:"usage_state"`
	CreatedAt  time.Time `json:"created_at"`
}

type ListComputerSessionsResponse struct {
	Sessions   []ComputerUsage `json:"sessions"`
	NextCursor string          `json:"next_cursor,omitempty"`
}
type ListComputerCommandsResponse struct {
	Commands   []ComputerUsage `json:"commands"`
	NextCursor string          `json:"next_cursor,omitempty"`
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
