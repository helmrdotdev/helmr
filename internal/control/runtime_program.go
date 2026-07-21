package control

import (
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/jackc/pgx/v5/pgtype"
)

func projectDeploymentProgram(
	row db.GetDeploymentProgramAuthorityRow,
	policy *deployment.BuildPolicy,
) (api.WorkerRuntimeProgram, error) {
	if _, err := requiredClaimUUIDString("Program environment ID", row.EnvironmentID); err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	if !row.ProgramArchitecture.Valid {
		return api.WorkerRuntimeProgram{}, errors.New("Deployment Program authority is incomplete")
	}
	return projectRuntimeProgram(
		runtimeProgramAuthorityFromDeployment(
			row.DeploymentID,
			row.ProgramRuntimeDigest,
			row.ProgramArchitecture.String,
			row.ProgramCodeDigest,
			row.ProgramCodeSizeBytes,
			row.ProgramCodeMediaType,
			row.ProgramDependencyDigest,
			row.ProgramDependencySizeBytes,
			row.ProgramDependencyMediaType,
			row.BuildContractVersion,
		),
		"",
		policy,
	)
}

type runtimeProgramAuthority struct {
	deploymentID         pgtype.UUID
	runtimeDigest        []byte
	architecture         string
	codeDigest           string
	codeSizeBytes        int64
	codeMediaType        string
	dependencyDigest     string
	dependencySizeBytes  int64
	dependencyMediaType  string
	buildContractVersion string
}

func projectRuntimeProgram(
	authority runtimeProgramAuthority,
	expectedArchitecture string,
	policy *deployment.BuildPolicy,
) (api.WorkerRuntimeProgram, error) {
	deploymentID, err := requiredClaimUUIDString("Program Deployment ID", authority.deploymentID)
	if err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	runtimeDigest, err := deployment.RuntimeDigestString(authority.runtimeDigest)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("decode Program Managed Runtime digest: %w", err)
	}
	if policy == nil {
		return api.WorkerRuntimeProgram{}, errors.New("runtime catalog policy is not configured")
	}
	runtimeDescriptor, err := policy.ResolveRuntime(runtimeDigest)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("resolve Program Managed Runtime: %w", err)
	}
	if string(runtimeDescriptor.Architecture) != authority.architecture {
		return api.WorkerRuntimeProgram{}, errors.New("Program architecture does not match Managed Runtime")
	}
	if expectedArchitecture != "" && authority.architecture != expectedArchitecture {
		return api.WorkerRuntimeProgram{}, errors.New("Program architecture does not match Workspace")
	}
	runtimeWire, err := deployment.RuntimeDescriptorWire(runtimeDescriptor)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("encode Program Managed Runtime descriptor: %w", err)
	}
	code, err := projectCASObject(
		authority.codeDigest,
		authority.codeSizeBytes,
		authority.codeMediaType,
		"Program code",
	)
	if err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	dependencies, err := projectCASObject(
		authority.dependencyDigest,
		authority.dependencySizeBytes,
		authority.dependencyMediaType,
		"Program dependencies",
	)
	if err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	if strings.TrimSpace(authority.buildContractVersion) == "" {
		return api.WorkerRuntimeProgram{}, errors.New("Program build contract version is required")
	}
	return api.WorkerRuntimeProgram{
		DeploymentID:         deploymentID,
		Runtime:              runtimeWire,
		Code:                 code,
		Dependencies:         dependencies,
		BuildContractVersion: authority.buildContractVersion,
	}, nil
}

func projectCASObject(digest string, sizeBytes int64, mediaType string, name string) (api.CASObject, error) {
	digest = strings.TrimSpace(digest)
	if _, err := cas.ObjectKey("", digest); err != nil {
		return api.CASObject{}, fmt.Errorf("%s digest is invalid: %w", name, err)
	}
	mediaType = strings.TrimSpace(mediaType)
	if mediaType == "" {
		return api.CASObject{}, fmt.Errorf("%s media type is required", name)
	}
	if sizeBytes < 0 {
		return api.CASObject{}, fmt.Errorf("%s size is negative", name)
	}
	return api.CASObject{Digest: digest, SizeBytes: sizeBytes, MediaType: mediaType}, nil
}

func runtimeProgramAuthorityFromDeployment(
	deploymentID pgtype.UUID,
	runtimeDigest []byte,
	architecture string,
	codeDigest string,
	codeSizeBytes int64,
	codeMediaType string,
	dependencyDigest string,
	dependencySizeBytes int64,
	dependencyMediaType string,
	buildContractVersion string,
) runtimeProgramAuthority {
	return runtimeProgramAuthority{
		deploymentID:         deploymentID,
		runtimeDigest:        runtimeDigest,
		architecture:         architecture,
		codeDigest:           codeDigest,
		codeSizeBytes:        codeSizeBytes,
		codeMediaType:        codeMediaType,
		dependencyDigest:     dependencyDigest,
		dependencySizeBytes:  dependencySizeBytes,
		dependencyMediaType:  dependencyMediaType,
		buildContractVersion: buildContractVersion,
	}
}
