//go:build linux

package deployment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type artifactSnapshotOwner struct {
	UID int
	GID int
}

type artifactSnapshotIdentity struct {
	device uint64
	inode  uint64
	size   int64
	mode   uint32
	uid    uint32
	gid    uint32
}

type artifactSnapshotPlatform struct {
	directory       *os.File
	name            string
	identity        artifactSnapshotIdentity
	removeDirectory bool
}

func snapshotArtifact(
	ctx context.Context,
	directory string,
	role artifactRole,
	expected artifactSnapshotDescriptor,
	source io.Reader,
) (*artifactSnapshot, error) {
	if source == nil {
		return nil, errors.New("artifact snapshot source is nil")
	}
	if err := validateArtifactSnapshotDescriptor(role, expected); err != nil {
		return nil, err
	}
	leaseDirectory, err := os.MkdirTemp(directory, ".helmr-artifact-")
	if err != nil {
		return nil, fmt.Errorf("create artifact snapshot lease: %w", err)
	}
	if err := os.Chmod(leaseDirectory, 0o700); err != nil {
		return nil, errors.Join(
			fmt.Errorf("set artifact snapshot lease mode: %w", err),
			os.Remove(leaseDirectory),
		)
	}
	snapshot, err := produceArtifactSnapshot(
		ctx,
		leaseDirectory,
		role,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(destination *os.File) error {
			return copyArtifactSnapshot(ctx, destination, source, expected)
		},
	)
	if err != nil {
		return nil, errors.Join(err, os.Remove(leaseDirectory))
	}
	snapshot.platform.removeDirectory = true
	if snapshot.descriptor != expected {
		return nil, errors.Join(
			errors.New("artifact snapshot descriptor changed during sealing"),
			snapshot.Close(),
		)
	}
	return snapshot, nil
}

func produceArtifactSnapshot(
	ctx context.Context,
	directory string,
	role artifactRole,
	owner artifactSnapshotOwner,
	produce func(*os.File) error,
) (_ *artifactSnapshot, returnErr error) {
	if ctx == nil {
		return nil, errors.New("artifact snapshot context is nil")
	}
	if produce == nil {
		return nil, errors.New("artifact snapshot producer is nil")
	}
	if err := validateArtifactSnapshotOwner(owner); err != nil {
		return nil, err
	}
	mediaType, maxBytes, err := artifactSnapshotRole(role)
	if err != nil {
		return nil, err
	}

	directoryFD, err := unix.Open(
		directory,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open artifact snapshot directory: %w", err)
	}
	directoryFile := os.NewFile(uintptr(directoryFD), directory)
	defer func() {
		if directoryFile != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactSnapshotFile(directoryFile, "directory"),
			)
		}
	}()
	if err := validateArtifactSnapshotDirectory(directoryFile); err != nil {
		return nil, err
	}

	name, err := newArtifactSnapshotName()
	if err != nil {
		return nil, err
	}
	writerFD, err := unix.Openat(
		directoryFD,
		name,
		unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_RDWR|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("create artifact snapshot: %w", err)
	}
	writer := os.NewFile(uintptr(writerFD), name)
	defer func() {
		if writer != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactSnapshotFile(writer, "writer"),
			)
		}
		if name != "" && directoryFile != nil {
			returnErr = errors.Join(
				returnErr,
				removeArtifactSnapshot(directoryFile, name),
			)
		}
	}()
	if err := writer.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("set artifact snapshot writer mode: %w", err)
	}
	if err := produce(writer); err != nil {
		return nil, fmt.Errorf("produce artifact snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("produce artifact snapshot: %w", err)
	}

	before, err := inspectArtifactSnapshot(writer)
	if err != nil {
		return nil, err
	}
	if before.size < 1 || before.size > maxBytes {
		return nil, fmt.Errorf(
			"artifact snapshot size = %d, want 1..%d",
			before.size,
			maxBytes,
		)
	}
	digest, err := hashArtifactSnapshot(ctx, writer, before.size)
	if err != nil {
		return nil, err
	}
	descriptor := artifactSnapshotDescriptor{
		Digest:    digest,
		SizeBytes: before.size,
		MediaType: mediaType,
	}
	if err := validateArtifactSnapshotDescriptor(role, descriptor); err != nil {
		return nil, err
	}
	if err := writer.Sync(); err != nil {
		return nil, fmt.Errorf("sync artifact snapshot: %w", err)
	}
	if int(before.uid) != owner.UID || int(before.gid) != owner.GID {
		if err := writer.Chown(owner.UID, owner.GID); err != nil {
			return nil, fmt.Errorf("set artifact snapshot owner: %w", err)
		}
	}
	if err := writer.Chmod(0o400); err != nil {
		return nil, fmt.Errorf("seal artifact snapshot mode: %w", err)
	}
	if err := writer.Sync(); err != nil {
		return nil, fmt.Errorf("sync sealed artifact snapshot: %w", err)
	}
	sealed, err := inspectSealedArtifactSnapshot(writer, descriptor, owner)
	if err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		writer = nil
		return nil, fmt.Errorf("close artifact snapshot writer: %w", err)
	}
	writer = nil

	verifier, err := openReadOnlyArtifactSnapshot(
		directoryFile,
		name,
		sealed,
	)
	if err != nil {
		return nil, fmt.Errorf("open artifact verifier snapshot: %w", err)
	}
	defer func() {
		if verifier != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactSnapshotFile(verifier, "verifier"),
			)
		}
	}()
	upload, err := openReadOnlyArtifactSnapshot(
		directoryFile,
		name,
		sealed,
	)
	if err != nil {
		return nil, fmt.Errorf("open artifact upload snapshot: %w", err)
	}
	defer func() {
		if upload != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactSnapshotFile(upload, "upload"),
			)
		}
	}()
	reopenedDigest, err := hashArtifactSnapshot(ctx, verifier, descriptor.SizeBytes)
	if err != nil {
		return nil, err
	}
	if reopenedDigest != descriptor.Digest {
		return nil, fmt.Errorf(
			"reopened artifact snapshot digest = %s, want %s",
			reopenedDigest,
			descriptor.Digest,
		)
	}

	snapshot := &artifactSnapshot{
		descriptor: descriptor,
		platform: artifactSnapshotPlatform{
			directory: directoryFile,
			name:      name,
			identity:  sealed,
		},
		verifier: verifier,
		upload:   upload,
	}
	directoryFile = nil
	name = ""
	verifier = nil
	upload = nil
	return snapshot, nil
}

