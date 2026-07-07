package executor

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type RuntimeSubstrateResolver interface {
	Resolve(context.Context, string, substrate.Source) (substrate.Result, error)
}

type RuntimeSubstrateDigestLookup interface {
	LookupDigest(context.Context, string) (substrate.Result, error)
}

type RuntimeSubstrateRegistrar interface {
	RegisterRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateRegisterRequest) (api.WorkerRuntimeSubstrateRegisterResponse, error)
}

type RuntimeSubstrateLookup interface {
	LookupRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateLookupRequest) (api.WorkerRuntimeSubstrateLookupResponse, error)
}

func runtimeSubstrateTopology(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, mount api.WorkerWorkspaceMount) (vm.RuntimeTopology, error) {
	return runtimeSubstrateTopologyFromSource(ctx, resolver, imagePath, api.WorkerRuntimeSubstrateSource{
		DeploymentSandboxID:        mount.DeploymentSandboxID,
		SandboxImageArtifact:       mount.SandboxImageArtifact,
		SandboxImageArtifactFormat: mount.SandboxImageArtifactFormat,
		ImageDigest:                mount.ImageDigest,
		ImageFormat:                mount.ImageFormat,
		RootfsDigest:               mount.RootfsDigest,
		RuntimeABI:                 mount.RuntimeABI,
		GuestdABI:                  mount.GuestdABI,
		AdapterABI:                 mount.AdapterABI,
		WorkspaceMountPath:         mount.WorkspaceMountPath,
	})
}

func runtimeSubstrateSourceFromPreparedSource(source api.WorkerPreparedRuntimeSource) *api.WorkerRuntimeSubstrateSource {
	return &api.WorkerRuntimeSubstrateSource{
		DeploymentSandboxID:        source.DeploymentSandboxID,
		SandboxImageArtifact:       source.SandboxImageArtifact,
		SandboxImageArtifactFormat: source.SandboxImageArtifactFormat,
		RootfsDigest:               source.RootfsDigest,
		ImageDigest:                source.ImageDigest,
		ImageFormat:                source.ImageFormat,
		WorkspaceMountPath:         source.WorkspaceMountPath,
		RuntimeABI:                 source.RuntimeABI,
		GuestdABI:                  source.GuestdABI,
		AdapterABI:                 source.AdapterABI,
		RuntimeSubstrate:           source.RuntimeSubstrate,
	}
}

func runtimeSubstrateSourceFromWorkspaceMount(mount api.WorkerWorkspaceMount) *api.WorkerRuntimeSubstrateSource {
	return &api.WorkerRuntimeSubstrateSource{
		DeploymentSandboxID:        mount.DeploymentSandboxID,
		SandboxImageArtifact:       mount.SandboxImageArtifact,
		SandboxImageArtifactFormat: mount.SandboxImageArtifactFormat,
		RootfsDigest:               mount.RootfsDigest,
		ImageDigest:                mount.ImageDigest,
		ImageFormat:                mount.ImageFormat,
		WorkspaceMountPath:         mount.WorkspaceMountPath,
		RuntimeABI:                 mount.RuntimeABI,
		GuestdABI:                  mount.GuestdABI,
		AdapterABI:                 mount.AdapterABI,
	}
}

func runtimeSubstrateTopologyFromSource(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, source api.WorkerRuntimeSubstrateSource) (vm.RuntimeTopology, error) {
	if resolver == nil {
		return vm.RuntimeTopology{}, nil
	}
	result, err := resolver.Resolve(ctx, imagePath, substrate.Source{
		SandboxArtifactDigest: source.SandboxImageArtifact.Digest,
		SandboxArtifactFormat: source.SandboxImageArtifactFormat,
		ImageDigest:           source.ImageDigest,
		RootfsDigest:          source.RootfsDigest,
		RuntimeABI:            source.RuntimeABI,
		GuestdABI:             source.GuestdABI,
		AdapterABI:            source.AdapterABI,
		WorkspaceMountPath:    source.WorkspaceMountPath,
	})
	if err != nil {
		return vm.RuntimeTopology{}, err
	}
	return vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Path:       result.Path,
		Digest:     result.Digest,
		Format:     result.Format,
		BuilderABI: result.BuilderABI,
		LayoutABI:  result.LayoutABI,
	}}, nil
}

func runtimeSubstrateDigest(topology vm.RuntimeTopology) string {
	if topology.Substrate == nil {
		return ""
	}
	return topology.Substrate.Digest
}

func runtimeSubstrateID(artifact *api.WorkerRuntimeSubstrate) string {
	if artifact == nil {
		return ""
	}
	return artifact.ID
}
