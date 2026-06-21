package api

import (
	"encoding/json"
	"time"
)

type WorkspaceMaterializeRequest struct {
	ProjectID     string `json:"project_id,omitempty"`
	EnvironmentID string `json:"environment_id,omitempty"`
}

type WorkspaceStopRequest struct {
	ProjectID         string `json:"project_id,omitempty"`
	EnvironmentID     string `json:"environment_id,omitempty"`
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
	IdempotencyKeyTTL string `json:"idempotency_key_ttl,omitempty"`
}

type WorkspaceStopResponse struct {
	WorkspaceID     string                            `json:"workspace_id"`
	State           string                            `json:"state"`
	Materialization *WorkspaceMaterializationResponse `json:"materialization,omitempty"`
}

type WorkspaceMaterializationResponse struct {
	ID                   string     `json:"id"`
	ProjectID            string     `json:"project_id"`
	EnvironmentID        string     `json:"environment_id"`
	WorkspaceID          string     `json:"workspace_id"`
	DeploymentSandboxID  string     `json:"deployment_sandbox_id"`
	BaseVersionID        string     `json:"base_version_id,omitempty"`
	WorkerInstanceID     string     `json:"worker_instance_id,omitempty"`
	State                string     `json:"state"`
	ClaimAttempt         int32      `json:"claim_attempt"`
	FencingGeneration    int64      `json:"fencing_generation"`
	DirtyGeneration      int64      `json:"dirty_generation"`
	ReservationExpiresAt *time.Time `json:"reservation_expires_at,omitempty"`
	LastHeartbeatAt      *time.Time `json:"last_heartbeat_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type WorkerWorkspaceMaterializationClaimRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerWorkspaceMaterializationClaimResponse struct {
	Materialization *WorkerWorkspaceMaterialization `json:"materialization,omitempty"`
}

type WorkerWorkspaceMaterialization struct {
	ID                         string                  `json:"id"`
	OrgID                      string                  `json:"org_id"`
	ProjectID                  string                  `json:"project_id"`
	EnvironmentID              string                  `json:"environment_id"`
	WorkspaceID                string                  `json:"workspace_id"`
	DeploymentSandboxID        string                  `json:"deployment_sandbox_id"`
	BaseVersionID              string                  `json:"base_version_id,omitempty"`
	ReservationToken           string                  `json:"reservation_token"`
	GuestdChannelToken         string                  `json:"guestd_channel_token"`
	GuestdChannelTokenHash     string                  `json:"guestd_channel_token_hash"`
	State                      string                  `json:"state"`
	RuntimeID                  string                  `json:"runtime_id"`
	SandboxImageArtifact       CASObject               `json:"sandbox_image_artifact"`
	SandboxImageArtifactFormat string                  `json:"sandbox_image_artifact_format"`
	RootfsDigest               string                  `json:"rootfs_digest"`
	ImageDigest                string                  `json:"image_digest"`
	ImageFormat                string                  `json:"image_format"`
	WorkspaceArtifact          WorkerWorkspaceArtifact `json:"workspace_artifact"`
	WorkspaceMountPath         string                  `json:"workspace_mount_path"`
	RuntimeABI                 string                  `json:"runtime_abi"`
	GuestdABI                  string                  `json:"guestd_abi"`
	AdapterABI                 string                  `json:"adapter_abi"`
	FencingGeneration          int64                   `json:"fencing_generation"`
	ExpiresAt                  time.Time               `json:"expires_at"`
}

type WorkerWorkspaceMaterializationRenewRequest struct {
	OrgID             string `json:"org_id"`
	MaterializationID string `json:"materialization_id"`
	ReservationToken  string `json:"reservation_token"`
}

type WorkerWorkspaceMaterializationRunningRequest struct {
	OrgID             string `json:"org_id"`
	MaterializationID string `json:"materialization_id"`
	ReservationToken  string `json:"reservation_token"`
}

type WorkerWorkspaceMaterializationStopRequest struct {
	OrgID             string `json:"org_id"`
	MaterializationID string `json:"materialization_id"`
	ReservationToken  string `json:"reservation_token"`
}

type WorkerWorkspaceMaterializationCaptureRequest struct {
	OrgID              string `json:"org_id"`
	ProjectID          string `json:"project_id"`
	EnvironmentID      string `json:"environment_id"`
	WorkspaceID        string `json:"workspace_id"`
	MaterializationID  string `json:"materialization_id"`
	ReservationToken   string `json:"reservation_token"`
	ArtifactDigest     string `json:"artifact_digest"`
	ArtifactSizeBytes  int64  `json:"artifact_size_bytes"`
	ArtifactMediaType  string `json:"artifact_media_type"`
	ArtifactEncoding   string `json:"artifact_encoding"`
	ArtifactEntryCount int32  `json:"artifact_entry_count"`
}

type WorkerWorkspaceMaterializationCaptureResponse struct {
	VersionID string `json:"version_id"`
}

type WorkerWorkspaceMaterializationFailRequest struct {
	OrgID             string          `json:"org_id"`
	MaterializationID string          `json:"materialization_id"`
	ReservationToken  string          `json:"reservation_token"`
	Error             json.RawMessage `json:"error"`
}