func validateArtifactSnapshotOwner(owner artifactSnapshotOwner) error {
	if owner.UID < 0 || uint64(owner.UID) > uint64(^uint32(0)) {
		return errors.New("artifact snapshot owner UID is invalid")
	}
	if owner.GID < 0 || uint64(owner.GID) > uint64(^uint32(0)) {
		return errors.New("artifact snapshot owner GID is invalid")
	}
	return nil
}

func artifactSnapshotRole(role artifactRole) (string, int64, error) {
	switch role {
	case codeArtifact:
		return ProgramCodeArtifactMediaType, maxCodePhysicalBytes, nil
	case dependencyArtifact:
		return ProgramDependencyArtifactMediaType, maxDependencyPhysicalBytes, nil
	case runtimeArtifact:
		return RuntimeArtifactMediaType, maxRuntimePhysicalBytes, nil
	case toolchainArtifact:
		return ToolchainMediaType, maxToolArtifactBytes, nil
	default:
		return "", 0, fmt.Errorf("artifact snapshot role = %d", role)
	}
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
	case toolchainArtifact:
		return validateProgramDescriptor(
			programDescriptor,
			"standard toolchain",
			ToolchainMediaType,
			maxToolArtifactBytes,
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

func inspectArtifactSnapshot(file *os.File) (artifactSnapshotIdentity, error) {
	if file == nil {
		return artifactSnapshotIdentity{}, errors.New("artifact snapshot file is nil")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return artifactSnapshotIdentity{}, fmt.Errorf("stat artifact snapshot: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return artifactSnapshotIdentity{}, errors.New("artifact snapshot is not a regular file")
	}
	return artifactSnapshotIdentity{
		device: uint64(stat.Dev),
		inode:  stat.Ino,
		size:   stat.Size,
		mode:   stat.Mode,
		uid:    stat.Uid,
		gid:    stat.Gid,
	}, nil
}

func validateArtifactSnapshotDirectory(directory *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return fmt.Errorf("stat artifact snapshot directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("artifact snapshot directory is not a directory")
	}
	if stat.Mode&0o7777 != 0o700 {
		return fmt.Errorf(
			"artifact snapshot directory mode = %#o, want 0700",
			stat.Mode&0o7777,
		)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		return fmt.Errorf(
			"artifact snapshot directory owner = %d:%d, want %d:%d",
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
	return nil
}

func inspectSealedArtifactSnapshot(
	file *os.File,
	expected artifactSnapshotDescriptor,
	owner artifactSnapshotOwner,
) (artifactSnapshotIdentity, error) {
	identity, err := inspectArtifactSnapshot(file)
	if err != nil {
		return artifactSnapshotIdentity{}, err
	}
	if identity.size != expected.SizeBytes {
		return artifactSnapshotIdentity{}, fmt.Errorf(
			"sealed artifact snapshot size = %d, want %d",
			identity.size,
			expected.SizeBytes,
		)
	}
	if identity.mode&0o7777 != 0o400 {
		return artifactSnapshotIdentity{}, fmt.Errorf(
			"sealed artifact snapshot mode = %#o, want 0400",
			identity.mode&0o7777,
		)
	}
	if identity.uid != uint32(owner.UID) || identity.gid != uint32(owner.GID) {
		return artifactSnapshotIdentity{}, fmt.Errorf(
			"sealed artifact snapshot owner = %d:%d, want %d:%d",
			identity.uid,
			identity.gid,
			owner.UID,
			owner.GID,
		)
	}
	return identity, nil
}

func openReadOnlyArtifactSnapshot(
	directory *os.File,
	name string,
	expected artifactSnapshotIdentity,
) (*os.File, error) {
	fd, err := unix.Openat(
		int(directory.Fd()),
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("reopen artifact snapshot read-only: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	identity, err := inspectArtifactSnapshot(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if identity != expected {
		return nil, errors.Join(
			errors.New("read-only artifact snapshot changed identity"),
			file.Close(),
		)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect read-only artifact snapshot: %w", err),
			file.Close(),
		)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.Join(
			errors.New("artifact snapshot descriptor is not read-only"),
			file.Close(),
		)
	}
	return file, nil
}

func hashArtifactSnapshot(
	ctx context.Context,
	file *os.File,
	sizeBytes int64,
) (string, error) {
	reader := io.NewSectionReader(file, 0, sizeBytes+1)
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var readBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("hash artifact snapshot: %w", err)
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return "", fmt.Errorf("hash artifact snapshot: %w", err)
			}
			readBytes += int64(count)
			if readBytes > sizeBytes {
				return "", fmt.Errorf(
					"artifact snapshot exceeds expected size %d",
					sizeBytes,
				)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", fmt.Errorf("read artifact snapshot: %w", readErr)
			}
			break
		}
		if count == 0 {
			return "", io.ErrNoProgress
		}
	}
	if readBytes != sizeBytes {
		return "", fmt.Errorf(
			"artifact snapshot size = %d, want %d",
			readBytes,
			sizeBytes,
		)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func newArtifactSnapshotName() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate artifact snapshot name: %w", err)
	}
	return "snapshot-" + hex.EncodeToString(random), nil
}

func removeArtifactSnapshot(directory *os.File, name string) error {
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove artifact snapshot: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync artifact snapshot directory: %w", err)
	}
	return nil
}

func (snapshot *artifactSnapshot) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) (returnErr error) {
	if snapshot == nil ||
		snapshot.platform.directory == nil ||
		snapshot.platform.name == "" ||
		snapshot.upload == nil {
		return errors.New("artifact snapshot is closed")
	}
	if filepath.Base(name) != name || name == "." || name == ".." || name == "" {
		return errors.New("artifact snapshot link name is invalid")
	}
	owner := artifactSnapshotOwner{UID: uid, GID: gid}
	if err := validateArtifactSnapshotOwner(owner); err != nil {
		return err
	}
	currentOwner := artifactSnapshotOwner{
		UID: int(snapshot.platform.identity.uid),
		GID: int(snapshot.platform.identity.gid),
	}
	before, err := inspectSealedArtifactSnapshot(
		snapshot.upload,
		snapshot.descriptor,
		currentOwner,
	)
	if err != nil {
		return err
	}
	if before != snapshot.platform.identity {
		return errors.New("artifact snapshot changed before link")
	}
	if currentOwner != owner {
		// A hard link retains inode ownership, so transfer the sealed inode before
		// the jailed process receives its mode-0400 link.
		if err := snapshot.upload.Chown(owner.UID, owner.GID); err != nil {
			return fmt.Errorf("transfer artifact snapshot owner: %w", err)
		}
		if err := snapshot.upload.Sync(); err != nil {
			return fmt.Errorf("sync artifact snapshot owner: %w", err)
		}
		transferred, err := inspectSealedArtifactSnapshot(
			snapshot.upload,
			snapshot.descriptor,
			owner,
		)
		if err != nil {
			return err
		}
		expected := before
		expected.uid = uint32(owner.UID)
		expected.gid = uint32(owner.GID)
		if transferred != expected {
			return errors.New("artifact snapshot changed during owner transfer")
		}
		snapshot.platform.identity = transferred
	}
	source, err := inspectArtifactSnapshotAt(
		int(snapshot.platform.directory.Fd()),
		snapshot.platform.name,
	)
	if err != nil {
		return fmt.Errorf("inspect artifact snapshot source link: %w", err)
	}
	if source != snapshot.platform.identity {
		return errors.New("artifact snapshot source link changed identity")
	}

	directoryFD, err := unix.Open(
		directory,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("open artifact snapshot link directory: %w", err)
	}
	destination := os.NewFile(uintptr(directoryFD), directory)
	defer func() {
		returnErr = errors.Join(
			returnErr,
			closeArtifactSnapshotFile(destination, "link directory"),
		)
	}()
	if err := unix.Linkat(
		int(snapshot.platform.directory.Fd()),
		snapshot.platform.name,
		directoryFD,
		name,
		0,
	); err != nil {
		return fmt.Errorf("link artifact snapshot: %w", err)
	}
	linked, err := inspectArtifactSnapshotAt(directoryFD, name)
	if err != nil {
		return cleanupArtifactSnapshotLink(
			destination,
			name,
			fmt.Errorf("inspect linked artifact snapshot: %w", err),
		)
	}
	after, err := inspectSealedArtifactSnapshot(
		snapshot.upload,
		snapshot.descriptor,
		owner,
	)
	if err != nil {
		return cleanupArtifactSnapshotLink(destination, name, err)
	}
	if linked != snapshot.platform.identity || after != snapshot.platform.identity {
		return cleanupArtifactSnapshotLink(
			destination,
			name,
			errors.New("artifact snapshot changed during link"),
		)
	}
	return nil
}

