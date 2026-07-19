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
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const toolchainCorpusRoot = "/usr/lib/helmr/toolchain-release"

type toolCorpusOwner struct {
	uid uint32
	gid uint32
}

func LoadToolchainCorpus(
	ctx context.Context,
	catalog *ToolchainCatalog,
	architecture RuntimeArchitecture,
) (*ToolchainCorpus, error) {
	return loadToolchainCorpus(
		ctx,
		toolchainCorpusRoot,
		catalog,
		architecture,
		toolCorpusOwner{},
	)
}

func loadToolchainCorpus(
	ctx context.Context,
	root string,
	catalog *ToolchainCatalog,
	architecture RuntimeArchitecture,
	owner toolCorpusOwner,
) (_ *ToolchainCorpus, returnErr error) {
	if ctx == nil {
		return nil, errors.New("standard-toolchain corpus context is nil")
	}
	if catalog == nil || !catalog.authenticated {
		return nil, errToolchainCatalogUnauthenticated
	}
	objects, err := toolchainClosureObjects(catalog, architecture)
	if err != nil {
		return nil, err
	}
	rootDirectory, err := openToolCorpusDirectory(
		root,
		"standard-toolchain corpus root",
		owner,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rootDirectory != nil {
			returnErr = errors.Join(returnErr, rootDirectory.Close())
		}
	}()
	if err := requireToolCorpusEntries(rootDirectory, []string{
		"catalog.json",
		"catalog.sigstore.json",
		"objects",
		"trusted-root.json",
	}); err != nil {
		return nil, fmt.Errorf(
			"enumerate standard-toolchain corpus root: %w",
			err,
		)
	}

	objectsDirectory, err := openToolCorpusDirectoryAt(
		int(rootDirectory.Fd()),
		"objects",
		"standard-toolchain corpus objects",
		owner,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if objectsDirectory != nil {
			returnErr = errors.Join(returnErr, objectsDirectory.Close())
		}
	}()
	if err := requireToolCorpusEntries(objectsDirectory, []string{"sha256"}); err != nil {
		return nil, fmt.Errorf("enumerate standard-toolchain corpus objects: %w", err)
	}
	digestDirectory, err := openToolCorpusDirectoryAt(
		int(objectsDirectory.Fd()),
		"sha256",
		"standard-toolchain corpus sha256 objects",
		owner,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if digestDirectory != nil {
			returnErr = errors.Join(returnErr, digestDirectory.Close())
		}
	}()

	names := make([]string, len(objects))
	for index, object := range objects {
		names[index] = strings.TrimPrefix(object.Digest, "sha256:")
	}
	if err := requireToolCorpusEntries(digestDirectory, names); err != nil {
		return nil, fmt.Errorf(
			"enumerate standard-toolchain corpus sha256 objects: %w",
			err,
		)
	}
	identities := make(map[string]toolCorpusIdentity, len(objects))
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("verify standard-toolchain corpus: %w", err)
		}
		name := strings.TrimPrefix(object.Digest, "sha256:")
		file, err := openToolCorpusFileAt(
			int(digestDirectory.Fd()),
			name,
			"standard-toolchain object "+object.Digest,
			owner,
			object.SizeBytes,
			object.SizeBytes,
		)
		if err != nil {
			return nil, err
		}
		identity, verifyErr := verifyToolCorpusObject(ctx, file, object, owner)
		closeErr := file.Close()
		if verifyErr != nil {
			return nil, errors.Join(verifyErr, closeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf(
				"close standard-toolchain object %s: %w",
				object.Digest,
				closeErr,
			)
		}
		if err := matchToolCorpusLink(
			int(digestDirectory.Fd()),
			name,
			identity,
			"standard-toolchain object "+object.Digest,
		); err != nil {
			return nil, err
		}
		identities[object.Digest] = identity
	}

	toolchains := make(map[string]Toolchain)
	for _, toolchain := range catalog.toolchains {
		if toolchain.Architecture != architecture {
			continue
		}
		digest, err := StandardToolchainDigest(toolchain)
		if err != nil {
			return nil, err
		}
		toolchains[digest] = toolchain
	}
	corpus := &ToolchainCorpus{
		architecture: architecture,
		toolchains:   toolchains,
		directory:    digestDirectory,
		identities:   identities,
		ownerUID:     owner.uid,
		ownerGID:     owner.gid,
	}
	digestDirectory = nil
	return corpus, nil
}

