package executor

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type RuntimeSubstrateResolver interface {
	Resolve(context.Context, string, runtime.Source) (runtime.Result, error)
}

type RuntimeSubstrateDigestLookup interface {
	LookupDigest(context.Context, string) (runtime.Result, error)
}

type RuntimeSubstrateRegistrar interface {
	RegisterRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateRegisterRequest) (api.WorkerRuntimeSubstrateRegisterResponse, error)
}

type RuntimeSubstrateLookup interface {
	LookupRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateLookupRequest) (api.WorkerRuntimeSubstrateLookupResponse, error)
}

func runtimeSubstrateTopology(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, mount api.WorkerWorkspaceMount) (vm.RuntimeTopology, error) {
	return runtimeSubstrateTopologyFromSource(ctx, resolver, imagePath, api.WorkerRuntimeSubstrateSource{
		DeploymentDefinitionID: mount.DeploymentDefinitionID,
		WorkspaceImage:         mount.WorkspaceImage,
	})
}

func runtimeSubstrateSourceFromRuntimeSource(source api.WorkerRuntimeSource) *api.WorkerRuntimeSubstrateSource {
	return &api.WorkerRuntimeSubstrateSource{
		DeploymentDefinitionID: source.DeploymentDefinitionID,
		WorkspaceImage:         source.WorkspaceImage,
		RuntimeSubstrate:       source.RuntimeSubstrate,
	}
}

func runtimeSubstrateSourceFromWorkspaceMount(mount api.WorkerWorkspaceMount) *api.WorkerRuntimeSubstrateSource {
	return &api.WorkerRuntimeSubstrateSource{
		DeploymentDefinitionID: mount.DeploymentDefinitionID,
		WorkspaceImage:         mount.WorkspaceImage,
	}
}

func runtimeSubstrateTopologyFromSource(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, source api.WorkerRuntimeSubstrateSource) (vm.RuntimeTopology, error) {
	if resolver == nil {
		return vm.RuntimeTopology{}, nil
	}
	result, err := resolver.Resolve(ctx, imagePath, runtime.Source{
		WorkspaceImageDigest:    source.WorkspaceImage.Digest,
		WorkspaceImageMediaType: source.WorkspaceImage.MediaType,
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
