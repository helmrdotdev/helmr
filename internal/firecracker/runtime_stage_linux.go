//go:build linux

package firecracker

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sys/unix"
)

const BootCorpusMaxMiB = int64(2048)

const runtimeStagePrefix = "runtime-"
const runtimeManifestMaxBytes = int64(64 * 1024)
const runtimeAllocationUnit = int64(4096)

func PrepareRuntime(sourceDir, workDir, epoch string) (string, error) {
	if err := ids.Validate(epoch); err != nil {
		return "", fmt.Errorf("runtime epoch %q is not a canonical UUIDv7", epoch)
	}
	source, err := openRuntimeDirectory(sourceDir)
	if err != nil {
		return "", fmt.Errorf("open runtime source directory: %w", err)
	}
	defer source.Close()
	work, err := openRuntimeDirectory(workDir)
	if err != nil {
		return "", fmt.Errorf("open worker work directory: %w", err)
	}
	defer work.Close()

	manifest, body, err := readRuntimeManifest(source, runtimeConfig(sourceDir))
	if err != nil {
		return "", err
	}
	if _, err := runtimeCorpusBytes(manifest, int64(len(body))); err != nil {
		return "", err
	}

	finalName := runtimeStagePrefix + epoch
	var stat unix.Stat_t
	if err := unix.Fstatat(int(work.Fd()), finalName, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return "", fmt.Errorf("runtime stage %q already exists", filepath.Join(workDir, finalName))
	} else if !errors.Is(err, unix.ENOENT) {
		return "", fmt.Errorf("inspect runtime stage %q: %w", filepath.Join(workDir, finalName), err)
	}
	stageName := "." + runtimeStagePrefix + uuid.Must(uuid.NewV7()).String()
	if err := unix.Mkdirat(int(work.Fd()), stageName, 0o700); err != nil {
		return "", fmt.Errorf("create runtime stage: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = removeRuntimeEntry(work, stageName)
		}
	}()
	stage, err := openRuntimeDirectoryAt(work, stageName)
	if err != nil {
		return "", fmt.Errorf("open runtime stage: %w", err)
	}
	defer stage.Close()

	for _, artifact := range []struct {
		name string
		item runtimeArtifact
	}{
		{name: "kernel", item: manifest.Kernel},
		{name: "initramfs", item: manifest.Initramfs},
		{name: "rootfs", item: manifest.Rootfs},
	} {
		if err := copyRuntimeArtifact(artifact.name, source, stage, artifact.item); err != nil {
			return "", err
		}
	}
	if err := writeRuntimeManifest(stage, body); err != nil {
		return "", err
	}
	stagedManifest, stagedBody, err := readRuntimeManifest(stage, runtimeConfig(filepath.Join(workDir, stageName)))
	if err != nil {
		return "", fmt.Errorf("verify staged runtime: %w", err)
	}
	if stagedManifest != manifest || !bytes.Equal(stagedBody, body) {
		return "", errors.New("staged runtime manifest changed")
	}
	for _, artifact := range []struct {
		name string
		item runtimeArtifact
	}{
		{name: "kernel", item: manifest.Kernel},
		{name: "initramfs", item: manifest.Initramfs},
		{name: "rootfs", item: manifest.Rootfs},
	} {
		if err := verifyRuntimeArtifact(stage, artifact.name, artifact.item); err != nil {
			return "", err
		}
	}
	if err := unix.Fchmod(int(stage.Fd()), 0o500); err != nil {
		return "", fmt.Errorf("seal runtime stage: %w", err)
	}
	if err := stage.Sync(); err != nil {
		return "", fmt.Errorf("sync runtime stage: %w", err)
	}
	if err := unix.Renameat(int(work.Fd()), stageName, int(work.Fd()), finalName); err != nil {
		return "", fmt.Errorf("publish runtime stage: %w", err)
	}
	published = true
	if err := work.Sync(); err != nil {
		return "", fmt.Errorf("sync runtime publication: %w", err)
	}
	return filepath.Join(workDir, finalName), nil
}

