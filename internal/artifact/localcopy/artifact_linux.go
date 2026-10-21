//go:build linux

package localcopy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/cas"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sys/unix"
)

type Owner struct {
	UID int
	GID int
}

type fileIdentity struct {
	device uint64
	inode  uint64
	size   int64
	mode   uint32
	uid    uint32
	gid    uint32
}

type artifactPlatform struct {
	directory       *os.File
	name            string
	identity        fileIdentity
	removeDirectory bool
}

func copyArtifact(
	ctx context.Context,
	directory string,
	role artifact.Role,
	expected cas.Descriptor,
	source io.Reader,
) (*Artifact, error) {
	spec, err := artifactSpecForRole(role)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("local artifact copy source is nil")
	}
	if err := validateArtifactCopyDescriptor(spec, expected); err != nil {
		return nil, err
	}
	leaseDirectory, err := os.MkdirTemp(directory, ".helmr-artifact-")
	if err != nil {
		return nil, fmt.Errorf("create local artifact copy lease: %w", err)
	}
	if err := os.Chmod(leaseDirectory, 0o700); err != nil {
		return nil, errors.Join(
			fmt.Errorf("set local artifact copy lease mode: %w", err),
			os.Remove(leaseDirectory),
		)
	}
	// Copies are recreated from their authoritative source for each use. Their
	// random lease is never a crash-recovery record, so synchronous writeback
	// adds latency without establishing a retained durability boundary. Keep
	// the sealed read-only descriptors and final content verification.
	local, err := produceArtifactCopyWithSpec(
		ctx,
		leaseDirectory,
		spec,
		Owner{UID: os.Geteuid(), GID: os.Getegid()},
		false,
		nil,
		func(destination *os.File) error {
			return copyArtifactCopy(ctx, destination, source, expected)
		},
	)
	if err != nil {
		return nil, errors.Join(err, os.Remove(leaseDirectory))
	}
	local.platform.removeDirectory = true
	if local.descriptor != expected {
		return nil, errors.Join(
			errors.New("local artifact copy descriptor changed during sealing"),
			local.Close(),
		)
	}
	return local, nil
}

// Produce seals the artifact that produce writes into a new file under
// directory. When ownDirectory is true the local copy takes ownership of
// directory, and Close removes it after the local copy itself.
func Produce(
	ctx context.Context,
	directory string,
	role artifact.Role,
	owner Owner,
	ownDirectory bool,
	produce func(*os.File) error,
) (*Artifact, error) {
	spec, err := artifactSpecForRole(role)
	if err != nil {
		return nil, err
	}
	local, err := produceArtifactCopyWithSpec(ctx, directory, spec, owner, true, nil, produce)
	if err != nil {
		return nil, err
	}
	local.platform.removeDirectory = ownDirectory
	return local, nil
}

// Receive seals bytes transferred from an already-authoritative artifact. The
// local file is disposable: its owner recreates it after loss, so publication
// durability belongs to the source rather than this temporary copy. The callback
// must join all writes before returning. The directory remains caller-owned.
func Receive(ctx context.Context, directory string, role artifact.Role, expected cas.Descriptor, receive func(*os.File) error) (*Artifact, error) {
	spec, err := artifactSpecForRole(role)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactCopyDescriptor(spec, expected); err != nil {
		return nil, err
	}
	return produceArtifactCopyWithSpec(ctx, directory, spec, Owner{UID: os.Geteuid(), GID: os.Getegid()}, false, &expected, receive)
}

