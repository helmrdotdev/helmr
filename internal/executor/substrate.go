package executor

import (
	"context"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type RuntimeSubstrateResolver interface {
	Resolve(context.Context, string, runtime.Source) (runtime.Result, error)
}

type RuntimeSubstrateRegistrar interface {
	RegisterRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateRegisterRequest) (api.WorkerRuntimeSubstrateRegisterResponse, error)
}

func runtimeSubstrateTopology(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, mount api.WorkerWorkspaceMount) (vm.RuntimeTopology, error) {
	if resolver == nil {
		return vm.RuntimeTopology{}, nil
	}
	result, err := resolver.Resolve(ctx, imagePath, runtime.Source{
		WorkspaceImageDigest:    mount.WorkspaceImage.Digest,
		WorkspaceImageMediaType: mount.WorkspaceImage.MediaType,
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
		SizeBytes:  result.SizeBytes,
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

func registerRuntimeSubstrate(
	ctx context.Context,
	registrar RuntimeSubstrateRegistrar,
	deploymentDefinitionID string,
	substrate *vm.RuntimeSubstrate,
) (*api.WorkerRuntimeSubstrate, error) {
	if substrate == nil {
		return nil, nil
	}
	if registrar == nil {
		return nil, errors.New("runtime substrate registrar is required")
	}
	if strings.TrimSpace(deploymentDefinitionID) == "" {
		return nil, errors.New("runtime substrate deployment_definition_id is required")
	}
	response, err := registrar.RegisterRuntimeSubstrate(
		ctx,
		api.WorkerRuntimeSubstrateRegisterRequest{
			DeploymentDefinitionID: strings.TrimSpace(deploymentDefinitionID),
			SubstrateDigest:        strings.TrimSpace(substrate.Digest),
			Format:                 strings.TrimSpace(substrate.Format),
			BuilderABI:             strings.TrimSpace(substrate.BuilderABI),
			LayoutABI:              strings.TrimSpace(substrate.LayoutABI),
			SizeBytes:              substrate.SizeBytes,
		},
	)
	if err != nil {
		return nil, err
	}
	registered := response.RuntimeSubstrate
	return &registered, nil
}
