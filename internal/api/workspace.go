package api

import (
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/sourceid"
)

type WorkspaceSecret struct {
	Name string `json:"name"`
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

type CreateWorkspaceRequest struct {
	Key            *string           `json:"key,omitempty"`
	Secrets        []WorkspaceSecret `json:"secrets,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

type WorkspaceStatus string

const (
	WorkspaceStatusAvailable        WorkspaceStatus = "available"
	WorkspaceStatusRecoveryRequired WorkspaceStatus = "recovery_required"
	WorkspaceStatusDeleting         WorkspaceStatus = "deleting"
)

// WorkspaceOwner names the Session or Run that currently owns a Workspace.
// Exactly one of SessionID and RunID is set; an unowned Workspace has no owner.
type WorkspaceOwner struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
}

type WorkspaceSnapshot struct {
	ID             string            `json:"id"`
	Key            *string           `json:"key,omitempty"`
	SandboxID      string            `json:"sandbox_id"`
	DeploymentID   string            `json:"deployment_id"`
	Status         WorkspaceStatus   `json:"status"`
	Owner          *WorkspaceOwner   `json:"owner,omitempty"`
	Secrets        []WorkspaceSecret `json:"secrets"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type WorkspaceListItem struct {
	ID             string          `json:"id"`
	Key            *string         `json:"key,omitempty"`
	SandboxID      string          `json:"sandbox_id"`
	DeploymentID   string          `json:"deployment_id"`
	Status         WorkspaceStatus `json:"status"`
	Owner          *WorkspaceOwner `json:"owner,omitempty"`
	LastActivityAt time.Time       `json:"last_activity_at"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type ListWorkspacesResponse struct {
	Workspaces []WorkspaceListItem `json:"workspaces"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type DeleteWorkspaceRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type DeleteWorkspaceReceipt struct {
	WorkspaceID string `json:"workspace_id"`
}

func ValidateSandboxDeclaredID(id string) error {
	if !sourceid.Valid(id) {
		return fmt.Errorf(
			"workspace declared ID %q must match %s",
			id,
			sourceid.Grammar,
		)
	}
	return nil
}

func ValidateWorkspaceSecret(secret WorkspaceSecret) error {
	if secret.Name == "" {
		return errors.New("workspace secret name is required")
	}
	hasEnv := secret.Env != ""
	hasFile := secret.File != ""
	if hasEnv == hasFile {
		return errors.New("workspace secret must contain exactly one of env or file")
	}
	return nil
}