func produceArtifactCopyWithSpec(
	ctx context.Context,
	directory string,
	spec artifactSpec,
	owner Owner,
	syncToDisk bool,
	expected *cas.Descriptor,
	produce func(*os.File) error,
) (_ *Artifact, returnErr error) {
	if ctx == nil {
		return nil, errors.New("local artifact copy context is nil")
	}
	if produce == nil {
		return nil, errors.New("local artifact copy producer is nil")
	}
	if err := validateArtifactCopyOwner(owner); err != nil {
		return nil, err
	}
	if err := validateArtifactCopySpec(spec); err != nil {
		return nil, err
	}

	directoryFD, err := unix.Open(
		directory,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open local artifact copy directory: %w", err)
	}
	directoryFile := os.NewFile(uintptr(directoryFD), directory)
	defer func() {
		if directoryFile != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactCopyFile(directoryFile, "directory"),
			)
		}
	}()
	if err := validateArtifactCopyDirectory(directoryFile); err != nil {
		return nil, err
	}

	name, err := newArtifactCopyName()
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
		return nil, fmt.Errorf("create local artifact copy: %w", err)
	}
	writer := os.NewFile(uintptr(writerFD), name)
	defer func() {
		if writer != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactCopyFile(writer, "writer"),
			)
		}
		if name != "" && directoryFile != nil {
			returnErr = errors.Join(
				returnErr,
				removeArtifactCopy(directoryFile, name),
			)
		}
	}()
	if err := writer.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("set local artifact copy writer mode: %w", err)
	}
	if err := produce(writer); err != nil {
		return nil, fmt.Errorf("produce local artifact copy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("produce local artifact copy: %w", err)
	}

	before, err := inspectArtifactCopy(writer)
	if err != nil {
		return nil, err
	}
	if before.size < 1 || before.size > spec.maxBytes {
		return nil, fmt.Errorf(
			"local artifact copy size = %d, want 1..%d",
			before.size,
			spec.maxBytes,
		)
	}
	var descriptor cas.Descriptor
	if expected != nil {
		if before.size != expected.SizeBytes {
			return nil, errors.New("received artifact size differs from pinned descriptor")
		}
		// The source already pins the content identity. Verify it after closing
		// the writer and reopening the sealed inode, without hashing it twice.
		descriptor = *expected
	} else {
		digest, err := hashArtifactCopy(ctx, writer, before.size)
		if err != nil {
			return nil, err
		}
		descriptor = cas.Descriptor{Digest: digest, SizeBytes: before.size, MediaType: spec.mediaType}
	}
	if err := validateArtifactCopyDescriptor(spec, descriptor); err != nil {
		return nil, err
	}
	if syncToDisk {
		if err := writer.Sync(); err != nil {
			return nil, fmt.Errorf("sync local artifact copy: %w", err)
		}
	}
	if int(before.uid) != owner.UID || int(before.gid) != owner.GID {
		if err := writer.Chown(owner.UID, owner.GID); err != nil {
			return nil, fmt.Errorf("set local artifact copy owner: %w", err)
		}
	}
	if err := writer.Chmod(0o400); err != nil {
		return nil, fmt.Errorf("seal local artifact copy mode: %w", err)
	}
	if syncToDisk {
		if err := writer.Sync(); err != nil {
			return nil, fmt.Errorf("sync sealed local artifact copy: %w", err)
		}
	}
	sealed, err := inspectSealedArtifactCopy(writer, descriptor, owner)
	if err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		writer = nil
		return nil, fmt.Errorf("close local artifact copy writer: %w", err)
	}
	writer = nil

	verifier, err := openReadOnlyArtifactCopy(
		directoryFile,
		name,
		sealed,
	)
	if err != nil {
		return nil, fmt.Errorf("open artifact verifier local: %w", err)
	}
	defer func() {
		if verifier != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactCopyFile(verifier, "verifier"),
			)
		}
	}()
	upload, err := openReadOnlyArtifactCopy(
		directoryFile,
		name,
		sealed,
	)
	if err != nil {
		return nil, fmt.Errorf("open artifact upload local: %w", err)
	}
	defer func() {
		if upload != nil {
			returnErr = errors.Join(
				returnErr,
				closeArtifactCopyFile(upload, "upload"),
			)
		}
	}()
	reopenedDigest, err := hashArtifactCopy(ctx, verifier, descriptor.SizeBytes)
	if err != nil {
		return nil, err
	}
	if reopenedDigest != descriptor.Digest {
		return nil, fmt.Errorf(
			"reopened local artifact copy digest = %s, want %s",
			reopenedDigest,
			descriptor.Digest,
		)
	}

	local := &Artifact{
		descriptor: descriptor,
		platform: artifactPlatform{
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
	return local, nil
}

func validateArtifactCopyOwner(owner Owner) error {
	if owner.UID < 0 || uint64(owner.UID) > uint64(^uint32(0)) {
		return errors.New("local artifact copy owner UID is invalid")
	}
	if owner.GID < 0 || uint64(owner.GID) > uint64(^uint32(0)) {
		return errors.New("local artifact copy owner GID is invalid")
	}
	return nil
}

func copyArtifactCopy(
	ctx context.Context,
	destination *os.File,
	source io.Reader,
	expected cas.Descriptor,
) error {
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var sizeBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy local artifact copy: %w", err)
		}
		remaining := expected.SizeBytes + 1 - sizeBytes
		if remaining < int64(len(buffer)) {
			buffer = buffer[:remaining]
		}
		count, readErr := source.Read(buffer)
		if count < 0 || count > len(buffer) {
			return errors.New("copy local artifact copy: source returned an invalid byte count")
		}
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return fmt.Errorf("hash local artifact copy: %w", err)
			}
			if _, err := destination.Write(buffer[:count]); err != nil {
				return fmt.Errorf("write local artifact copy: %w", err)
			}
			sizeBytes += int64(count)
			if sizeBytes > expected.SizeBytes {
				return fmt.Errorf(
					"local artifact copy size exceeds expected %d bytes",
					expected.SizeBytes,
				)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read local artifact copy source: %w", readErr)
			}
			break
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	if sizeBytes != expected.SizeBytes {
		return fmt.Errorf(
			"local artifact copy size = %d, want %d",
			sizeBytes,
			expected.SizeBytes,
		)
	}
	actualDigest := sha256sum.FormatDigest(digest.Sum(nil))
	if actualDigest != expected.Digest {
		return fmt.Errorf(
			"local artifact copy digest = %s, want %s",
			actualDigest,
			expected.Digest,
		)
	}
	return nil
}

