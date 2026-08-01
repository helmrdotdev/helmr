package worker

import (
	"errors"
	"fmt"
	"io/fs"
	"math/bits"
	"path/filepath"
	"strconv"
	"strings"
)

type storageFileKind uint8

const (
	storageFileUnknown storageFileKind = iota
	storageFileDirectory
	storageFileSymlink
	storageFileBlock
	storageFileRegular
)

type storageFile struct {
	Kind   storageFileKind
	Device string
	RDev   string
	Size   int64
	Blocks int64
}

type storageFS struct {
	Blocks    uint64
	Available uint64
	BlockSize uint64
}

type storageProbe interface {
	Lstat(string) (storageFile, error)
	StatFS(string) (storageFS, error)
	ReadFile(string) ([]byte, error)
}

func proveBuildStorage(config BuildStorageConfig, probe storageProbe) (BuildStorageProof, error) {
	if probe == nil {
		return BuildStorageProof{}, errors.New("storage probe is required")
	}
	if config.RequiredCacheBytes == 0 || config.RequiredScratchBytes == 0 {
		return BuildStorageProof{}, errors.New("certified build storage sizes must be positive")
	}
	if config.RequiredScratchAvailableBytes == 0 || config.RequiredScratchAvailableBytes > config.RequiredScratchBytes {
		return BuildStorageProof{}, errors.New("required build scratch availability must be positive and no greater than its capacity")
	}

	paths := []struct {
		name string
		path string
	}{
		{"build cache root", config.CacheRoot},
		{"build scratch root", config.ScratchRoot},
		{"worker work directory", config.WorkDir},
		{"Firecracker jailer root", config.JailerRoot},
	}
	for _, configured := range paths {
		if err := validateStoragePath(configured.name, configured.path, probe); err != nil {
			return BuildStorageProof{}, err
		}
	}
	if !strictDescendant(config.ScratchRoot, config.WorkDir) {
		return BuildStorageProof{}, errors.New("worker work directory must be a strict descendant of build scratch")
	}
	if !strictDescendant(config.ScratchRoot, config.JailerRoot) {
		return BuildStorageProof{}, errors.New("firecracker jailer root must be a strict descendant of build scratch")
	}

	substrateCache := filepath.Join(config.CacheRoot, "substrate-cache")
	artifactCache := filepath.Join(config.CacheRoot, "artifact-cache")
	for _, layout := range []struct {
		name string
		path string
	}{
		{"substrate cache", substrateCache},
		{"Artifact cache", artifactCache},
	} {
		if !strictDescendant(config.CacheRoot, layout.path) {
			return BuildStorageProof{}, fmt.Errorf("%s escapes build cache", layout.name)
		}
	}
	rawMountInfo, err := probe.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return BuildStorageProof{}, fmt.Errorf("read mountinfo: %w", err)
	}
	mounts, err := parseMountInfo(rawMountInfo)
	if err != nil {
		return BuildStorageProof{}, err
	}
	cache, cacheIdentity, err := proveStorageMount(config.CacheRoot, config.RequiredCacheBytes, 0, mounts, probe)
	if err != nil {
		return BuildStorageProof{}, fmt.Errorf("prove build cache: %w", err)
	}
	scratch, scratchIdentity, err := proveStorageMount(
		config.ScratchRoot,
		config.RequiredScratchBytes,
		config.RequiredScratchAvailableBytes,
		mounts,
		probe,
	)
	if err != nil {
		return BuildStorageProof{}, fmt.Errorf("prove build scratch: %w", err)
	}
	if cache.MountID == scratch.MountID {
		return BuildStorageProof{}, errors.New("build cache and scratch share a mount ID")
	}
	if cache.Device == scratch.Device {
		return BuildStorageProof{}, errors.New("build cache and scratch share a device")
	}
	if cacheIdentity == scratchIdentity {
		return BuildStorageProof{}, errors.New("build cache and scratch share a device source")
	}
	jailerMount, err := mountContaining(config.JailerRoot, mounts)
	if err != nil {
		return BuildStorageProof{}, fmt.Errorf("prove Firecracker jailer root mount: %w", err)
	}
	if jailerMount.ID != scratch.MountID {
		return BuildStorageProof{}, errors.New("firecracker jailer root is not on the build scratch mount")
	}
	workMount, err := mountContaining(config.WorkDir, mounts)
	if err != nil {
		return BuildStorageProof{}, fmt.Errorf("prove worker work directory mount: %w", err)
	}
	if workMount.ID != scratch.MountID {
		return BuildStorageProof{}, errors.New("worker work directory is not on the build scratch mount")
	}
	if nested, ok := nestedMount(config.WorkDir, mounts); ok {
		return BuildStorageProof{}, fmt.Errorf(
			"worker work directory contains nested mount %q",
			nested.MountPoint,
		)
	}
	if nested, ok := nestedMount(config.JailerRoot, mounts); ok {
		return BuildStorageProof{}, fmt.Errorf(
			"firecracker jailer root contains nested mount %q",
			nested.MountPoint,
		)
	}

	return BuildStorageProof{
		Cache:             cache,
		Scratch:           scratch,
		WorkDir:           config.WorkDir,
		JailerRoot:        config.JailerRoot,
		SubstrateCacheDir: substrateCache,
		ArtifactCacheDir:  artifactCache,
	}, nil
}