func CleanRuntimes(workDir, keep string) error {
	work, err := openRuntimeDirectory(workDir)
	if err != nil {
		return fmt.Errorf("open worker work directory: %w", err)
	}
	defer work.Close()
	keepName := ""
	if keep != "" {
		if !filepath.IsAbs(keep) || filepath.Clean(keep) != keep || filepath.Dir(keep) != workDir {
			return fmt.Errorf("retained runtime %q is not an epoch stage in %q", keep, workDir)
		}
		keepName = filepath.Base(keep)
		if !strings.HasPrefix(keepName, runtimeStagePrefix) {
			return fmt.Errorf("retained runtime %q is not an epoch stage in %q", keep, workDir)
		}
	}
	entries, err := work.Readdir(-1)
	if err != nil {
		return fmt.Errorf("read worker work directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == keepName {
			continue
		}
		if !strings.HasPrefix(name, runtimeStagePrefix) && !strings.HasPrefix(name, "."+runtimeStagePrefix) {
			continue
		}
		if !entry.IsDir() && entry.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("stale runtime %q is not a directory", filepath.Join(workDir, name))
		}
		if err := removeRuntimeEntry(work, name); err != nil {
			return fmt.Errorf("remove stale runtime %q: %w", filepath.Join(workDir, name), err)
		}
	}
	if err := work.Sync(); err != nil {
		return fmt.Errorf("sync runtime cleanup: %w", err)
	}
	return nil
}

func runtimeConfig(dir string) Config {
	return (Config{
		KernelPath:           filepath.Join(dir, "vmlinuz"),
		InitramfsPath:        filepath.Join(dir, "initramfs"),
		RootfsPath:           filepath.Join(dir, "rootfs.squashfs"),
		RuntimeArtifactsPath: filepath.Join(dir, "runtime-artifacts.json"),
	}).WithDefaults()
}

func readRuntimeManifest(dir *os.File, cfg Config) (runtimeArtifacts, []byte, error) {
	file, before, err := openRuntimeFileAt(dir, filepath.Base(cfg.RuntimeArtifactsPath), unix.O_RDONLY, 0)
	if err != nil {
		return runtimeArtifacts{}, nil, fmt.Errorf("open runtime artifacts manifest: %w", err)
	}
	defer file.Close()
	if before.Size <= 0 || before.Size > runtimeManifestMaxBytes {
		return runtimeArtifacts{}, nil, fmt.Errorf("runtime artifacts manifest size %d is invalid", before.Size)
	}
	body := make([]byte, before.Size)
	if _, err := io.ReadFull(file, body); err != nil {
		return runtimeArtifacts{}, nil, fmt.Errorf("read runtime artifacts manifest: %w", err)
	}
	var extra [1]byte
	if count, readErr := file.Read(extra[:]); count != 0 || !errors.Is(readErr, io.EOF) {
		return runtimeArtifacts{}, nil, errors.New("runtime artifacts manifest changed while it was read")
	}
	after, err := runtimeFileStat(file)
	if err != nil {
		return runtimeArtifacts{}, nil, fmt.Errorf("stat runtime artifacts manifest: %w", err)
	}
	if before != after {
		return runtimeArtifacts{}, nil, errors.New("runtime artifacts manifest changed while it was read")
	}
	manifest, err := decodeRuntimeArtifacts(bytes.NewReader(body))
	if err != nil {
		return runtimeArtifacts{}, nil, err
	}
	if err := validateRuntimeArtifactsManifest(cfg, manifest); err != nil {
		return runtimeArtifacts{}, nil, err
	}
	return manifest, body, nil
}

func copyRuntimeArtifact(name string, source, destination *os.File, expected runtimeArtifact) error {
	input, before, err := openRuntimeFileAt(source, expected.Path, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open runtime artifacts %s: %w", name, err)
	}
	defer input.Close()
	if before.Size != expected.SizeBytes {
		return fmt.Errorf("runtime artifacts %s size %d does not match manifest size %d", name, before.Size, expected.SizeBytes)
	}
	output, _, err := openRuntimeFileAt(
		destination,
		expected.Path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create staged runtime artifacts %s: %w", name, err)
	}
	hash := sha256.New()
	written, copyErr := io.CopyN(io.MultiWriter(output, hash), input, expected.SizeBytes)
	if copyErr == nil {
		var extra [1]byte
		count, readErr := input.Read(extra[:])
		if count != 0 || !errors.Is(readErr, io.EOF) {
			copyErr = errors.New("source contains bytes beyond its declared size")
		}
	}
	if copyErr == nil && written != expected.SizeBytes {
		copyErr = fmt.Errorf("copied %d bytes, want %d", written, expected.SizeBytes)
	}
	if copyErr == nil && sha256sum.DigestHash(hash) != expected.Digest {
		copyErr = errors.New("source digest does not match manifest")
	}
	if copyErr == nil {
		after, statErr := runtimeFileStat(input)
		if statErr != nil {
			copyErr = statErr
		} else if before != after {
			copyErr = errors.New("source changed while it was copied")
		}
	}
	if copyErr == nil {
		copyErr = output.Chmod(0o444)
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("copy runtime artifacts %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close staged runtime artifacts %s: %w", name, closeErr)
	}
	return nil
}

