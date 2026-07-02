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
		RuntimeSubstrateCacheKey:   preparedRuntimeSubstrateCacheKey(mount.SandboxImageArtifact.Digest, mount.SandboxImageArtifactFormat, mount.ImageDigest, mount.RootfsDigest, mount.RuntimeABI, mount.GuestdABI, mount.AdapterABI, mount.WorkspaceMountPath),
		Network:                    compute.NetworkPolicyJSON(network),
	}
}

func preparedRuntimeSubstrateCacheKey(sandboxDigest string, sandboxFormat string, imageDigest string, rootfsDigest string, runtimeABI string, guestdABI string, adapterABI string, workspaceMountPath string) string {
	key, err := substrate.CacheKey(substrate.Source{
		SandboxArtifactDigest: sandboxDigest,
		SandboxArtifactFormat: sandboxFormat,
		ImageDigest:           imageDigest,
		RootfsDigest:          rootfsDigest,
		RuntimeABI:            runtimeABI,
		GuestdABI:             guestdABI,
		AdapterABI:            adapterABI,
		WorkspaceMountPath:    workspaceMountPath,
	})
	if err != nil {
		return ""
	}
	return key
}