func nestedMount(root string, mounts []storageMountInfo) (storageMountInfo, bool) {
	for _, mount := range mounts {
		if strictDescendant(root, mount.MountPoint) {
			return mount, true
		}
	}
	return storageMountInfo{}, false
}

func mountContaining(path string, mounts []storageMountInfo) (storageMountInfo, error) {
	var match storageMountInfo
	found := false
	for _, mount := range mounts {
		if path != mount.MountPoint && !strictDescendant(mount.MountPoint, path) {
			continue
		}
		if !found || len(mount.MountPoint) > len(match.MountPoint) {
			match = mount
			found = true
			continue
		}
		if len(mount.MountPoint) == len(match.MountPoint) {
			return storageMountInfo{}, fmt.Errorf(
				"%q matches duplicate mountpoints",
				path,
			)
		}
	}
	if !found {
		return storageMountInfo{}, fmt.Errorf("%q has no containing mount", path)
	}
	return match, nil
}

func validateStoragePath(name, path string, probe storageProbe) error {
	if path == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be absolute", name)
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("%s must be canonical", name)
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, current), current)
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		stat, err := probe.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect %s component %q: %w", name, current, err)
		}
		if stat.Kind == storageFileSymlink {
			return fmt.Errorf("%s component %q is a symlink", name, current)
		}
		if stat.Kind != storageFileDirectory {
			return fmt.Errorf("%s component %q is not a directory", name, current)
		}
	}
	return nil
}

func strictDescendant(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type storageMountInfo struct {
	ID         uint64
	Device     string
	Root       string
	MountPoint string
	Options    []string
	FSType     string
	Source     string
	Super      []string
}

func parseMountInfo(raw []byte) ([]storageMountInfo, error) {
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	mounts := make([]storageMountInfo, 0, len(lines))
	for lineNumber, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if len(fields) < 10 || separator < 6 || separator+3 >= len(fields) {
			return nil, fmt.Errorf("mountinfo line %d is malformed", lineNumber+1)
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("mountinfo line %d has invalid mount ID", lineNumber+1)
		}
		mountPoint, err := unescapeMountInfo(fields[4])
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d mount point: %w", lineNumber+1, err)
		}
		root, err := unescapeMountInfo(fields[3])
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d root: %w", lineNumber+1, err)
		}
		source, err := unescapeMountInfo(fields[separator+2])
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d source: %w", lineNumber+1, err)
		}
		mounts = append(mounts, storageMountInfo{
			ID:         id,
			Device:     fields[2],
			Root:       filepath.Clean(root),
			MountPoint: filepath.Clean(mountPoint),
			Options:    strings.Split(fields[5], ","),
			FSType:     fields[separator+1],
			Source:     source,
			Super:      strings.Split(fields[separator+3], ","),
		})
	}
	return mounts, nil
}

func unescapeMountInfo(value string) (string, error) {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			builder.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", errors.New("truncated escape")
		}
		code := value[index+1 : index+4]
		switch code {
		case "040":
			builder.WriteByte(' ')
		case "011":
			builder.WriteByte('\t')
		case "012":
			builder.WriteByte('\n')
		case "134":
			builder.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported escape %q", code)
		}
		index += 3
	}
	return builder.String(), nil
}