func inspectArtifactCopy(file *os.File) (fileIdentity, error) {
	if file == nil {
		return fileIdentity{}, errors.New("local artifact copy file is nil")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, fmt.Errorf("stat local artifact copy: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileIdentity{}, errors.New("local artifact copy is not a regular file")
	}
	return fileIdentity{
		device: uint64(stat.Dev),
		inode:  stat.Ino,
		size:   stat.Size,
		mode:   stat.Mode,
		uid:    stat.Uid,
		gid:    stat.Gid,
	}, nil
}

func validateArtifactCopyDirectory(directory *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return fmt.Errorf("stat local artifact copy directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("local artifact copy directory is not a directory")
	}
	if stat.Mode&0o7777 != 0o700 {
		return fmt.Errorf(
			"local artifact copy directory mode = %#o, want 0700",
			stat.Mode&0o7777,
		)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		return fmt.Errorf(
			"local artifact copy directory owner = %d:%d, want %d:%d",
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
	return nil
}

func inspectSealedArtifactCopy(
	file *os.File,
	expected cas.Descriptor,
	owner Owner,
) (fileIdentity, error) {
	identity, err := inspectArtifactCopy(file)
	if err != nil {
		return fileIdentity{}, err
	}
	if identity.size != expected.SizeBytes {
		return fileIdentity{}, fmt.Errorf(
			"sealed local artifact copy size = %d, want %d",
			identity.size,
			expected.SizeBytes,
		)
	}
	if identity.mode&0o7777 != 0o400 {
		return fileIdentity{}, fmt.Errorf(
			"sealed local artifact copy mode = %#o, want 0400",
			identity.mode&0o7777,
		)
	}
	if identity.uid != uint32(owner.UID) || identity.gid != uint32(owner.GID) {
		return fileIdentity{}, fmt.Errorf(
			"sealed local artifact copy owner = %d:%d, want %d:%d",
			identity.uid,
			identity.gid,
			owner.UID,
			owner.GID,
		)
	}
	return identity, nil
}

func openReadOnlyArtifactCopy(
	directory *os.File,
	name string,
	expected fileIdentity,
) (*os.File, error) {
	fd, err := unix.Openat(
		int(directory.Fd()),
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("reopen local artifact copy read-only: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	identity, err := inspectArtifactCopy(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if identity != expected {
		return nil, errors.Join(
			errors.New("read-only local artifact copy changed identity"),
			file.Close(),
		)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect read-only local artifact copy: %w", err),
			file.Close(),
		)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.Join(
			errors.New("local artifact copy descriptor is not read-only"),
			file.Close(),
		)
	}
	return file, nil
}

func hashArtifactCopy(
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
			return "", fmt.Errorf("hash local artifact copy: %w", err)
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return "", fmt.Errorf("hash local artifact copy: %w", err)
			}
			readBytes += int64(count)
			if readBytes > sizeBytes {
				return "", fmt.Errorf(
					"local artifact copy exceeds expected size %d",
					sizeBytes,
				)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", fmt.Errorf("read local artifact copy: %w", readErr)
			}
			break
		}
		if count == 0 {
			return "", io.ErrNoProgress
		}
	}
	if readBytes != sizeBytes {
		return "", fmt.Errorf(
			"local artifact copy size = %d, want %d",
			readBytes,
			sizeBytes,
		)
	}
	return sha256sum.FormatDigest(digest.Sum(nil)), nil
}

