package workerapi

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
	WorkspaceID string                  `json:"workspace_id"`
	State       string                  `json:"state"`
	Mount       *WorkspaceMountResponse `json:"mount,omitempty"`
}

type WorkspaceMountResponse struct {
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
	FinalizationKind     string     `json:"finalization_kind,omitempty"`
	ReservationExpiresAt *time.Time `json:"reservation_expires_at,omitempty"`
	LastHeartbeatAt      *time.Time `json:"last_heartbeat_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type WorkspaceMountClaimRequest struct {
	Capabilities Capabilities `json:"capabilities"`
}

type WorkspaceMountClaimResponse struct {
	Mount *WorkspaceMount `json:"mount,omitempty"`
}

type WorkspaceMount struct {
	ID                      string            `json:"id"`
	OrgID                   string            `json:"org_id"`
	ProjectID               string            `json:"project_id"`
	EnvironmentID           string            `json:"environment_id"`
	WorkspaceID             string            `json:"workspace_id"`
	DeploymentDefinitionID  string            `json:"deployment_definition_id"`
	BaseVersionID           string            `json:"base_version_id,omitempty"`
	RuntimeInstanceID       string            `json:"runtime_instance_id,omitempty"`
	RestoreCheckpointID     string            `json:"restore_checkpoint_id,omitempty"`
	RuntimeEpoch            int64             `json:"runtime_epoch"`
	GuestdChannelToken      string            `json:"guestd_channel_token"`
	GuestdChannelTokenHash  string            `json:"guestd_channel_token_hash"`
	State                   string            `json:"state"`
	RuntimeIdentityID       string            `json:"runtime_identity_id"`
	WorkspaceImage          CASObject         `json:"workspace_image"`
	RootfsDigest            string            `json:"rootfs_digest"`
	WorkspaceArtifact       WorkspaceArtifact `json:"workspace_artifact"`
	WorkspaceMountPath      string            `json:"workspace_mount_path"`
	RequestedMilliCPU       int64             `json:"requested_milli_cpu"`
	RequestedMemoryMiB      int64             `json:"requested_memory_mib"`
	RequestedDiskMiB        int64             `json:"requested_disk_mib"`
	RequestedExecutionSlots int32             `json:"requested_execution_slots"`
	RuntimeABI              string            `json:"runtime_abi"`
	FencingGeneration       int64             `json:"fencing_generation"`
	ExpiresAt               time.Time         `json:"expires_at"`
}

type WorkspaceMountRenewRequest struct {
	OrgID            string `json:"org_id"`
	WorkspaceMountID string `json:"workspace_mount_id"`
}

type WorkspaceMountMountedRequest struct {
	OrgID            string `json:"org_id"`
	WorkspaceMountID string `json:"workspace_mount_id"`
}

type WorkspaceMountStopRequest struct {
	OrgID            string              `json:"org_id"`
	WorkspaceMountID string              `json:"workspace_mount_id"`
	CleanupProof     RuntimeCleanupProof `json:"cleanup_proof"`
}

type WorkspaceMountCaptureRequest struct {
	OrgID              string `json:"org_id"`
	ProjectID          string `json:"project_id"`
	EnvironmentID      string `json:"environment_id"`
	WorkspaceID        string `json:"workspace_id"`
	WorkspaceMountID   string `json:"workspace_mount_id"`
	ArtifactDigest     string `json:"artifact_digest"`
	ArtifactSizeBytes  int64  `json:"artifact_size_bytes"`
	ArtifactMediaType  string `json:"artifact_media_type"`
	ArtifactEncoding   string `json:"artifact_encoding"`
	ArtifactEntryCount int32  `json:"artifact_entry_count"`
}

type WorkspaceMountCaptureResponse struct {
	VersionID string `json:"version_id"`
}

type WorkspaceMountFailRequest struct {
	OrgID            string          `json:"org_id"`
	WorkspaceMountID string          `json:"workspace_mount_id"`
	Error            json.RawMessage `json:"error"`
}
