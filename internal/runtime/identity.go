package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
)

func KeyFromWorkspaceMount(mount api.WorkerWorkspaceMount, network compute.NetworkPolicy) string {
	return Key(Identity{
		RuntimeID:                  mount.RuntimeID,
		DeploymentSandboxID:        mount.DeploymentSandboxID,
		ImageDigest:                mount.ImageDigest,
		ImageFormat:                mount.ImageFormat,
		RootfsDigest:               mount.RootfsDigest,
		RuntimeABI:                 mount.RuntimeABI,
		GuestdABI:                  mount.GuestdABI,
		AdapterABI:                 mount.AdapterABI,
		WorkspaceMountPath:         mount.WorkspaceMountPath,
		SandboxImageArtifactDigest: mount.SandboxImageArtifact.Digest,
		SandboxImageArtifactFormat: mount.SandboxImageArtifactFormat,
		RuntimeSubstrateCacheKey:   substrateKey(mount.SandboxImageArtifact.Digest, mount.SandboxImageArtifactFormat, mount.ImageDigest, mount.RootfsDigest, mount.RuntimeABI, mount.GuestdABI, mount.AdapterABI, mount.WorkspaceMountPath),
		Network:                    network,
	})
}

func KeyFromSource(source api.WorkerPreparedRuntimeSource, network compute.NetworkPolicy) string {
	return Key(Identity{
		RuntimeID:                  source.RuntimeID,
		DeploymentSandboxID:        source.DeploymentSandboxID,
		ImageDigest:                source.ImageDigest,
		ImageFormat:                source.ImageFormat,
		RootfsDigest:               source.RootfsDigest,
		RuntimeABI:                 source.RuntimeABI,
		GuestdABI:                  source.GuestdABI,
		AdapterABI:                 source.AdapterABI,
		WorkspaceMountPath:         source.WorkspaceMountPath,
		SandboxImageArtifactDigest: source.SandboxImageArtifact.Digest,
		SandboxImageArtifactFormat: source.SandboxImageArtifactFormat,
		RuntimeSubstrateCacheKey:   substrateKey(source.SandboxImageArtifact.Digest, source.SandboxImageArtifactFormat, source.ImageDigest, source.RootfsDigest, source.RuntimeABI, source.GuestdABI, source.AdapterABI, source.WorkspaceMountPath),
		Network:                    network,
	})
}

type Identity struct {
	RuntimeID                  string                `json:"runtime_id"`
	DeploymentSandboxID        string                `json:"deployment_sandbox_id"`
	ImageDigest                string                `json:"image_digest"`
	ImageFormat                string                `json:"image_format"`
	RootfsDigest               string                `json:"rootfs_digest"`
	RuntimeABI                 string                `json:"runtime_abi"`
	GuestdABI                  string                `json:"guestd_abi"`
	AdapterABI                 string                `json:"adapter_abi"`
	WorkspaceMountPath         string                `json:"workspace_mount_path"`
	SandboxImageArtifactDigest string                `json:"sandbox_artifact_digest"`
	SandboxImageArtifactFormat string                `json:"sandbox_artifact_format"`
	RuntimeSubstrateCacheKey   string                `json:"substrate_key"`
	Network                    compute.NetworkPolicy `json:"network"`
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

func NetworkPolicyJSON(network compute.NetworkPolicy) json.RawMessage {
	body, _ := json.Marshal(network)
	if len(body) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(body)
}