func newArtifactCopyName() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate local artifact copy name: %w", err)
	}
	return "local-" + hex.EncodeToString(random), nil
}

func removeArtifactCopy(directory *os.File, name string) error {
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove local artifact copy: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync local artifact copy directory: %w", err)
	}
	return nil
}

func (local *Artifact) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) (returnErr error) {
	if local == nil ||
		local.platform.directory == nil ||
		local.platform.name == "" ||
		local.upload == nil {
		return errors.New("local artifact copy is closed")
	}
	if filepath.Base(name) != name || name == "." || name == ".." || name == "" {
		return errors.New("local artifact copy link name is invalid")
	}
	owner := Owner{UID: uid, GID: gid}
	if err := validateArtifactCopyOwner(owner); err != nil {
		return err
	}
	currentOwner := Owner{
		UID: int(local.platform.identity.uid),
		GID: int(local.platform.identity.gid),
	}
	before, err := inspectSealedArtifactCopy(
		local.upload,
		local.descriptor,
		currentOwner,
	)
	if err != nil {
		return err
	}
	if before != local.platform.identity {
		return errors.New("local artifact copy changed before link")
	}
	if currentOwner != owner {
		// A hard link retains inode ownership, so transfer the sealed inode before
		// the jailed process receives its mode-0400 link.
		if err := local.upload.Chown(owner.UID, owner.GID); err != nil {
			return fmt.Errorf("transfer local artifact copy owner: %w", err)
		}
		transferred, err := inspectSealedArtifactCopy(
			local.upload,
			local.descriptor,
			owner,
		)
		if err != nil {
			return err
		}
		expected := before
		expected.uid = uint32(owner.UID)
		expected.gid = uint32(owner.GID)
		if transferred != expected {
			return errors.New("local artifact copy changed during owner transfer")
		}
		local.platform.identity = transferred
	}
	// A linked drive can outlive the transfer operation. Flush its data and
	// ownership before exposing it, including transient same-owner copies.
	if err := local.upload.Sync(); err != nil {
		return fmt.Errorf("sync local artifact copy before link: %w", err)
	}
	source, err := inspectArtifactCopyAt(
		int(local.platform.directory.Fd()),
		local.platform.name,
	)
	if err != nil {
		return fmt.Errorf("inspect local artifact copy source link: %w", err)
	}
	if source != local.platform.identity {
		return errors.New("local artifact copy source link changed identity")
	}

	directoryFD, err := unix.Open(
		directory,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("open local artifact copy link directory: %w", err)
	}
	destination := os.NewFile(uintptr(directoryFD), directory)
	defer func() {
		returnErr = errors.Join(
			returnErr,
			closeArtifactCopyFile(destination, "link directory"),
		)
	}()
	if err := unix.Linkat(
		int(local.platform.directory.Fd()),
		local.platform.name,
		directoryFD,
		name,
		0,
	); err != nil {
		return fmt.Errorf("link local artifact copy: %w", err)
	}
	linked, err := inspectArtifactCopyAt(directoryFD, name)
	if err != nil {
		return cleanupArtifactCopyLink(
			destination,
			name,
			fmt.Errorf("inspect linked local artifact copy: %w", err),
		)
	}
	after, err := inspectSealedArtifactCopy(
		local.upload,
		local.descriptor,
		owner,
	)
	if err != nil {
		return cleanupArtifactCopyLink(destination, name, err)
	}
	if linked != local.platform.identity || after != local.platform.identity {
		return cleanupArtifactCopyLink(
			destination,
			name,
			errors.New("local artifact copy changed during link"),
		)
	}
	return nil
}

