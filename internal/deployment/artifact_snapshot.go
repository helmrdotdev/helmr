package deployment

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type artifactSnapshotDescriptor ProgramDescriptor

type artifactSnapshotSpec struct {
	label     string
	mediaType string
	maxBytes  int64
}

type artifactSnapshot struct {
	descriptor artifactSnapshotDescriptor
	platform   artifactSnapshotPlatform
	verifier   *os.File
	upload     *os.File
}

type ArtifactSnapshot struct {
	content *artifactSnapshot
}

func (snapshot *ArtifactSnapshot) verifier() (*os.File, error) {
	if snapshot == nil || snapshot.content == nil {
		return nil, errors.New("artifact snapshot is closed")
	}
	return snapshot.content.verifierFile()
}

func (snapshot *ArtifactSnapshot) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if snapshot == nil || snapshot.content == nil {
		return errors.New("artifact snapshot is closed")
	}
	return snapshot.content.LinkInto(directory, name, uid, gid)
}

func (snapshot *ArtifactSnapshot) Close() error {
	if snapshot == nil || snapshot.content == nil {
		return nil
	}
	err := snapshot.content.Close()
	snapshot.content = nil
	return err
}

func artifactSnapshotSpecForRole(role artifactRole) (artifactSnapshotSpec, error) {
	switch role {
	case programArtifact:
		return artifactSnapshotSpec{
			label:     "program",
			mediaType: ProgramArtifactMediaType,
			maxBytes:  maxProgramPhysicalBytes,
		}, nil
	case runtimeArtifact:
		return artifactSnapshotSpec{
			label:     "runtime",
			mediaType: RuntimeArtifactMediaType,
			maxBytes:  maxRuntimePhysicalBytes,
		}, nil
	case buildTreeArtifact:
		return artifactSnapshotSpec{
			label:     "build tree",
			mediaType: buildTreeSnapshotMediaType,
			maxBytes:  maxBuildTreePhysicalBytes,
		}, nil
	default:
		return artifactSnapshotSpec{}, fmt.Errorf("artifact snapshot role = %d", role)
	}
}

func validateArtifactSnapshotSpec(spec artifactSnapshotSpec) error {
	if spec.label == "" {
		return errors.New("artifact snapshot label is empty")
	}
	if spec.mediaType == "" {
		return errors.New("artifact snapshot media type is empty")
	}
	if spec.maxBytes < 1 {
		return errors.New("artifact snapshot byte limit is invalid")
	}
	return nil
}

func validateArtifactSnapshotDescriptor(
	spec artifactSnapshotSpec,
	descriptor artifactSnapshotDescriptor,
) error {
	if err := validateArtifactSnapshotSpec(spec); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(descriptor.Digest) {
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

func (snapshot *artifactSnapshot) verifierFile() (*os.File, error) {
	if snapshot == nil || snapshot.verifier == nil {
		return nil, errors.New("artifact snapshot is closed")
	}
	return snapshot.verifier, nil
}

func (snapshot *artifactSnapshot) uploadReader(ctx context.Context) (io.Reader, error) {
	if ctx == nil {
		return nil, errors.New("artifact snapshot upload context is nil")
	}
	if snapshot == nil || snapshot.upload == nil {
		return nil, errors.New("artifact snapshot is closed")
	}
	if err := validateArtifactSnapshotPlatform(snapshot); err != nil {
		return nil, err
	}
	return &artifactSnapshotReader{
		ctx:         ctx,
		source:      io.NewSectionReader(snapshot.upload, 0, snapshot.descriptor.SizeBytes+1),
		expected:    snapshot.descriptor,
		digest:      sha256.New(),
		sizeBytes:   0,
		terminalErr: nil,
		finalCheck: func() error {
			return validateArtifactSnapshotPlatform(snapshot)
		},
	}, nil
}

func (snapshot *artifactSnapshot) Close() error {
	if snapshot == nil {
		return nil
	}
	var closeErr error
	if snapshot.verifier != nil {
		closeErr = errors.Join(closeErr, snapshot.verifier.Close())
		snapshot.verifier = nil
	}
	if snapshot.upload != nil {
		closeErr = errors.Join(closeErr, snapshot.upload.Close())
		snapshot.upload = nil
	}
	closeErr = errors.Join(closeErr, closeArtifactSnapshotPlatform(snapshot))
	return closeErr
}

type artifactSnapshotReader struct {
	ctx         context.Context
	source      *io.SectionReader
	expected    artifactSnapshotDescriptor
	digest      hash.Hash
	sizeBytes   int64
	complete    bool
	terminalErr error
	finalCheck  func() error
}

func (reader *artifactSnapshotReader) Read(buffer []byte) (int, error) {
	if reader.terminalErr != nil {
		return 0, reader.terminalErr
	}
	if reader.complete {
		return 0, io.EOF
	}
	if err := reader.ctx.Err(); err != nil {
		reader.terminalErr = fmt.Errorf("read artifact snapshot for upload: %w", err)
		return 0, reader.terminalErr
	}

	count, readErr := reader.source.Read(buffer)
	if count > 0 {
		if _, err := reader.digest.Write(buffer[:count]); err != nil {
			reader.terminalErr = fmt.Errorf("hash artifact snapshot upload: %w", err)
			return count, reader.terminalErr
		}
		reader.sizeBytes += int64(count)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		reader.terminalErr = fmt.Errorf("read artifact snapshot for upload: %w", readErr)
		return count, reader.terminalErr
	}
	if errors.Is(readErr, io.EOF) {
		if reader.sizeBytes != reader.expected.SizeBytes {
			reader.terminalErr = fmt.Errorf(
				"artifact snapshot upload size = %d, want %d",
				reader.sizeBytes,
				reader.expected.SizeBytes,
			)
			return count, reader.terminalErr
		}
		digest := sha256sum.FormatDigest(reader.digest.Sum(nil))
		if digest != reader.expected.Digest {
			reader.terminalErr = fmt.Errorf(
				"artifact snapshot upload digest = %s, want %s",
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
