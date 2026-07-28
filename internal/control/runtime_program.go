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
	return projectRuntimeProgram(
		runtimeProgramAuthorityFromDeployment(
			row.DeploymentID,
			row.BuildRuntimeDigest,
			row.ProgramArtifactDigest,
			row.ProgramArtifactSizeBytes,
			row.ProgramArtifactMediaType,
			row.BuildContractVersion,
			row.ProgramIndexDigest,
		),
		"",
		policy,
	)
}

type runtimeProgramAuthority struct {
	deploymentID         pgtype.UUID
	runtimeDigest        []byte
	artifactDigest       string
	artifactSizeBytes    int64
	artifactMediaType    string
	buildContractVersion string
	indexDigest          []byte
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
	if expectedArchitecture != "" &&
		string(runtimeDescriptor.Architecture) != expectedArchitecture {
		return api.WorkerRuntimeProgram{}, errors.New("Program architecture does not match Workspace")
	}
	runtimeWire, err := deployment.RuntimeDescriptorWire(runtimeDescriptor)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("encode Program Managed Runtime descriptor: %w", err)
	}
	artifact, err := projectCASObject(
		authority.artifactDigest,
		authority.artifactSizeBytes,
		authority.artifactMediaType,
		"Program Artifact",
	)
	if err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	if strings.TrimSpace(authority.buildContractVersion) == "" {
		return api.WorkerRuntimeProgram{}, errors.New("Program build contract version is required")
	}
	indexDigest, err := deployment.RuntimeDigestString(authority.indexDigest)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf(
			"Program index digest is invalid: %w",
			err,
		)
	}
	if _, err := cas.ObjectKey("", indexDigest); err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf(
			"Program index digest is invalid: %w",
			err,
		)
	}
	return api.WorkerRuntimeProgram{
		DeploymentID:         deploymentID,
		Runtime:              runtimeWire,
		Artifact:             artifact,
		BuildContractVersion: authority.buildContractVersion,
		IndexDigest:          indexDigest,
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
	artifactDigest string,
	artifactSizeBytes int64,
	artifactMediaType string,
	buildContractVersion string,
	indexDigest []byte,
) runtimeProgramAuthority {
	return runtimeProgramAuthority{
		deploymentID:         deploymentID,
		runtimeDigest:        runtimeDigest,
		artifactDigest:       artifactDigest,
		artifactSizeBytes:    artifactSizeBytes,
		artifactMediaType:    artifactMediaType,
		buildContractVersion: buildContractVersion,
		indexDigest:          indexDigest,
	}
}
