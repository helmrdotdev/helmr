package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func projectDeploymentProgram(
	ctx context.Context,
	row db.GetDeploymentProgramAuthorityRow,
	platformStore cas.Reader,
) (workerapi.RuntimeProgram, error) {
	if _, err := requiredClaimUUIDString("program environment ID", row.EnvironmentID); err != nil {
		return workerapi.RuntimeProgram{}, err
	}
	return projectRuntimeProgram(
		ctx,
		runtimeProgramAuthorityFromDeployment(
			row.DeploymentID,
			row.BuildRuntimeDigest,
			row.ProgramArtifactDigest,
			row.ProgramArtifactSizeBytes,
			row.ProgramArtifactMediaType,
			row.BuildContract,
			row.ProgramIndexDigest,
		),
		"",
		platformStore,
	)
}

type runtimeProgramAuthority struct {
	deploymentID      pgtype.UUID
	runtimeDigest     []byte
	artifactDigest    string
	artifactSizeBytes int64
	artifactMediaType string
	buildContract     string
	indexDigest       []byte
}

func projectRuntimeProgram(
	ctx context.Context,
	authority runtimeProgramAuthority,
	expectedArchitecture string,
	platformStore cas.Reader,
) (workerapi.RuntimeProgram, error) {
	deploymentID, err := requiredClaimUUIDString("program deployment ID", authority.deploymentID)
	if err != nil {
		return workerapi.RuntimeProgram{}, err
	}
	if expectedArchitecture != "" && expectedArchitecture != string(deployment.ArchitectureX8664) {
		return workerapi.RuntimeProgram{}, errors.New("program architecture does not match workspace")
	}
	runtimeDigest, err := deployment.RuntimeDigestString(authority.runtimeDigest)
	if err != nil {
		return workerapi.RuntimeProgram{}, fmt.Errorf("decode program managed runtime digest: %w", err)
	}
	if platformStore == nil {
		return workerapi.RuntimeProgram{}, errors.New("platform artifact store is not configured")
	}
	runtimeObject, err := platformStore.Stat(ctx, runtimeDigest)
	if err != nil {
		return workerapi.RuntimeProgram{}, fmt.Errorf("stat program managed runtime: %w", err)
	}
	if runtimeObject.Digest != runtimeDigest ||
		runtimeObject.MediaType != deployment.RuntimeArtifactMediaType ||
		runtimeObject.SizeBytes < 1 {
		return workerapi.RuntimeProgram{}, errors.New("program managed runtime does not match its deployment pin")
	}
	artifact, err := projectCASObject(
		authority.artifactDigest,
		authority.artifactSizeBytes,
		authority.artifactMediaType,
		"program artifact",
	)
	if err != nil {
		return workerapi.RuntimeProgram{}, err
	}
	if strings.TrimSpace(authority.buildContract) == "" {
		return workerapi.RuntimeProgram{}, errors.New("program build contract version is required")
	}
	indexDigest, err := deployment.RuntimeDigestString(authority.indexDigest)
	if err != nil {
		return workerapi.RuntimeProgram{}, fmt.Errorf("program index digest is invalid: %w", err)
	}
	if _, err := cas.ObjectKey("", indexDigest); err != nil {
		return workerapi.RuntimeProgram{}, fmt.Errorf("program index digest is invalid: %w", err)
	}
	return workerapi.RuntimeProgram{
		DeploymentID: deploymentID,
		Runtime: workerapi.CASObject{
			Digest:    runtimeObject.Digest,
			SizeBytes: runtimeObject.SizeBytes,
			MediaType: runtimeObject.MediaType,
		},
		Artifact:      artifact,
		BuildContract: authority.buildContract,
		IndexDigest:   indexDigest,
	}, nil
}

func projectCASObject(digest string, sizeBytes int64, mediaType string, name string) (workerapi.CASObject, error) {
	digest = strings.TrimSpace(digest)
	mediaType = strings.TrimSpace(mediaType)
	if _, err := cas.ObjectKey("", digest); err != nil {
		return workerapi.CASObject{}, fmt.Errorf("%s digest is invalid: %w", name, err)
	}
	if sizeBytes < 1 {
		return workerapi.CASObject{}, fmt.Errorf("%s size is invalid", name)
	}
	if mediaType == "" {
		return workerapi.CASObject{}, fmt.Errorf("%s media type is required", name)
	}
	return workerapi.CASObject{Digest: digest, SizeBytes: sizeBytes, MediaType: mediaType}, nil
}

func runtimeProgramAuthorityFromDeployment(
	deploymentID pgtype.UUID,
	runtimeDigest []byte,
	artifactDigest string,
	artifactSizeBytes int64,
	artifactMediaType string,
	buildContract string,
	indexDigest []byte,
) runtimeProgramAuthority {
	return runtimeProgramAuthority{
		deploymentID:      deploymentID,
		runtimeDigest:     runtimeDigest,
		artifactDigest:    artifactDigest,
		artifactSizeBytes: artifactSizeBytes,
		artifactMediaType: artifactMediaType,
		buildContract:     buildContract,
		indexDigest:       indexDigest,
	}
}
