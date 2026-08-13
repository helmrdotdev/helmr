package api

import (
	"errors"
	"regexp"
	"strings"
	"time"
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

type DeploymentBundleUpload struct {
	Digest  string            `json:"digest"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type DeploymentBundleUploadPlanResponse struct {
	BundleDigest string                   `json:"bundle_digest"`
	Uploads      []DeploymentBundleUpload `json:"uploads"`
}

type FinalizeDeploymentBundleRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	BundleDigest   string `json:"bundle_digest"`
}

type DeploymentResponse struct {
	ID           string    `json:"id"`
	Version      string    `json:"version"`
	BundleDigest string    `json:"bundle_digest"`
	CreatedAt    time.Time `json:"created_at"`
}

type DeploymentListItem struct {
	ID           string    `json:"id"`
	Version      string    `json:"version"`
	BundleDigest string    `json:"bundle_digest"`
	CreatedAt    time.Time `json:"created_at"`
}

type PromoteDeploymentRequest struct {
	Reason string `json:"reason,omitempty"`
}

type ListDeploymentsResponse struct {
	Deployments []DeploymentListItem `json:"deployments"`
	NextCursor  string               `json:"next_cursor,omitempty"`
}
