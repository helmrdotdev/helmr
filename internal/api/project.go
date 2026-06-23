package api

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var environmentColorHexPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

type ProjectSummary struct {
	ID           string               `json:"id"`
	Slug         string               `json:"slug"`
	Name         string               `json:"name"`
	IsDefault    bool                 `json:"is_default"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
	Environments []EnvironmentSummary `json:"environments,omitempty"`
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
	Slug string `json:"slug"`
	Name string `json:"name"`
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
	ProjectID             string `json:"project_id"`
	EnvironmentID         string `json:"environment_id,omitempty"`
	ContentHash           string `json:"content_hash"`
	APIVersion            string `json:"api_version,omitempty"`
	SDKVersion            string `json:"sdk_version,omitempty"`
	CLIVersion            string `json:"cli_version,omitempty"`
	BundleFormatVersion   int32  `json:"bundle_format_version,omitempty"`
	WorkerProtocolVersion string `json:"worker_protocol_version,omitempty"`
}

type GetDeploymentRequest struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id,omitempty"`
}

type DeploymentResponse struct {
	ID                       string                   `json:"id"`
	Version                  string                   `json:"version"`
	APIVersion               string                   `json:"api_version"`
	SDKVersion               string                   `json:"sdk_version,omitempty"`
	CLIVersion               string                   `json:"cli_version,omitempty"`
	BundleFormatVersion      int32                    `json:"bundle_format_version"`
	WorkerProtocolVersion    string                   `json:"worker_protocol_version"`
	ProjectID                string                   `json:"project_id"`
	EnvironmentID            string                   `json:"environment_id"`
	ContentHash              string                   `json:"content_hash"`
	DeploymentSource         DeploymentSourceArtifact `json:"deployment_source"`
	BuildManifestDigest      string                   `json:"build_manifest_digest,omitempty"`
	DeploymentManifestDigest string                   `json:"deployment_manifest_digest,omitempty"`
	Status                   string                   `json:"status"`
	Error                    *DeploymentErrorResponse `json:"error,omitempty"`
	Tasks                    []DeploymentTaskResponse `json:"tasks"`
	CreatedAt                time.Time                `json:"created_at"`
	BuildingAt               time.Time                `json:"building_at"`
	BuiltAt                  time.Time                `json:"built_at"`
	DeployedAt               time.Time                `json:"deployed_at"`
	FailedAt                 time.Time                `json:"failed_at"`
}

type PromoteDeploymentRequest struct {
	ProjectID     string `json:"project_id,omitempty"`
	EnvironmentID string `json:"environment_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type DeploymentErrorResponse struct {
	Message string `json:"message,omitempty"`
}

type GetCurrentDeploymentResponse struct {
	Deployment *DeploymentResponse `json:"deployment"`
}

type ListDeploymentsResponse struct {
	Deployments []DeploymentResponse `json:"deployments"`
}

const DeploymentSourceArtifactMediaType = "application/vnd.helmr.deployment-source.v0.tar"
const TaskBundleArtifactMediaType = "application/vnd.helmr.task-bundle.v0+proto"
const DeploymentManifestArtifactMediaType = "application/vnd.helmr.deployment-manifest.v0+json"
const BuildManifestArtifactMediaType = "application/vnd.helmr.build-manifest.v0+json"
const SandboxImageArtifactMediaType = "application/vnd.helmr.sandbox-image.v0.oci-tar"

type DeploymentSourceArtifact struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type DeploymentTaskResponse struct {
	ID                  string    `json:"id"`
	TaskID              string    `json:"task_id"`
	FilePath            string    `json:"file_path,omitempty"`
	ExportName          string    `json:"export_name,omitempty"`
	HandlerEntrypoint   string    `json:"handler_entrypoint,omitempty"`
	BundleDigest        string    `json:"bundle_digest,omitempty"`
	BundleFormatVersion int32     `json:"bundle_format_version"`
	QueueName           string    `json:"queue_name,omitempty"`
	ConcurrencyLimit    *int32    `json:"concurrency_limit,omitempty"`
	TTL                 string    `json:"ttl,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

type ListTasksResponse struct {
	Tasks []DeploymentTaskResponse `json:"tasks"`
}

type SandboxResponse struct {
	ID                  string          `json:"id"`
	DeploymentID        string          `json:"deployment_id"`
	SandboxID           string          `json:"sandbox_id"`
	Fingerprint         string          `json:"fingerprint"`
	ImageArtifactID     string          `json:"image_artifact_id"`
	ImageArtifactFormat string          `json:"image_artifact_format"`
	RootfsDigest        string          `json:"rootfs_digest"`
	ImageDigest         string          `json:"image_digest"`
	ImageFormat         string          `json:"image_format"`
	WorkspaceMountPath  string          `json:"workspace_mount_path"`
	ResourceFloor       json.RawMessage `json:"resource_floor,omitempty"`
	DiskFloorMib        int32           `json:"disk_floor_mib"`
	NetworkPolicy       json.RawMessage `json:"network_policy,omitempty"`
	RuntimeABI          string          `json:"runtime_abi"`
	GuestdABI           string          `json:"guestd_abi"`
	AdapterABI          string          `json:"adapter_abi"`
	FilesystemFormat    string          `json:"filesystem_format"`
	DefaultUID          *int32          `json:"default_uid,omitempty"`
	DefaultGID          *int32          `json:"default_gid,omitempty"`
	DefaultWorkdir      string          `json:"default_workdir"`
	ContractVersion     int32           `json:"contract_version"`
	CreatedAt           time.Time       `json:"created_at"`
}

type ListSandboxesResponse struct {
	Sandboxes []SandboxResponse `json:"sandboxes"`
}