func writeRuntimeManifest(destination *os.File, body []byte) error {
	output, _, err := openRuntimeFileAt(
		destination,
		"runtime-artifacts.json",
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create staged runtime artifacts manifest: %w", err)
	}
	written, writeErr := output.Write(body)
	if writeErr == nil && written != len(body) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = output.Chmod(0o444)
	}
	if writeErr == nil {
		writeErr = output.Sync()
	}
	closeErr := output.Close()
	if writeErr != nil {
		return fmt.Errorf("write runtime artifacts manifest: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close staged runtime artifacts manifest: %w", closeErr)
	}
	return nil
}

func verifyRuntimeArtifact(dir *os.File, name string, expected runtimeArtifact) error {
	file, before, err := openRuntimeFileAt(dir, expected.Path, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open staged runtime artifacts %s: %w", name, err)
	}
	defer file.Close()
	if before.Size != expected.SizeBytes {
		return fmt.Errorf("staged runtime artifacts %s size %d does not match manifest size %d", name, before.Size, expected.SizeBytes)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return fmt.Errorf("hash staged runtime artifacts %s: %w", name, err)
	}
	after, err := runtimeFileStat(file)
	if err != nil {
		return fmt.Errorf("stat staged runtime artifacts %s: %w", name, err)
	}
	if before != after {
		return fmt.Errorf("staged runtime artifacts %s changed while it was verified", name)
	}
	if written != expected.SizeBytes || sha256sum.DigestHash(hash) != expected.Digest {
		return fmt.Errorf("staged runtime artifacts %s does not match its manifest", name)
	}
	return nil
}

type runtimeStat struct {
	Device  uint64
	Inode   uint64
	Size    int64
	MTimeNS int64
	CTimeNS int64
}

func openRuntimeDirectory(path string) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%q must be absolute and canonical", path)
	}
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, how)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openRuntimeDirectoryAt(parent *os.File, name string) (*os.File, error) {
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(int(parent.Fd()), name, how)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openRuntimeFileAt(dir *os.File, name string, flags int, mode uint64) (*os.File, runtimeStat, error) {
	how := &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Mode:    mode,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(int(dir.Fd()), name, how)
	if err != nil {
		return nil, runtimeStat{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	stat, err := runtimeFileStat(file)
	if err != nil {
		file.Close()
		return nil, runtimeStat{}, err
	}
	return file, stat, nil
}

func runtimeFileStat(file *os.File) (runtimeStat, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return runtimeStat{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return runtimeStat{}, errors.New("source is not a regular file")
	}
	return runtimeStat{
		Device:  uint64(stat.Dev),
		Inode:   stat.Ino,
		Size:    stat.Size,
		MTimeNS: stat.Mtim.Sec*1_000_000_000 + stat.Mtim.Nsec,
		CTimeNS: stat.Ctim.Sec*1_000_000_000 + stat.Ctim.Nsec,
	}, nil
}

func removeRuntimeEntry(parent *os.File, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	dir, err := openRuntimeDirectoryAt(parent, name)
	if err != nil {
		return err
	}
	entries, readErr := dir.Readdirnames(-1)
	if readErr != nil {
		dir.Close()
		return readErr
	}
	for _, child := range entries {
		if err := removeRuntimeEntry(dir, child); err != nil {
			dir.Close()
			return err
		}
	}
	if err := dir.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

func roundedRuntimeBytes(size int64) int64 {
	return (size + runtimeAllocationUnit - 1) / runtimeAllocationUnit * runtimeAllocationUnit
}

func runtimeCorpusBytes(manifest runtimeArtifacts, manifestBytes int64) (int64, error) {
	limit := BootCorpusMaxMiB * 1024 * 1024
	if manifestBytes <= 0 || manifestBytes > runtimeManifestMaxBytes {
		return 0, fmt.Errorf("runtime artifacts manifest size %d is invalid", manifestBytes)
	}
	allocated := runtimeAllocationUnit + roundedRuntimeBytes(manifestBytes)
	for _, size := range []int64{manifest.Kernel.SizeBytes, manifest.Initramfs.SizeBytes, manifest.Rootfs.SizeBytes} {
		remaining := limit - allocated
		if size <= 0 || size > remaining {
			return 0, fmt.Errorf("runtime boot corpus exceeds %d bytes", limit)
		}
		rounded := roundedRuntimeBytes(size)
		if rounded > remaining {
			return 0, fmt.Errorf("runtime boot corpus exceeds %d bytes", limit)
		}
		allocated += rounded
	}
	return allocated, nil
}
