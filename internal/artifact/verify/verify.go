// Package verify admits untrusted Program and Node runtime artifacts. It
// decodes their SquashFS images and runs deep verification in a sandboxed
// verifier child re-executed from the current binary.
package verify

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type artifactInput struct {
	Digest    string
	SizeBytes int64
	MediaType string
	Reader    artifact.Reader
}

type verifiedProgram struct {
	index artifact.ProgramIndex
}

func (program *verifiedProgram) Index() artifact.ProgramIndex {
	return program.index.Clone()
}

func verifyProgramArtifact(ctx context.Context, input artifactInput) (*verifiedProgram, error) {
	if err := validateArtifactDescriptor(input, artifact.RoleProgram); err != nil {
		return nil, err
	}

	inspected, err := artifact.Inspect(
		ctx,
		input.Reader,
		artifact.RoleProgram,
		input.SizeBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("program artifact: %w", err)
	}

	verifier := programVerifier{
		ctx:      ctx,
		artifact: inspected,
	}
	if err := verifier.verify(); err != nil {
		return nil, err
	}
	return &verifiedProgram{
		index: verifier.index,
	}, nil
}

func validateArtifactDescriptor(
	input artifactInput,
	role artifact.Role,
) error {
	if role != artifact.RoleProgram && role != artifact.RoleRuntime {
		return fmt.Errorf("artifact role = %d", role)
	}
	label, _ := role.Label()
	mediaType, _ := role.MediaType()
	maxPhysicalBytes, _ := role.PhysicalLimit()
	if !sha256sum.ValidDigest(input.Digest) {
		return fmt.Errorf("%s artifact digest is not a lowercase SHA-256 digest", label)
	}
	if input.SizeBytes < 1 || input.SizeBytes > maxPhysicalBytes {
		return fmt.Errorf(
			"%s artifact size is outside [1,%d]",
			label,
			maxPhysicalBytes,
		)
	}
	if input.MediaType != mediaType {
		return fmt.Errorf("%s artifact media type = %q, want %q", label, input.MediaType, mediaType)
	}
	if input.Reader == nil {
		return fmt.Errorf("%s artifact reader is nil", label)
	}
	return nil
}
