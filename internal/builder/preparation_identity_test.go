package builder

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestPreparationIdentityIncludesReadableMetadataOutsidePayload(t *testing.T) {
	root := t.TempDir()
	image, _ := writeSeedFixture(t, root)
	// Both artifacts have the same executable files, installed tree, config,
	// Computer seed and locator. Only the Agent's generated policy metadata changes.
	// Preparation can read that file, even when its own function is unchanged.
	path, body, metadata := writeVerifiedProgramFixtureWithMetadata(t, t.TempDir(), nil, image)
	path2, body2, metadata2 := writeVerifiedProgramFixtureWithMetadata(t, t.TempDir(), func(m *artifact.ProgramMetadata) {
		duration := int64(1234)
		m.Definitions[0].Agent.MaxTurnDurationMs = &duration
	}, image)
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body)), MediaType: artifact.ProgramArtifactMediaType}, Metadata: metadata}
	changed := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: sha256sum.DigestBytes(body2), SizeBytes: int64(len(body2)), MediaType: artifact.ProgramArtifactMediaType}, Metadata: metadata2}
	if err := verifyFinalObject(t.Context(), path, bundle.Object(output.Artifact), output); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalObject(t.Context(), path2, bundle.Object(changed.Artifact), changed); err != nil {
		t.Fatal(err)
	}
	// Cross-pairing producer metadata must fail before any registration accepts it.
	if err := verifyFinalObject(t.Context(), path, bundle.Object(output.Artifact), artifact.ProgramOutput{Artifact: output.Artifact, Metadata: changed.Metadata}); err == nil {
		t.Fatal("unbound metadata admitted")
	}
	first, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		t.Fatal(err)
	}
	second, err := artifact.BuildComputerPreparationSpecs(changed)
	if err != nil {
		t.Fatal(err)
	}
	a, err := artifact.ComputerPreparationSpecDigest(first[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := artifact.ComputerPreparationSpecDigest(second[0])
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("readable generated metadata omitted from preparation identity")
	}
	if output.Metadata.Definitions[1].Kind != definition.KindComputer {
		t.Fatal("fixture Computer selection changed")
	}
}
