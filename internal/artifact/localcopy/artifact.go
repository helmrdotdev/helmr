// Package localcopy owns verified local copies of Program and Node runtime
// artifacts and the file descriptors that read, link and upload them.
package localcopy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type artifactSpec struct {
	label     string
	mediaType string
	maxBytes  int64
}

type Artifact struct {
	descriptor cas.Descriptor
	platform   artifactPlatform
	verifier   *os.File
	upload     *os.File
}

type Program struct {
	content *Artifact
}

func (local *Program) VerifierFile() (*os.File, error) {
	if local == nil || local.content == nil {
		return nil, errors.New("local artifact copy is closed")
	}
	return local.content.VerifierFile()
}

func (local *Program) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if local == nil || local.content == nil {
		return errors.New("local artifact copy is closed")
	}
	return local.content.LinkInto(directory, name, uid, gid)
}

func (local *Program) Close() error {
	if local == nil || local.content == nil {
		return nil
	}
	err := local.content.Close()
	local.content = nil
	return err
}

func artifactSpecForRole(role artifact.Role) (artifactSpec, error) {
	label, labelOK := role.Label()
	mediaType, mediaTypeOK := role.MediaType()
	maxBytes, maxBytesOK := role.PhysicalLimit()
	if !labelOK || !mediaTypeOK || !maxBytesOK {
		return artifactSpec{}, fmt.Errorf("local artifact copy role = %d", role)
	}
	return artifactSpec{label: label, mediaType: mediaType, maxBytes: maxBytes}, nil
}

func validateArtifactCopySpec(spec artifactSpec) error {
	if spec.label == "" {
		return errors.New("local artifact copy label is empty")
	}
	if spec.mediaType == "" {
		return errors.New("local artifact copy media type is empty")
	}
	if spec.maxBytes < 1 {
		return errors.New("local artifact copy byte limit is invalid")
	}
	return nil
}

func validateArtifactCopyDescriptor(
	spec artifactSpec,
	descriptor cas.Descriptor,
) error {
	if err := validateArtifactCopySpec(spec); err != nil {
		return err
	}
	if !sha256sum.ValidDigest(descriptor.Digest) {
		return fmt.Errorf(
			"%s artifact digest is not a lowercase SHA-256 digest",
			spec.label,
		)
	}
	if descriptor.SizeBytes < 1 || descriptor.SizeBytes > spec.maxBytes {
		return fmt.Errorf(
			"%s artifact size is outside [1,%d]",
			spec.label,
			spec.maxBytes,
		)
	}
	if descriptor.MediaType != spec.mediaType {
		return fmt.Errorf(
			"%s artifact media type = %q, want %q",
			spec.label,
			descriptor.MediaType,
			spec.mediaType,
		)
	}
	return nil
}

// Descriptor returns the digest, size and media type the local copy was sealed
// for.
func (local *Artifact) Descriptor() cas.Descriptor {
	return cas.Descriptor{
		Digest:    local.descriptor.Digest,
		SizeBytes: local.descriptor.SizeBytes,
		MediaType: local.descriptor.MediaType,
	}
}

func (local *Artifact) VerifierFile() (*os.File, error) {
	if local == nil || local.verifier == nil {
		return nil, errors.New("local artifact copy is closed")
	}
	return local.verifier, nil
}

func (local *Artifact) UploadReader(ctx context.Context) (io.Reader, error) {
	if ctx == nil {
		return nil, errors.New("local artifact copy upload context is nil")
	}
	if local == nil || local.upload == nil {
		return nil, errors.New("local artifact copy is closed")
	}
	if err := validateArtifactCopyPlatform(local); err != nil {
		return nil, err
	}
	return &uploadReader{
		ctx:         ctx,
		source:      io.NewSectionReader(local.upload, 0, local.descriptor.SizeBytes+1),
		expected:    local.descriptor,
		digest:      sha256.New(),
		sizeBytes:   0,
		terminalErr: nil,
		finalCheck: func() error {
			return validateArtifactCopyPlatform(local)
		},
	}, nil
}

func (local *Artifact) Close() error {
	if local == nil {
		return nil
	}
	var closeErr error
	if local.verifier != nil {
		closeErr = errors.Join(closeErr, local.verifier.Close())
		local.verifier = nil
	}
	if local.upload != nil {
		closeErr = errors.Join(closeErr, local.upload.Close())
		local.upload = nil
	}
	closeErr = errors.Join(closeErr, closeArtifactCopyPlatform(local))
	return closeErr
}

type uploadReader struct {
	ctx         context.Context
	source      *io.SectionReader
	expected    cas.Descriptor
	digest      hash.Hash
	sizeBytes   int64
	complete    bool
	terminalErr error
	finalCheck  func() error
}

func (reader *uploadReader) Read(buffer []byte) (int, error) {
	if reader.terminalErr != nil {
		return 0, reader.terminalErr
	}
	if reader.complete {
		return 0, io.EOF
	}
	if err := reader.ctx.Err(); err != nil {
		reader.terminalErr = fmt.Errorf("read local artifact copy for upload: %w", err)
		return 0, reader.terminalErr
	}

	count, readErr := reader.source.Read(buffer)
	if count > 0 {
		if _, err := reader.digest.Write(buffer[:count]); err != nil {
			reader.terminalErr = fmt.Errorf("hash local artifact copy upload: %w", err)
			return count, reader.terminalErr
		}
		reader.sizeBytes += int64(count)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		reader.terminalErr = fmt.Errorf("read local artifact copy for upload: %w", readErr)
		return count, reader.terminalErr
	}
	if errors.Is(readErr, io.EOF) {
		if reader.sizeBytes != reader.expected.SizeBytes {
			reader.terminalErr = fmt.Errorf(
				"local artifact copy upload size = %d, want %d",
				reader.sizeBytes,
				reader.expected.SizeBytes,
			)
			return count, reader.terminalErr
		}
		digest := sha256sum.FormatDigest(reader.digest.Sum(nil))
		if digest != reader.expected.Digest {
			reader.terminalErr = fmt.Errorf(
				"local artifact copy upload digest = %s, want %s",
				digest,
				reader.expected.Digest,
			)
			return count, reader.terminalErr
		}
		if err := reader.finalCheck(); err != nil {
			reader.terminalErr = err
			return count, reader.terminalErr
		}
		reader.complete = true
		return count, io.EOF
	}
	return count, nil
}
