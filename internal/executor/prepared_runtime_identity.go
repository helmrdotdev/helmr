package executor

import (
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/substrate"
)

func preparedRuntimeKeyFromWorkspaceMount(mount api.WorkerWorkspaceMount, network compute.NetworkPolicy) string {
	return runtime.Key(preparedRuntimeIdentityFromWorkspaceMount(mount, network))
}

func preparedRuntimeIdentityFromWorkspaceMount(mount api.WorkerWorkspaceMount, network compute.NetworkPolicy) runtime.Identity {
	return runtime.Identity{
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
		RuntimeSubstrateCacheKey:   preparedRuntimeSubstrateCacheKey(mount),
		Network:                    compute.NetworkPolicyJSON(network),
	}
}

func preparedRuntimeSubstrateCacheKey(mount api.WorkerWorkspaceMount) string {
	return substrate.OptionalCacheKey(substrate.Source{
		SandboxArtifactDigest: mount.SandboxImageArtifact.Digest,
		SandboxArtifactFormat: mount.SandboxImageArtifactFormat,
		ImageDigest:           mount.ImageDigest,
		RootfsDigest:          mount.RootfsDigest,
		RuntimeABI:            mount.RuntimeABI,
		GuestdABI:             mount.GuestdABI,
		AdapterABI:            mount.AdapterABI,
		WorkspaceMountPath:    mount.WorkspaceMountPath,
	})
}