func inspectArtifactSnapshotAt(
	directoryFD int,
	name string,
) (artifactSnapshotIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(
		directoryFD,
		name,
		&stat,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return artifactSnapshotIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return artifactSnapshotIdentity{}, errors.New("artifact snapshot link is not regular")
	}
	return artifactSnapshotIdentity{
		device: uint64(stat.Dev),
		inode:  stat.Ino,
		size:   stat.Size,
		mode:   stat.Mode,
		uid:    stat.Uid,
		gid:    stat.Gid,
	}, nil
}

func cleanupArtifactSnapshotLink(
	directory *os.File,
	name string,
	cause error,
) error {
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("remove invalid artifact snapshot link: %w", err),
		)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("sync invalid artifact snapshot link cleanup: %w", err),
		)
	}
	return cause
}

func closeArtifactSnapshotPlatform(snapshot *artifactSnapshot) error {
	if snapshot == nil || snapshot.platform.directory == nil {
		return nil
	}
	var closeErr error
	if snapshot.platform.name != "" {
		closeErr = errors.Join(
			closeErr,
			removeArtifactSnapshot(
				snapshot.platform.directory,
				snapshot.platform.name,
			),
		)
		snapshot.platform.name = ""
	}
	directoryPath := snapshot.platform.directory.Name()
	closeErr = errors.Join(closeErr, snapshot.platform.directory.Close())
	snapshot.platform.directory = nil
	if snapshot.platform.removeDirectory {
		closeErr = errors.Join(closeErr, os.Remove(directoryPath))
		snapshot.platform.removeDirectory = false
	}
	snapshot.platform.identity = artifactSnapshotIdentity{}
	return closeErr
}

func validateArtifactSnapshotPlatform(snapshot *artifactSnapshot) error {
	if snapshot == nil ||
		snapshot.platform.directory == nil ||
		snapshot.platform.name == "" ||
		snapshot.upload == nil {
		return errors.New("artifact snapshot is closed")
	}
	identity, err := inspectArtifactSnapshot(snapshot.upload)
	if err != nil {
		return err
	}
	if identity != snapshot.platform.identity {
		return errors.New("artifact snapshot changed after sealing")
	}
	pathIdentity, err := inspectArtifactSnapshotAt(
		int(snapshot.platform.directory.Fd()),
		snapshot.platform.name,
	)
	if err != nil {
		return fmt.Errorf("inspect artifact snapshot path: %w", err)
	}
	if pathIdentity != snapshot.platform.identity {
		return errors.New("artifact snapshot path changed after sealing")
	}
	return nil
}

func closeArtifactSnapshotFile(file *os.File, role string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close artifact snapshot %s: %w", role, err)
	}
	return nil
}
