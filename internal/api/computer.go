package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/sourceid"
)

type ComputerSecret struct {
	Name string      `json:"secret"`
	Env  *SecretEnv  `json:"env,omitempty"`
	File *SecretFile `json:"file,omitempty"`
}

type SecretEnv struct {
	Name           string   `json:"name"`
	Mode           string   `json:"mode"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
}

type SecretFile struct {
	Path string `json:"path"`
}

type CreateComputerRequest struct {
	Key            *string          `json:"key,omitempty"`
	Secrets        []ComputerSecret `json:"secrets,omitempty"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
}

type ComputerStatus string

const (
	ComputerStatusAvailable ComputerStatus = "available"
	ComputerStatusDeleted   ComputerStatus = "deleted"
	ComputerStatusDeleting  ComputerStatus = "deleting"
)

type ComputerResidency string

type ComputerSnapshot struct {
	Residency      ComputerResidency `json:"residency"`
	Error          json.RawMessage   `json:"error,omitempty"`
	ID             string            `json:"id"`
	Key            *string           `json:"key,omitempty"`
	SandboxID      string            `json:"sandbox_id"`
	DeploymentID   string            `json:"deployment_id"`
	Status         ComputerStatus    `json:"status"`
	Secrets        []ComputerSecret  `json:"secrets"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
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

func ValidateSandboxDeclaredID(id string) error {
	if !sourceid.Valid(id) {
		return fmt.Errorf(
			"computer declared ID %q must match %s",
			id,
			sourceid.Grammar,
		)
	}
	return nil
}

func ValidateComputerSecret(secret ComputerSecret) error {
	if secret.Name == "" {
		return errors.New("computer secret name is required")
	}
	hasEnv := secret.Env != nil
	hasFile := secret.File != nil
	if hasEnv == hasFile {
		return errors.New("computer secret must contain exactly one of env or file")
	}
	if hasEnv {
		if secret.Env.Name == "" || (secret.Env.Mode != "raw" && secret.Env.Mode != "protected") {
			return errors.New("computer secret env requires name and explicit raw or protected mode")
		}
		if secret.Env.Mode == "protected" && len(secret.Env.AllowedOrigins) == 0 || secret.Env.Mode == "raw" && secret.Env.AllowedOrigins != nil {
			return errors.New("allowed_origins is required only for protected env")
		}
	} else if secret.File.Path == "" {
		return errors.New("computer secret file path is required")
	}
	return nil
}
