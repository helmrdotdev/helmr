package controlplane

import (
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestProjectCheckpointWorkspaceBaseKeepsOriginalArtifactSeparate(t *testing.T) {
	versionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := db.GetCheckpointWorkspaceBaseAuthorityRow{
		VersionID:       versionID,
		ParentVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ArtifactID:      pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ArtifactKind:    db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
		VersionKind:     db.WorkspaceVersionKindUser,
		ContentDigest:   digestWith("1"), LogicalSizeBytes: 5, EntryCount: 2,
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnershipGeneration:    2, WriterGeneration: 3,
		ArtifactRowKind:   db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
		ArtifactDigest:    pgvalue.Text(digestWith("2")),
		ArtifactSizeBytes: pgtype.Int8{Int64: 1024, Valid: true},
		ArtifactMediaType: pgvalue.Text(workspace.ArtifactMediaType),
	}
	projected, err := projectCheckpointWorkspaceBase(authority)
	if err != nil {
		t.Fatal(err)
	}
	want := workerapi.CheckpointWorkspaceBase{
		ArtifactDigest: digestWith("2"), ArtifactSizeBytes: 1024,
		ArtifactMediaType: workspace.ArtifactMediaType,
		ArtifactEncoding:  workspace.ArtifactEncoding,
		MountPath:         "/workspace",
	}
	if !workerapi.CheckpointWorkspaceBaseEqual(projected, want) {
		t.Fatalf("projected source base = %+v, want %+v", projected, want)
	}

	authority.ArtifactRowKind = db.NullArtifactKind{}
	if _, err := projectCheckpointWorkspaceBase(authority); err == nil {
		t.Fatal("partial checkpoint source Workspace Artifact authority was accepted")
	}
}

func TestProjectCheckpointWorkspaceBaseSupportsCanonicalEmptyBase(t *testing.T) {
	versionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projected, err := projectCheckpointWorkspaceBase(db.GetCheckpointWorkspaceBaseAuthorityRow{
		VersionID: versionID, VersionKind: db.WorkspaceVersionKindSystem,
		ContentDigest: workspace.CanonicalEmptyTreeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if projected != (workerapi.CheckpointWorkspaceBase{MountPath: "/workspace"}) {
		t.Fatalf("empty source base = %+v", projected)
	}
}

func TestValidateCheckpointWorkspaceBaseAuthorityRejectsCrossedFrontier(t *testing.T) {
	base := workerapi.CheckpointWorkspaceBase{
		ArtifactDigest: digestWith("4"), ArtifactSizeBytes: 1024,
		ArtifactMediaType: workspace.ArtifactMediaType,
		ArtifactEncoding:  workspace.ArtifactEncoding,
		MountPath:         "/workspace",
	}
	manifest := workerapi.CheckpointManifest{
		WorkspaceState: workerapi.CheckpointWorkspaceState{Base: base},
	}
	if err := validateCheckpointWorkspaceBaseAuthority(
		manifest,
		base,
	); err != nil {
		t.Fatal(err)
	}
	crossed := base
	crossed.ArtifactDigest = digestWith("5")
	if err := validateCheckpointWorkspaceBaseAuthority(
		manifest,
		crossed,
	); err != errStaleRunLeaseClaim {
		t.Fatalf("crossed source base error = %v, want stale authority", err)
	}
}
