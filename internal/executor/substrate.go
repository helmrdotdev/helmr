package executor

import (
	"context"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type RuntimeSubstrateResolver interface {
	Resolve(context.Context, string, substrate.Source) (substrate.Result, error)
}

type RuntimeSubstrateRegistrar interface {
	RegisterRuntimeSubstrate(context.Context, workerapi.RuntimeSubstrateRegisterRequest) (workerapi.RuntimeSubstrateRegisterResponse, error)
}

func runtimeSubstrateTopology(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, mount workerapi.WorkspaceMount) (vm.RuntimeTopology, error) {
	if resolver == nil {
		return vm.RuntimeTopology{}, nil
	}
	result, err := resolver.Resolve(ctx, imagePath, substrate.Source{
		WorkspaceImageDigest:    mount.WorkspaceImage.Digest,
		WorkspaceImageMediaType: mount.WorkspaceImage.MediaType,
	})
	if err != nil {
		return vm.RuntimeTopology{}, err
	}
	return vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Path:      result.Path,
		Digest:    result.Digest,
		Format:    result.Format,
		Contract:  result.Contract,
		SizeBytes: result.SizeBytes,
	}}, nil
}

func runtimeSubstrateDigest(topology vm.RuntimeTopology) string {
	if topology.Substrate == nil {
		return ""
	}
	return topology.Substrate.Digest
}

func runtimeSubstrateID(artifact *workerapi.RuntimeSubstrate) string {
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
) (*workerapi.RuntimeSubstrate, error) {
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
		workerapi.RuntimeSubstrateRegisterRequest{
			DeploymentDefinitionID: strings.TrimSpace(deploymentDefinitionID),
			SubstrateDigest:        strings.TrimSpace(substrate.Digest),
			Format:                 strings.TrimSpace(substrate.Format),
			Contract:               strings.TrimSpace(substrate.Contract),
			SizeBytes:              substrate.SizeBytes,
		},
	)
	if err != nil {
		return nil, err
	}
	registered := response.RuntimeSubstrate
	return &registered, nil
}
