package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type Identity struct {
	RuntimeID                  string          `json:"runtime_id"`
	DeploymentSandboxID        string          `json:"deployment_sandbox_id"`
	ImageDigest                string          `json:"image_digest"`
	ImageFormat                string          `json:"image_format"`
	RootfsDigest               string          `json:"rootfs_digest"`
	RuntimeABI                 string          `json:"runtime_abi"`
	GuestdABI                  string          `json:"guestd_abi"`
	AdapterABI                 string          `json:"adapter_abi"`
	WorkspaceMountPath         string          `json:"workspace_mount_path"`
	SandboxImageArtifactDigest string          `json:"sandbox_artifact_digest"`
	SandboxImageArtifactFormat string          `json:"sandbox_artifact_format"`
	RuntimeSubstrateCacheKey   string          `json:"substrate_key"`
	Network                    json.RawMessage `json:"network"`
}

func Key(identity Identity) string {
	identity = normalizeIdentity(identity)
	body, _ := json.Marshal(identity)
	return string(body)
}

func normalizeIdentity(identity Identity) Identity {
	identity.RuntimeID = strings.TrimSpace(identity.RuntimeID)
	identity.DeploymentSandboxID = strings.TrimSpace(identity.DeploymentSandboxID)
	identity.ImageDigest = strings.TrimSpace(identity.ImageDigest)
	identity.ImageFormat = strings.TrimSpace(identity.ImageFormat)
	identity.RootfsDigest = strings.TrimSpace(identity.RootfsDigest)
	identity.RuntimeABI = strings.TrimSpace(identity.RuntimeABI)
	identity.GuestdABI = strings.TrimSpace(identity.GuestdABI)
	identity.AdapterABI = strings.TrimSpace(identity.AdapterABI)
	identity.WorkspaceMountPath = strings.TrimSpace(identity.WorkspaceMountPath)
	identity.SandboxImageArtifactDigest = strings.TrimSpace(identity.SandboxImageArtifactDigest)
	identity.SandboxImageArtifactFormat = strings.TrimSpace(identity.SandboxImageArtifactFormat)
	identity.RuntimeSubstrateCacheKey = strings.TrimSpace(identity.RuntimeSubstrateCacheKey)
	if len(identity.Network) == 0 {
		identity.Network = json.RawMessage(`{}`)
	}
	return identity
}

func ID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