func inspectArtifactCopyAt(
	directoryFD int,
	name string,
) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(
		directoryFD,
		name,
		&stat,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fileIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileIdentity{}, errors.New("local artifact copy link is not regular")
	}
	return fileIdentity{
		device: uint64(stat.Dev),
		inode:  stat.Ino,
		size:   stat.Size,
		mode:   stat.Mode,
		uid:    stat.Uid,
		gid:    stat.Gid,
	}, nil
}

func cleanupArtifactCopyLink(
	directory *os.File,
	name string,
	cause error,
) error {
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("remove invalid local artifact copy link: %w", err),
		)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("sync invalid local artifact copy link cleanup: %w", err),
		)
	}
	return cause
}

func closeArtifactCopyPlatform(local *Artifact) error {
	if local == nil || local.platform.directory == nil {
		return nil
	}
	var closeErr error
	if local.platform.name != "" {
		closeErr = errors.Join(
			closeErr,
			removeArtifactCopy(
				local.platform.directory,
				local.platform.name,
			),
		)
		local.platform.name = ""
	}
	directoryPath := local.platform.directory.Name()
	closeErr = errors.Join(closeErr, local.platform.directory.Close())
	local.platform.directory = nil
	if local.platform.removeDirectory {
		closeErr = errors.Join(closeErr, os.Remove(directoryPath))
		local.platform.removeDirectory = false
	}
	local.platform.identity = fileIdentity{}
	return closeErr
}

func validateArtifactCopyPlatform(local *Artifact) error {
	if local == nil ||
		local.platform.directory == nil ||
		local.platform.name == "" ||
		local.upload == nil {
		return errors.New("local artifact copy is closed")
	}
	identity, err := inspectArtifactCopy(local.upload)
	if err != nil {
		return err
	}
	if identity != local.platform.identity {
		return errors.New("local artifact copy changed after sealing")
	}
	pathIdentity, err := inspectArtifactCopyAt(
		int(local.platform.directory.Fd()),
		local.platform.name,
	)
	if err != nil {
		return fmt.Errorf("inspect local artifact copy path: %w", err)
	}
	if pathIdentity != local.platform.identity {
		return errors.New("local artifact copy path changed after sealing")
	}
	return nil
}

func closeArtifactCopyFile(file *os.File, role string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close local artifact copy %s: %w", role, err)
	}
	return nil
}
