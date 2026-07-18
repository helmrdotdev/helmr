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

func snapshotArtifact(
	ctx context.Context,
	directory string,
	role artifactRole,
	expected artifactSnapshotDescriptor,
	source io.Reader,
) (_ *artifactSnapshot, returnErr error) {
	if ctx == nil {
		return nil, errors.New("artifact snapshot context is nil")
	}
	if source == nil {
		return nil, errors.New("artifact snapshot source is nil")
	}
	if err := validateArtifactSnapshotDescriptor(role, expected); err != nil {
		return nil, err
	}

	directoryFD, err := unix.Open(
		directory,
		unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open artifact snapshot directory: %w", err)
	}
	defer func() {
		if directoryFD >= 0 {
			if err := unix.Close(directoryFD); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close artifact snapshot directory: %w", err))
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
		return nil, fmt.Errorf("create unnamed artifact snapshot: %w", err)
	}
	if err := unix.Close(directoryFD); err != nil {
		writerCloseErr := unix.Close(writerFD)
		directoryFD = -1
		if writerCloseErr != nil {
			return nil, errors.Join(
				fmt.Errorf("close artifact snapshot directory: %w", err),
				fmt.Errorf("close artifact snapshot writer: %w", writerCloseErr),
			)
		}
		return nil, fmt.Errorf("close artifact snapshot directory: %w", err)
	}
	directoryFD = -1

	writer := os.NewFile(uintptr(writerFD), "artifact snapshot")
	defer func() {
		if writer != nil {
			if err := writer.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close artifact snapshot writer: %w", err))
			}
		}
	}()

	if err := copyArtifactSnapshot(ctx, writer, source, expected); err != nil {
		return nil, err
	}
	verifier, err := openReadOnlyArtifactSnapshot(writer, expected.SizeBytes)
	if err != nil {
		return nil, fmt.Errorf("open artifact verifier snapshot: %w", err)
	}
	defer func() {
		if verifier != nil {
			if err := verifier.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close artifact verifier snapshot: %w", err))
			}
		}
	}()
	upload, err := openReadOnlyArtifactSnapshot(writer, expected.SizeBytes)
	if err != nil {
		return nil, fmt.Errorf("open artifact upload snapshot: %w", err)
	}
	defer func() {
		if upload != nil {
			if err := upload.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close artifact upload snapshot: %w", err))
			}
		}
	}()

	if err := writer.Chmod(0o400); err != nil {
		return nil, fmt.Errorf("remove artifact snapshot write permission: %w", err)
	}
	if err := writer.Close(); err != nil {
		writer = nil
		return nil, fmt.Errorf("close artifact snapshot writer: %w", err)
	}
	writer = nil
	snapshot := &artifactSnapshot{
		descriptor: expected,
		verifier:   verifier,
		upload:     upload,
	}
	verifier = nil
	upload = nil
	return snapshot, nil
}

func validateArtifactSnapshotDescriptor(
	role artifactRole,
	descriptor artifactSnapshotDescriptor,
) error {
	programDescriptor := ProgramDescriptor(descriptor)
	switch role {
	case codeArtifact:
		return validateProgramDescriptor(
			programDescriptor,
			"code",
			ProgramCodeArtifactMediaType,
			maxCodePhysicalBytes,
		)
	case dependencyArtifact:
		return validateProgramDescriptor(
			programDescriptor,
			"dependencies",
			ProgramDependencyArtifactMediaType,
			maxDependencyPhysicalBytes,
		)
	case runtimeArtifact:
		return validateProgramDescriptor(
			programDescriptor,
			"runtime",
			RuntimeArtifactMediaType,
			maxRuntimePhysicalBytes,
		)
	default:
		return fmt.Errorf("artifact snapshot role = %d", role)
	}
}

func copyArtifactSnapshot(
	ctx context.Context,
	destination *os.File,
	source io.Reader,
	expected artifactSnapshotDescriptor,
) error {
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var sizeBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy artifact snapshot: %w", err)
		}
		remaining := expected.SizeBytes + 1 - sizeBytes
		if remaining < int64(len(buffer)) {
			buffer = buffer[:remaining]
		}
		count, readErr := source.Read(buffer)
		if count < 0 || count > len(buffer) {
			return errors.New("copy artifact snapshot: source returned an invalid byte count")
		}
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return fmt.Errorf("hash artifact snapshot: %w", err)
			}
			if _, err := destination.Write(buffer[:count]); err != nil {
				return fmt.Errorf("write artifact snapshot: %w", err)
			}
			sizeBytes += int64(count)
			if sizeBytes > expected.SizeBytes {
				return fmt.Errorf(
					"artifact snapshot size exceeds expected %d bytes",
					expected.SizeBytes,
				)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read artifact snapshot source: %w", readErr)
			}
			break
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	if sizeBytes != expected.SizeBytes {
		return fmt.Errorf(
			"artifact snapshot size = %d, want %d",
			sizeBytes,
			expected.SizeBytes,
		)
	}
	actualDigest := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if actualDigest != expected.Digest {
		return fmt.Errorf(
			"artifact snapshot digest = %s, want %s",
			actualDigest,
			expected.Digest,
		)
	}
	return nil
}

func openReadOnlyArtifactSnapshot(writer *os.File, expectedSize int64) (*os.File, error) {
	var writerStat unix.Stat_t
	if err := unix.Fstat(int(writer.Fd()), &writerStat); err != nil {
		return nil, fmt.Errorf("stat writable artifact snapshot: %w", err)
	}
	if writerStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("artifact snapshot is not a regular file")
	}
	if writerStat.Size != expectedSize {
		return nil, fmt.Errorf(
			"writable artifact snapshot size = %d, want %d",
			writerStat.Size,
			expectedSize,
		)
	}
	path := "/proc/self/fd/" + strconv.FormatUint(uint64(writer.Fd()), 10)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("reopen artifact snapshot read-only: %w", err)
	}
	var readerStat unix.Stat_t
	if err := unix.Fstat(fd, &readerStat); err != nil {
		return nil, closeArtifactSnapshotFD(
			fd,
			fmt.Errorf("stat read-only artifact snapshot: %w", err),
		)
	}
	if writerStat.Dev != readerStat.Dev || writerStat.Ino != readerStat.Ino {
		return nil, closeArtifactSnapshotFD(
			fd,
			errors.New("read-only artifact snapshot changed inode identity"),
		)
	}
	if readerStat.Size != expectedSize {
		return nil, closeArtifactSnapshotFD(
			fd,
			fmt.Errorf(
				"read-only artifact snapshot size = %d, want %d",
				readerStat.Size,
				expectedSize,
			),
		)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, closeArtifactSnapshotFD(
			fd,
			fmt.Errorf("inspect read-only artifact snapshot: %w", err),
		)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, closeArtifactSnapshotFD(
			fd,
			errors.New("artifact snapshot descriptor is not read-only"),
		)
	}
	return os.NewFile(uintptr(fd), "artifact snapshot"), nil
}

func closeArtifactSnapshotFD(fd int, cause error) error {
	if err := unix.Close(fd); err != nil {
		return errors.Join(cause, fmt.Errorf("close artifact snapshot descriptor: %w", err))
	}
	return cause
}