func proveStorageMount(root string, required uint64, requiredAvailable uint64, mounts []storageMountInfo, probe storageProbe) (BuildStorageMount, string, error) {
	var matches []storageMountInfo
	for _, mount := range mounts {
		if mount.MountPoint == root {
			matches = append(matches, mount)
		}
	}
	if len(matches) != 1 {
		return BuildStorageMount{}, "", fmt.Errorf("%q must identify exactly one mountpoint", root)
	}
	mount := matches[0]
	if mount.Root != "/" {
		return BuildStorageMount{}, "", fmt.Errorf("%q is a bind or subdirectory mount", root)
	}
	if mount.FSType != "ext4" {
		return BuildStorageMount{}, "", fmt.Errorf("%q uses unsupported filesystem %q", root, mount.FSType)
	}
	if !hasMountOption(mount.Options, "rw") {
		return BuildStorageMount{}, "", fmt.Errorf("%q is not writable", root)
	}
	if hasMountOptionPrefix(mount.Options, "discard") || hasMountOptionPrefix(mount.Super, "discard") {
		return BuildStorageMount{}, "", fmt.Errorf("%q enables discard", root)
	}

	rootStat, err := probe.Lstat(root)
	if err != nil {
		return BuildStorageMount{}, "", fmt.Errorf("stat mount root: %w", err)
	}
	device, err := parseDevice(mount.Device)
	if err != nil {
		return BuildStorageMount{}, "", err
	}
	if rootStat.Device != device {
		return BuildStorageMount{}, "", fmt.Errorf("%q device does not match mountinfo", root)
	}

	statfs, err := probe.StatFS(root)
	if err != nil {
		return BuildStorageMount{}, "", fmt.Errorf("statfs mount root: %w", err)
	}
	high, capacity := bits.Mul64(statfs.Blocks, statfs.BlockSize)
	if high != 0 {
		return BuildStorageMount{}, "", fmt.Errorf("%q capacity overflows uint64", root)
	}
	high, available := bits.Mul64(statfs.Available, statfs.BlockSize)
	if high != 0 {
		return BuildStorageMount{}, "", fmt.Errorf("%q available capacity overflows uint64", root)
	}
	if capacity < required {
		return BuildStorageMount{}, "", fmt.Errorf("%q has %d bytes of usable capacity; need %d", root, capacity, required)
	}
	if available < requiredAvailable {
		return BuildStorageMount{}, "", fmt.Errorf("%q has %d available bytes; need %d", root, available, requiredAvailable)
	}

	identity, err := proveMountSource(mount, probe)
	if err != nil {
		return BuildStorageMount{}, "", err
	}
	return BuildStorageMount{
		Root:           root,
		MountID:        mount.ID,
		Device:         mount.Device,
		Source:         identity,
		CapacityBytes:  capacity,
		AvailableBytes: available,
	}, identity, nil
}

func proveMountSource(mount storageMountInfo, probe storageProbe) (string, error) {
	if !filepath.IsAbs(mount.Source) || filepath.Clean(mount.Source) != mount.Source {
		return "", fmt.Errorf("mount %q has unprovable source %q", mount.MountPoint, mount.Source)
	}
	if err := validateSourcePath(mount.Source, probe); err != nil {
		return "", err
	}
	source, err := probe.Lstat(mount.Source)
	if err != nil {
		return "", fmt.Errorf("stat mount source %q: %w", mount.Source, err)
	}
	if source.Kind != storageFileBlock {
		return "", fmt.Errorf("mount source %q is not a block device", mount.Source)
	}
	device, err := parseDevice(mount.Device)
	if err != nil {
		return "", err
	}
	if source.RDev != device {
		return "", fmt.Errorf("mount source %q does not match mounted device", mount.Source)
	}

	backingPath := filepath.Join("/sys/dev/block", mount.Device, "loop/backing_file")
	rawBacking, err := probe.ReadFile(backingPath)
	if errors.Is(err, fs.ErrNotExist) {
		return mount.Source, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect backing file for %q: %w", mount.Source, err)
	}
	backing := strings.TrimSpace(string(rawBacking))
	if backing == "" {
		return "", fmt.Errorf("mount source %q has an empty backing file", mount.Source)
	}
	if !filepath.IsAbs(backing) {
		backing = string(filepath.Separator) + backing
	}
	backing = filepath.Clean(backing)
	if err := validateSourcePath(backing, probe); err != nil {
		return "", err
	}
	stat, err := probe.Lstat(backing)
	if err != nil {
		return "", fmt.Errorf("stat backing file %q: %w", backing, err)
	}
	if stat.Kind != storageFileRegular || stat.Size <= 0 || stat.Blocks < 0 {
		return "", fmt.Errorf("backing file %q is not a non-empty regular file", backing)
	}
	allocatedHigh, allocated := bits.Mul64(uint64(stat.Blocks), 512)
	if allocatedHigh != 0 || allocated < uint64(stat.Size) {
		return "", fmt.Errorf("backing file %q allocation is smaller than its size", backing)
	}
	return backing, nil
}

func validateSourcePath(path string, probe storageProbe) error {
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		stat, err := probe.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect source component %q: %w", current, err)
		}
		if stat.Kind == storageFileSymlink {
			return fmt.Errorf("source component %q is a symlink", current)
		}
	}
	return nil
}

func parseDevice(value string) (string, error) {
	majorText, minorText, ok := strings.Cut(value, ":")
	if !ok {
		return "", fmt.Errorf("mount device %q is malformed", value)
	}
	major, err := strconv.ParseUint(majorText, 10, 32)
	if err != nil {
		return "", fmt.Errorf("mount device %q has invalid major number", value)
	}
	minor, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return "", fmt.Errorf("mount device %q has invalid minor number", value)
	}
	return strconv.FormatUint(major, 10) + ":" + strconv.FormatUint(minor, 10), nil
}

func hasMountOption(options []string, expected string) bool {
	for _, option := range options {
		if option == expected {
			return true
		}
	}
	return false
}

func hasMountOptionPrefix(options []string, expected string) bool {
	for _, option := range options {
		name, _, _ := strings.Cut(option, "=")
		if name == expected {
			return true
		}
	}
	return false
}