func (c *ToolchainCorpus) OpenToolchain(
	ctx context.Context,
	toolchain Toolchain,
) (_ *ToolObjectFile, returnErr error) {
	if c == nil {
		return nil, errors.New("standard-toolchain corpus is nil")
	}
	if ctx == nil {
		return nil, errors.New("standard-toolchain corpus context is nil")
	}
	if err := validateToolchain(toolchain); err != nil {
		return nil, err
	}
	if toolchain.Architecture != c.architecture {
		return nil, errors.New(
			"standard-toolchain architecture does not match the installed corpus",
		)
	}
	digest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		return nil, err
	}
	descriptor := toolObject(toolchain.ToolchainClosure)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.directory == nil {
		return nil, errors.New(
			"standard-toolchain corpus is closed or not installed",
		)
	}
	registered, ok := c.toolchains[digest]
	if !ok || registered != toolchain {
		return nil, errors.New(
			"standard toolchain is absent from the installed catalog",
		)
	}
	expected, ok := c.identities[descriptor.Digest]
	if !ok {
		return nil, errors.New(
			"standard-toolchain object is absent from the installed corpus",
		)
	}
	name := strings.TrimPrefix(descriptor.Digest, "sha256:")
	file, err := openToolCorpusFileAt(
		int(c.directory.Fd()),
		name,
		"standard-toolchain object "+descriptor.Digest,
		toolCorpusOwner{uid: c.ownerUID, gid: c.ownerGID},
		descriptor.SizeBytes,
		descriptor.SizeBytes,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if file != nil {
			returnErr = errors.Join(returnErr, file.Close())
		}
	}()
	identity, err := verifyToolCorpusObject(
		ctx,
		file,
		descriptor,
		toolCorpusOwner{uid: c.ownerUID, gid: c.ownerGID},
	)
	if err != nil {
		return nil, err
	}
	if identity != expected {
		return nil, errors.New(
			"standard-toolchain object changed after worker readiness",
		)
	}
	if err := matchToolCorpusLink(
		int(c.directory.Fd()),
		name,
		identity,
		"standard-toolchain object "+descriptor.Digest,
	); err != nil {
		return nil, err
	}
	result := &ToolObjectFile{file: file, descriptor: descriptor}
	file = nil
	return result, nil
}

func openToolCorpusDirectory(
	path,
	label string,
	owner toolCorpusOwner,
) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validateToolCorpusDirectory(file, label, owner); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func openToolCorpusDirectoryAt(
	parentFD int,
	name,
	label string,
	owner toolCorpusOwner,
) (*os.File, error) {
	fd, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := validateToolCorpusDirectory(file, label, owner); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func validateToolCorpusDirectory(
	file *os.File,
	label string,
	owner toolCorpusOwner,
) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("stat %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%s is not a directory", label)
	}
	if stat.Mode&0o7777 != 0o755 {
		return fmt.Errorf("%s mode = %#o, want 0755", label, stat.Mode&0o7777)
	}
	if stat.Uid != owner.uid || stat.Gid != owner.gid {
		return fmt.Errorf(
			"%s owner = %d:%d, want %d:%d",
			label,
			stat.Uid,
			stat.Gid,
			owner.uid,
			owner.gid,
		)
	}
	return nil
}

func requireToolCorpusEntries(directory *os.File, expected []string) error {
	if _, err := directory.Seek(0, io.SeekStart); err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	actual := make([]string, len(entries))
	for index, entry := range entries {
		actual[index] = entry.Name()
	}
	sort.Strings(actual)
	expected = append([]string(nil), expected...)
	sort.Strings(expected)
	if len(actual) != len(expected) {
		return fmt.Errorf("entries = %v, want %v", actual, expected)
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return fmt.Errorf("entries = %v, want %v", actual, expected)
		}
	}
	return nil
}

func openToolCorpusFileAt(
	parentFD int,
	name,
	label string,
	owner toolCorpusOwner,
	minBytes,
	maxBytes int64,
) (*os.File, error) {
	fd, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), name)
	identity, err := inspectToolCorpusFile(file, label, owner, minBytes, maxBytes)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if identity.links != 1 {
		return nil, errors.Join(
			fmt.Errorf("%s link count = %d, want 1", label, identity.links),
			file.Close(),
		)
	}
	return file, nil
}

