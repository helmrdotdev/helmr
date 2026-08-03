package control

import (
	"context"
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
	ctx context.Context,
	row db.GetDeploymentProgramAuthorityRow,
	platformStore cas.Reader,
) (api.WorkerRuntimeProgram, error) {
	if _, err := requiredClaimUUIDString("Program environment ID", row.EnvironmentID); err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	return projectRuntimeProgram(
		ctx,
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
		platformStore,
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
	ctx context.Context,
	authority runtimeProgramAuthority,
	expectedArchitecture string,
	platformStore cas.Reader,
) (api.WorkerRuntimeProgram, error) {
	deploymentID, err := requiredClaimUUIDString("Program Deployment ID", authority.deploymentID)
	if err != nil {
		return api.WorkerRuntimeProgram{}, err
	}
	if expectedArchitecture != "" && expectedArchitecture != string(deployment.ArchitectureX8664) {
		return api.WorkerRuntimeProgram{}, errors.New("Program architecture does not match Workspace")
	}
	runtimeDigest, err := deployment.RuntimeDigestString(authority.runtimeDigest)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("decode Program Managed Runtime digest: %w", err)
	}
	if platformStore == nil {
		return api.WorkerRuntimeProgram{}, errors.New("Platform Artifact store is not configured")
	}
	runtimeObject, err := platformStore.Stat(ctx, runtimeDigest)
	if err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("stat Program Managed Runtime: %w", err)
	}
	if runtimeObject.Digest != runtimeDigest ||
		runtimeObject.MediaType != deployment.RuntimeArtifactMediaType ||
		runtimeObject.SizeBytes < 1 {
		return api.WorkerRuntimeProgram{}, errors.New("Program Managed Runtime does not match its Deployment pin")
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
		return api.WorkerRuntimeProgram{}, fmt.Errorf("Program index digest is invalid: %w", err)
	}
	if _, err := cas.ObjectKey("", indexDigest); err != nil {
		return api.WorkerRuntimeProgram{}, fmt.Errorf("Program index digest is invalid: %w", err)
	}
	return api.WorkerRuntimeProgram{
		DeploymentID: deploymentID,
		Runtime: api.CASObject{
			Digest:    runtimeObject.Digest,
			SizeBytes: runtimeObject.SizeBytes,
			MediaType: runtimeObject.MediaType,
		},
		Artifact:             artifact,
		BuildContractVersion: authority.buildContractVersion,
		IndexDigest:          indexDigest,
	}, nil
}

func projectCASObject(digest string, sizeBytes int64, mediaType string, name string) (api.CASObject, error) {
	digest = strings.TrimSpace(digest)
	mediaType = strings.TrimSpace(mediaType)
	if _, err := cas.ObjectKey("", digest); err != nil {
		return api.CASObject{}, fmt.Errorf("%s digest is invalid: %w", name, err)
	}
	if sizeBytes < 1 {
		return api.CASObject{}, fmt.Errorf("%s size is invalid", name)
	}
	if mediaType == "" {
		return api.CASObject{}, fmt.Errorf("%s media type is required", name)
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
