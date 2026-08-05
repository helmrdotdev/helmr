package api

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

var workspaceDeclaredIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

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
	WorkspaceStatusRecoveryRequired WorkspaceStatus = "recovery-required"
	WorkspaceStatusDeleting         WorkspaceStatus = "deleting"
)

type WorkspaceSnapshot struct {
	ID             string            `json:"id"`
	Key            *string           `json:"key,omitempty"`
	SandboxID      string            `json:"sandbox_id"`
	DeploymentID   string            `json:"deployment_id"`
	Status         WorkspaceStatus   `json:"status"`
	Secrets        []WorkspaceSecret `json:"secrets"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ListWorkspacesResponse struct {
	Workspaces []WorkspaceSnapshot `json:"workspaces"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type DeleteWorkspaceRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type DeleteWorkspaceReceipt struct {
	WorkspaceID string `json:"workspace_id"`
}

func ValidateSandboxDeclaredID(id string) error {
	if !workspaceDeclaredIDPattern.MatchString(id) {
		return fmt.Errorf(
			"workspace declared ID %q must match %s",
			id,
			workspaceDeclaredIDPattern.String(),
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