func inspectToolCorpusFile(
	file *os.File,
	label string,
	owner toolCorpusOwner,
	minBytes,
	maxBytes int64,
) (toolCorpusIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return toolCorpusIdentity{}, fmt.Errorf("stat %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return toolCorpusIdentity{}, fmt.Errorf("%s is not a regular file", label)
	}
	if stat.Mode&0o7777 != 0o444 {
		return toolCorpusIdentity{}, fmt.Errorf("%s mode = %#o, want 0444", label, stat.Mode&0o7777)
	}
	if stat.Uid != owner.uid || stat.Gid != owner.gid {
		return toolCorpusIdentity{}, fmt.Errorf(
			"%s owner = %d:%d, want %d:%d",
			label,
			stat.Uid,
			stat.Gid,
			owner.uid,
			owner.gid,
		)
	}
	if stat.Size < minBytes || stat.Size > maxBytes {
		return toolCorpusIdentity{}, fmt.Errorf(
			"%s size = %d, want %d..%d",
			label,
			stat.Size,
			minBytes,
			maxBytes,
		)
	}
	return toolCorpusIdentity{
		device:             uint64(stat.Dev),
		inode:              stat.Ino,
		size:               stat.Size,
		mode:               stat.Mode,
		uid:                stat.Uid,
		gid:                stat.Gid,
		links:              uint64(stat.Nlink),
		modifiedSeconds:    stat.Mtim.Sec,
		modifiedNanosecond: stat.Mtim.Nsec,
		changedSeconds:     stat.Ctim.Sec,
		changedNanosecond:  stat.Ctim.Nsec,
	}, nil
}

func verifyToolCorpusObject(
	ctx context.Context,
	file *os.File,
	descriptor ToolObject,
	owner toolCorpusOwner,
) (toolCorpusIdentity, error) {
	label := "standard-toolchain object " + descriptor.Digest
	before, err := inspectToolCorpusFile(
		file,
		label,
		owner,
		descriptor.SizeBytes,
		descriptor.SizeBytes,
	)
	if err != nil {
		return toolCorpusIdentity{}, err
	}
	reader := io.NewSectionReader(file, 0, descriptor.SizeBytes+1)
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return toolCorpusIdentity{}, fmt.Errorf("hash %s: %w", label, err)
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > descriptor.SizeBytes {
				return toolCorpusIdentity{}, fmt.Errorf("%s has trailing bytes", label)
			}
			if _, err := hash.Write(buffer[:count]); err != nil {
				return toolCorpusIdentity{}, fmt.Errorf("hash %s: %w", label, err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return toolCorpusIdentity{}, fmt.Errorf("read %s: %w", label, readErr)
		}
	}
	if total != descriptor.SizeBytes {
		return toolCorpusIdentity{}, fmt.Errorf(
			"%s bytes = %d, want %d",
			label,
			total,
			descriptor.SizeBytes,
		)
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != descriptor.Digest {
		return toolCorpusIdentity{}, fmt.Errorf(
			"%s digest = %s, want %s",
			label,
			actualDigest,
			descriptor.Digest,
		)
	}
	after, err := inspectToolCorpusFile(
		file,
		label,
		owner,
		descriptor.SizeBytes,
		descriptor.SizeBytes,
	)
	if err != nil {
		return toolCorpusIdentity{}, err
	}
	if after != before {
		return toolCorpusIdentity{}, fmt.Errorf("%s changed while hashing", label)
	}
	return after, nil
}

func matchToolCorpusLink(
	parentFD int,
	name string,
	expected toolCorpusIdentity,
	label string,
) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("inspect %s directory entry: %w", label, err)
	}
	actual := toolCorpusIdentity{
		device:             uint64(stat.Dev),
		inode:              stat.Ino,
		size:               stat.Size,
		mode:               stat.Mode,
		uid:                stat.Uid,
		gid:                stat.Gid,
		links:              uint64(stat.Nlink),
		modifiedSeconds:    stat.Mtim.Sec,
		modifiedNanosecond: stat.Mtim.Nsec,
		changedSeconds:     stat.Ctim.Sec,
		changedNanosecond:  stat.Ctim.Nsec,
	}
	if actual != expected {
		return fmt.Errorf("%s directory entry changed identity", label)
	}
	return nil
}
