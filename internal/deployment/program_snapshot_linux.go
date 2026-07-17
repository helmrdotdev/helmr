//go:build linux

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func snapshotProgram(
	ctx context.Context,
	directory string,
	role artifactRole,
	expected ProgramDescriptor,
	source io.Reader,
) (_ *programSnapshot, returnErr error) {
	if ctx == nil {
		return nil, errors.New("program snapshot context is nil")
	}
	if source == nil {
		return nil, errors.New("program snapshot source is nil")
	}
	if err := validateProgramSnapshotDescriptor(role, expected); err != nil {
		return nil, err
	}

	directoryFD, err := unix.Open(
		directory,
		unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open program snapshot directory: %w", err)
	}
	defer func() {
		if directoryFD >= 0 {
			if err := unix.Close(directoryFD); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close program snapshot directory: %w", err))
			}
		}
	}()

	writerFD, err := unix.Openat(
		directoryFD,
		".",
		unix.O_TMPFILE|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("create unnamed program snapshot: %w", err)
	}
	if err := unix.Close(directoryFD); err != nil {
		writerCloseErr := unix.Close(writerFD)
		directoryFD = -1
		if writerCloseErr != nil {
			return nil, errors.Join(
				fmt.Errorf("close program snapshot directory: %w", err),
				fmt.Errorf("close program snapshot writer: %w", writerCloseErr),
			)
		}
		return nil, fmt.Errorf("close program snapshot directory: %w", err)
	}
	directoryFD = -1

	writer := os.NewFile(uintptr(writerFD), "Program snapshot")
	defer func() {
		if writer != nil {
			if err := writer.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close program snapshot writer: %w", err))
			}
		}
	}()

	if err := copyProgramSnapshot(ctx, writer, source, expected); err != nil {
		return nil, err
	}
	verifier, err := openReadOnlyProgramSnapshot(writer, expected.SizeBytes)
	if err != nil {
		return nil, fmt.Errorf("open program verifier snapshot: %w", err)
	}
	defer func() {
		if verifier != nil {
			if err := verifier.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close program verifier snapshot: %w", err))
			}
		}
	}()
	upload, err := openReadOnlyProgramSnapshot(writer, expected.SizeBytes)
	if err != nil {
		return nil, fmt.Errorf("open program upload snapshot: %w", err)
	}
	defer func() {
		if upload != nil {
			if err := upload.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close program upload snapshot: %w", err))
			}
		}
	}()

	if err := writer.Close(); err != nil {
		writer = nil
		return nil, fmt.Errorf("close program snapshot writer: %w", err)
	}
	writer = nil
	snapshot := &programSnapshot{
		descriptor: expected,
		verifier:   verifier,
		upload:     upload,
	}
	verifier = nil
	upload = nil
	return snapshot, nil
}

func validateProgramSnapshotDescriptor(role artifactRole, descriptor ProgramDescriptor) error {
	switch role {
	case codeArtifact:
		return validateProgramDescriptor(
			descriptor,
			"code",
			ProgramCodeArtifactMediaType,
			maxCodePhysicalBytes,
		)
	case dependencyArtifact:
		return validateProgramDescriptor(
			descriptor,
			"dependencies",
			ProgramDependencyArtifactMediaType,
			maxDependencyPhysicalBytes,
		)
	default:
		return fmt.Errorf("program snapshot Artifact role = %d", role)
	}
}

func copyProgramSnapshot(
	ctx context.Context,
	destination *os.File,
	source io.Reader,
	expected ProgramDescriptor,
) error {
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var sizeBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy program snapshot: %w", err)
		}
		remaining := expected.SizeBytes + 1 - sizeBytes
		if remaining < int64(len(buffer)) {
			buffer = buffer[:remaining]
		}
		count, readErr := source.Read(buffer)
		if count < 0 || count > len(buffer) {
			return errors.New("copy program snapshot: source returned an invalid byte count")
		}
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return fmt.Errorf("hash program snapshot: %w", err)
			}
			if _, err := destination.Write(buffer[:count]); err != nil {
				return fmt.Errorf("write program snapshot: %w", err)
			}
			sizeBytes += int64(count)
			if sizeBytes > expected.SizeBytes {
				return fmt.Errorf(
					"program snapshot size exceeds expected %d bytes",
					expected.SizeBytes,
				)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read program snapshot source: %w", readErr)
			}
			break
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	if sizeBytes != expected.SizeBytes {
		return fmt.Errorf(
			"program snapshot size = %d, want %d",
			sizeBytes,
			expected.SizeBytes,
		)
	}
	actualDigest := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if actualDigest != expected.Digest {
		return fmt.Errorf(
			"program snapshot digest = %s, want %s",
			actualDigest,
			expected.Digest,
		)
	}
	return nil
}

func openReadOnlyProgramSnapshot(writer *os.File, expectedSize int64) (*os.File, error) {
	var writerStat unix.Stat_t
	if err := unix.Fstat(int(writer.Fd()), &writerStat); err != nil {
		return nil, fmt.Errorf("stat writable program snapshot: %w", err)
	}
	if writerStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("program snapshot is not a regular file")
	}
	if writerStat.Size != expectedSize {
		return nil, fmt.Errorf(
			"writable program snapshot size = %d, want %d",
			writerStat.Size,
			expectedSize,
		)
	}
	path := "/proc/self/fd/" + strconv.FormatUint(uint64(writer.Fd()), 10)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("reopen program snapshot read-only: %w", err)
	}
	var readerStat unix.Stat_t
	if err := unix.Fstat(fd, &readerStat); err != nil {
		return nil, closeProgramSnapshotFD(
			fd,
			fmt.Errorf("stat read-only program snapshot: %w", err),
		)
	}
	if writerStat.Dev != readerStat.Dev || writerStat.Ino != readerStat.Ino {
		return nil, closeProgramSnapshotFD(
			fd,
			errors.New("read-only program snapshot changed inode identity"),
		)
	}
	if readerStat.Size != expectedSize {
		return nil, closeProgramSnapshotFD(
			fd,
			fmt.Errorf(
				"read-only program snapshot size = %d, want %d",
				readerStat.Size,
				expectedSize,
			),
		)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, closeProgramSnapshotFD(
			fd,
			fmt.Errorf("inspect read-only program snapshot: %w", err),
		)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, closeProgramSnapshotFD(
			fd,
			errors.New("program snapshot descriptor is not read-only"),
		)
	}
	return os.NewFile(uintptr(fd), "Program snapshot"), nil
}

func closeProgramSnapshotFD(fd int, cause error) error {
	if err := unix.Close(fd); err != nil {
		return errors.Join(cause, fmt.Errorf("close program snapshot descriptor: %w", err))
	}
	return cause
}
