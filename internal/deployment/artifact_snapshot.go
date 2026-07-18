package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
)

type artifactSnapshotDescriptor ProgramDescriptor

type artifactSnapshot struct {
	descriptor artifactSnapshotDescriptor
	verifier   *os.File
	upload     *os.File
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
	return &artifactSnapshotReader{
		ctx:         ctx,
		source:      io.NewSectionReader(snapshot.upload, 0, snapshot.descriptor.SizeBytes+1),
		expected:    snapshot.descriptor,
		digest:      sha256.New(),
		sizeBytes:   0,
		terminalErr: nil,
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
		digest := "sha256:" + hex.EncodeToString(reader.digest.Sum(nil))
		if digest != reader.expected.Digest {
			reader.terminalErr = fmt.Errorf(
				"artifact snapshot upload digest = %s, want %s",
				digest,
				reader.expected.Digest,
			)
			return count, reader.terminalErr
		}
		reader.complete = true
		return count, io.EOF
	}
	return count, nil
}
