package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestCheckpointWorkspaceBasePreservesCanonicalEmptyAuthority(t *testing.T) {
	target, err := workspace.EmptyResetTarget(
		"version-empty",
		workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
	)
	if err != nil {
		t.Fatal(err)
	}

	base, err := checkpointWorkspaceBase(target)
	if err != nil {
		t.Fatal(err)
	}
	if base.MountPath != "/workspace" || base.ArtifactDigest != "" ||
		base.ArtifactSizeBytes != 0 || base.ArtifactMediaType != "" || base.ArtifactEncoding != "" {
		t.Fatalf("empty checkpoint workspace base = %+v", base)
	}
}

func TestCheckpointWorkspaceBasePreservesCanonicalArtifactAuthority(t *testing.T) {
	artifact := workspace.ArtifactIdentity{
		Digest:     sha256sum.DigestBytes([]byte("logical workspace artifact")),
		MediaType:  workspace.ArtifactMediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  4096,
		EntryCount: 7,
	}
	target, err := workspace.ArtifactResetTarget(
		"version-artifact",
		workspace.TreeIdentity{
			Digest:     sha256sum.DigestBytes([]byte("logical workspace tree")),
			SizeBytes:  8192,
			EntryCount: 7,
		},
		artifact,
	)
	if err != nil {
		t.Fatal(err)
	}

	base, err := checkpointWorkspaceBase(target)
	if err != nil {
		t.Fatal(err)
	}
	if base.MountPath != "/workspace" || base.ArtifactDigest != artifact.Digest ||
		base.ArtifactSizeBytes != artifact.SizeBytes || base.ArtifactMediaType != artifact.MediaType ||
		base.ArtifactEncoding != artifact.Encoding {
		t.Fatalf("artifact checkpoint workspace base = %+v", base)
	}
}
