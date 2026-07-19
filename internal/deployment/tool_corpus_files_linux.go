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

const toolCorpusRoot = "/usr/lib/helmr/dependency-tools"

type toolCorpusOwner struct {
	uid uint32
	gid uint32
}

func LoadToolCorpus(
	ctx context.Context,
	registry *ToolRegistry,
	architecture RuntimeArchitecture,
) (*ToolCorpus, error) {
	return loadToolCorpus(ctx, toolCorpusRoot, registry, architecture, toolCorpusOwner{})
}

func loadToolCorpus(
	ctx context.Context,
	root string,
	registry *ToolRegistry,
	architecture RuntimeArchitecture,
	owner toolCorpusOwner,
) (_ *ToolCorpus, returnErr error) {
	if ctx == nil {
		return nil, errors.New("dependency tool corpus context is nil")
	}
	rootDirectory, err := openToolCorpusDirectory(root, "dependency tool corpus root", owner)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rootDirectory != nil {
			returnErr = errors.Join(returnErr, rootDirectory.Close())
		}
	}()
	if err := requireToolCorpusEntries(rootDirectory, []string{"corpus.json", "objects"}); err != nil {
		return nil, fmt.Errorf("enumerate dependency tool corpus root: %w", err)
	}

	manifest, err := openToolCorpusFileAt(
		int(rootDirectory.Fd()),
		"corpus.json",
		"dependency tool corpus manifest",
		owner,
		1,
		maxToolCorpusManifestBytes,
	)
	if err != nil {
		return nil, err
	}
	raw, manifestIdentity, err := readToolCorpusManifest(ctx, manifest, owner)
	closeErr := manifest.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close dependency tool corpus manifest: %w", closeErr)
	}
	if err := matchToolCorpusLink(
		int(rootDirectory.Fd()),
		"corpus.json",
		manifestIdentity,
		"dependency tool corpus manifest",
	); err != nil {
		return nil, err
	}
	corpus, err := ParseToolCorpus(raw, registry, architecture)
	if err != nil {
		return nil, err
	}

	objectsDirectory, err := openToolCorpusDirectoryAt(
		int(rootDirectory.Fd()),
		"objects",
		"dependency tool corpus objects",
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
		return nil, fmt.Errorf("enumerate dependency tool corpus objects: %w", err)
	}
	digestDirectory, err := openToolCorpusDirectoryAt(
		int(objectsDirectory.Fd()),
		"sha256",
		"dependency tool corpus sha256 objects",
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

	names := make([]string, len(corpus.objects))
	for index, object := range corpus.objects {
		names[index] = strings.TrimPrefix(object.Digest, "sha256:")
	}
	if err := requireToolCorpusEntries(digestDirectory, names); err != nil {
		return nil, fmt.Errorf("enumerate dependency tool corpus sha256 objects: %w", err)
	}
	identities := make(map[string]toolCorpusIdentity, len(corpus.objects))
	for _, object := range corpus.objects {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("verify dependency tool corpus: %w", err)
		}
		name := strings.TrimPrefix(object.Digest, "sha256:")
		file, err := openToolCorpusFileAt(
			int(digestDirectory.Fd()),
			name,
			"dependency tool object "+object.Digest,
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
			return nil, fmt.Errorf("close dependency tool object %s: %w", object.Digest, closeErr)
		}
		if err := matchToolCorpusLink(
			int(digestDirectory.Fd()),
			name,
			identity,
			"dependency tool object "+object.Digest,
		); err != nil {
			return nil, err
		}
		identities[object.Digest] = identity
	}

	corpus.directory = digestDirectory
	corpus.identities = identities
	corpus.ownerUID = owner.uid
	corpus.ownerGID = owner.gid
	digestDirectory = nil
	return corpus, nil
}

func (c *ToolCorpus) OpenToolset(
	ctx context.Context,
	toolset Toolset,
) (_ *ToolObjectFile, returnErr error) {
	if c == nil {
		return nil, errors.New("dependency tool corpus is nil")
	}
	if ctx == nil {
		return nil, errors.New("dependency tool corpus context is nil")
	}
	if err := validateToolset(toolset); err != nil {
		return nil, err
	}
	if toolset.Architecture != c.architecture {
		return nil, errors.New("dependency toolset architecture does not match the installed corpus")
	}
	descriptor := toolObject(toolset.Artifact)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.directory == nil {
		return nil, errors.New("dependency tool corpus is closed or not installed")
	}
	expected, ok := c.identities[descriptor.Digest]
	if !ok {
		return nil, errors.New("dependency toolset object is absent from the installed corpus")
	}
	name := strings.TrimPrefix(descriptor.Digest, "sha256:")
	file, err := openToolCorpusFileAt(
		int(c.directory.Fd()),
		name,
		"dependency toolset object "+descriptor.Digest,
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
		return nil, errors.New("dependency toolset object changed after worker readiness")
	}
	if err := matchToolCorpusLink(
		int(c.directory.Fd()),
		name,
		identity,
		"dependency toolset object "+descriptor.Digest,
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

func readToolCorpusManifest(
	ctx context.Context,
	file *os.File,
	owner toolCorpusOwner,
) ([]byte, toolCorpusIdentity, error) {
	before, err := inspectToolCorpusFile(
		file,
		"dependency tool corpus manifest",
		owner,
		1,
		maxToolCorpusManifestBytes,
	)
	if err != nil {
		return nil, toolCorpusIdentity{}, err
	}
	raw, err := readToolCorpusBytes(ctx, file, before.size)
	if err != nil {
		return nil, toolCorpusIdentity{}, err
	}
	after, err := inspectToolCorpusFile(
		file,
		"dependency tool corpus manifest",
		owner,
		before.size,
		before.size,
	)
	if err != nil {
		return nil, toolCorpusIdentity{}, err
	}
	if after != before {
		return nil, toolCorpusIdentity{}, errors.New(
			"dependency tool corpus manifest changed while reading",
		)
	}
	return raw, after, nil
}

func readToolCorpusBytes(
	ctx context.Context,
	file *os.File,
	sizeBytes int64,
) ([]byte, error) {
	if sizeBytes > maxToolCorpusManifestBytes {
		return nil, errors.New("dependency tool corpus manifest exceeds the in-memory bound")
	}
	reader := io.NewSectionReader(file, 0, sizeBytes+1)
	raw := make([]byte, 0, sizeBytes)
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			raw = append(raw, buffer[:count]...)
			if int64(len(raw)) > sizeBytes {
				return nil, errors.New("dependency tool corpus manifest has trailing bytes")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if int64(len(raw)) != sizeBytes {
		return nil, errors.New("dependency tool corpus manifest ended before its sealed size")
	}
	return raw, nil
}

func verifyToolCorpusObject(
	ctx context.Context,
	file *os.File,
	descriptor ToolObject,
	owner toolCorpusOwner,
) (toolCorpusIdentity, error) {
	label := "dependency tool object " + descriptor.Digest
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
