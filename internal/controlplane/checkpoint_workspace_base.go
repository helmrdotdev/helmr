package controlplane

import (
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func projectCheckpointWorkspaceBase(
	authority db.GetCheckpointWorkspaceBaseAuthorityRow,
) (workerapi.RuntimeRestoreWorkspaceBase, error) {
	versionID, err := requiredClaimUUIDString("checkpoint source Workspace base version ID", authority.VersionID)
	if err != nil {
		return workerapi.RuntimeRestoreWorkspaceBase{}, err
	}
	tree := workspace.TreeIdentity{
		Digest: authority.ContentDigest, SizeBytes: authority.LogicalSizeBytes,
		EntryCount: int(authority.EntryCount),
	}
	emptyShape := !authority.ParentVersionID.Valid && !authority.ArtifactID.Valid &&
		!authority.ArtifactKind.Valid && authority.VersionKind == db.WorkspaceVersionKindSystem &&
		!authority.SourceWorkspaceLeaseID.Valid && authority.OwnershipGeneration == 0 &&
		authority.WriterGeneration == 0 && !authority.ArtifactRowKind.Valid &&
		!authority.ArtifactDigest.Valid && !authority.ArtifactSizeBytes.Valid &&
		!authority.ArtifactMediaType.Valid
	if emptyShape {
		if _, err := workspace.EmptyResetTarget(versionID, tree); err != nil {
			return workerapi.RuntimeRestoreWorkspaceBase{}, fmt.Errorf("invalid empty checkpoint source Workspace base: %w", err)
		}
		return workerapi.RuntimeRestoreWorkspaceBase{
			VersionID: versionID,
			Base:      workerapi.CheckpointWorkspaceBase{MountPath: "/workspace"},
		}, nil
	}
	artifactShape := authority.ParentVersionID.Valid && authority.ArtifactID.Valid &&
		authority.ArtifactKind.Valid && authority.ArtifactKind.ArtifactKind == db.ArtifactKindWorkspaceVersion &&
		authority.VersionKind == db.WorkspaceVersionKindUser && authority.SourceWorkspaceLeaseID.Valid &&
		authority.OwnershipGeneration > 0 && authority.WriterGeneration > 0 &&
		authority.ArtifactRowKind.Valid && authority.ArtifactRowKind.ArtifactKind == db.ArtifactKindWorkspaceVersion &&
		authority.ArtifactDigest.Valid && authority.ArtifactSizeBytes.Valid && authority.ArtifactMediaType.Valid
	if !artifactShape {
		return workerapi.RuntimeRestoreWorkspaceBase{}, errors.New("checkpoint source Workspace base authority has an invalid version/artifact relation")
	}
	artifact := workspace.ArtifactIdentity{
		Digest: authority.ArtifactDigest.String, MediaType: authority.ArtifactMediaType.String,
		Encoding: workspace.ArtifactEncoding, SizeBytes: authority.ArtifactSizeBytes.Int64,
		EntryCount: int(authority.EntryCount),
	}
	if _, err := workspace.ArtifactResetTarget(versionID, tree, artifact); err != nil {
		return workerapi.RuntimeRestoreWorkspaceBase{}, fmt.Errorf("invalid checkpoint source Workspace base artifact: %w", err)
	}
	return workerapi.RuntimeRestoreWorkspaceBase{
		VersionID: versionID,
		Base: workerapi.CheckpointWorkspaceBase{
			ArtifactDigest: artifact.Digest, ArtifactSizeBytes: artifact.SizeBytes,
			ArtifactMediaType: artifact.MediaType, ArtifactEncoding: artifact.Encoding,
			MountPath: "/workspace",
		},
	}, nil
}

func validateCheckpointWorkspaceBaseAuthority(
	manifest workerapi.CheckpointManifest,
	source workerapi.RuntimeRestoreWorkspaceBase,
) error {
	if !workerapi.CheckpointWorkspaceBaseEqual(manifest.WorkspaceState.Base, source.Base) {
		return errStaleRunLeaseClaim
	}
	return nil
}
