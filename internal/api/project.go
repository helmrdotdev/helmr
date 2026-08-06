package api

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
)

var environmentColorHexPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

type ProjectSummary struct {
	ID              string               `json:"id"`
	Slug            string               `json:"slug"`
	Name            string               `json:"name"`
	DefaultRegionID string               `json:"default_region_id"`
	IsDefault       bool                 `json:"is_default"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
	Environments    []EnvironmentSummary `json:"environments,omitempty"`
}

type EnvironmentSummary struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	ColorHex  string    `json:"color_hex"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ListProjectsResponse struct {
	Projects []ProjectSummary `json:"projects"`
}

type CreateProjectRequest struct {
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	DefaultRegionID string `json:"default_region_id"`
}

type UpdateProjectRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type CreateEnvironmentRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ColorHex string `json:"color_hex"`
}

type UpdateEnvironmentRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ColorHex string `json:"color_hex"`
}

func NormalizeEnvironmentColorHex(colorHex string) (string, error) {
	colorHex = strings.TrimSpace(colorHex)
	if !environmentColorHexPattern.MatchString(colorHex) {
		return "", errors.New("must be a #RRGGBB color")
	}
	return strings.ToUpper(colorHex), nil
}

type CreateDeploymentRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	ContentHash    string `json:"content_hash"`
	ImageCacheMode string `json:"image_cache_mode,omitempty"`
}

type DeploymentStatus string

const (
	DeploymentStatusQueued   DeploymentStatus = "queued"
	DeploymentStatusBuilding DeploymentStatus = "building"
	DeploymentStatusDeployed DeploymentStatus = "deployed"
	DeploymentStatusFailed   DeploymentStatus = "failed"
)

type DeploymentResponse struct {
	ID               string                   `json:"id"`
	Version          string                   `json:"version"`
	ContentHash      string                   `json:"content_hash"`
	DeploymentSource DeploymentSourceArtifact `json:"deployment_source"`
	Status           DeploymentStatus         `json:"status"`
	Failure          *DeploymentFailure       `json:"failure,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
	BuildingAt       *time.Time               `json:"building_at,omitempty"`
	BuiltAt          *time.Time               `json:"built_at,omitempty"`
	DeployedAt       *time.Time               `json:"deployed_at,omitempty"`
	FailedAt         *time.Time               `json:"failed_at,omitempty"`
}

type DeploymentListItem struct {
	ID         string           `json:"id"`
	Version    string           `json:"version"`
	Status     DeploymentStatus `json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
	BuildingAt *time.Time       `json:"building_at,omitempty"`
	BuiltAt    *time.Time       `json:"built_at,omitempty"`
	DeployedAt *time.Time       `json:"deployed_at,omitempty"`
	FailedAt   *time.Time       `json:"failed_at,omitempty"`
}

type PromoteDeploymentRequest struct {
	Reason string `json:"reason,omitempty"`
}

type DeploymentFailure struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

type ListDeploymentsResponse struct {
	Deployments []DeploymentListItem `json:"deployments"`
	NextCursor  string               `json:"next_cursor,omitempty"`
}

const DeploymentSourceArtifactMediaType = archive.SourceMediaType

type DeploymentSourceArtifact struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}
