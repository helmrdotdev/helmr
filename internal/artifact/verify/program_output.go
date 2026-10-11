package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// ProgramOutputFile proves that a finalized Program object has the exact
// bytes and executable closure described by output. It is the shared admission
// boundary for producer output; callers do not infer validity from a successful
// build process or from producer metadata.
func ProgramOutputFile(
	ctx context.Context,
	file *os.File,
	output artifact.ProgramOutput,
) error {
	if ctx == nil {
		return errors.New("program verification context is nil")
	}
	if file == nil {
		return errors.New("program verification file is nil")
	}
	if err := artifact.ValidateProgramOutput(output); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect Program object: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != output.Artifact.SizeBytes {
		return errors.New("program object does not exact-match its descriptor")
	}
	hash := sha256.New()
	if _, err := io.Copy(
		hash,
		io.NewSectionReader(file, 0, output.Artifact.SizeBytes),
	); err != nil {
		return fmt.Errorf("hash Program object: %w", err)
	}
	actualDigest := sha256sum.FormatDigest(hash.Sum(nil))
	if actualDigest != output.Artifact.Digest {
		return errors.New("program object digest does not match its descriptor")
	}
	reader, err := newSquashFSArtifactReader(
		ctx,
		file,
		output.Artifact.SizeBytes,
		artifact.RoleProgram,
	)
	if err != nil {
		return fmt.Errorf("open Program object: %w", err)
	}
	verified, err := verifyProgramArtifact(ctx, artifactInput{
		Digest:    output.Artifact.Digest,
		SizeBytes: output.Artifact.SizeBytes,
		MediaType: output.Artifact.MediaType,
		Reader:    reader,
	})
	if err != nil {
		return fmt.Errorf("verify Program object: %w", err)
	}
	verifiedIndex, err := artifact.CanonicalProgramMetadata(verified.Metadata())
	if err != nil {
		return err
	}
	expectedIndex, err := artifact.CanonicalProgramMetadata(output.Metadata)
	if err != nil {
		return err
	}
	if !bytes.Equal(verifiedIndex, expectedIndex) {
		return errors.New("program object index does not match its descriptor")
	}
	return nil
}
