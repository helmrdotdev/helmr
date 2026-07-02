package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/localcache"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sync/singleflight"
)

const (
	Format     = "ext4"
	BuilderABI = "helmr-substrate-ext4-v0"
	LayoutABI  = "helmr-overlay-lower-rootfs-v0"

	defaultExtraBytes = int64(128 * 1024 * 1024)
)

type Resolver struct {
	CacheDir      string
	MkfsExt4Path  string
	MaxCacheBytes int64

	group singleflight.Group
}

type Source struct {
	SandboxArtifactDigest string `json:"sandbox_artifact_digest"`
	SandboxArtifactFormat string `json:"sandbox_artifact_format"`
	ImageDigest           string `json:"image_digest"`
	RootfsDigest          string `json:"rootfs_digest"`
	RuntimeABI            string `json:"runtime_abi"`
	GuestdABI             string `json:"guestd_abi"`
	AdapterABI            string `json:"adapter_abi"`
	WorkspaceMountPath    string `json:"workspace_mount_path"`
}

type Result struct {
	Path       string
	Digest     string
	Format     string
	BuilderABI string
	LayoutABI  string
	CacheKey   string
	SizeBytes  int64
}

type cacheIdentity struct {
	Source     Source `json:"source"`
	Format     string `json:"format"`
	BuilderABI string `json:"builder_abi"`
	LayoutABI  string `json:"layout_abi"`
}

type cacheMetadata struct {
	CacheKey   string        `json:"cache_key"`
	Digest     string        `json:"digest"`
	Format     string        `json:"format"`
	BuilderABI string        `json:"builder_abi"`
	LayoutABI  string        `json:"layout_abi"`
	Source     Source        `json:"source"`
	SizeBytes  int64         `json:"size_bytes"`
	CreatedAt  time.Time     `json:"created_at"`
	Identity   cacheIdentity `json:"identity"`
}

func (r *Resolver) Resolve(ctx context.Context, imagePath string, source Source) (Result, error) {
	imagePath = strings.TrimSpace(imagePath)
	if imagePath == "" {
		return Result{}, errors.New("substrate image path is required")
	}
	if err := validateSource(source); err != nil {
		return Result{}, err
	}
	cacheDir := strings.TrimSpace(r.CacheDir)
	if cacheDir == "" {
		return Result{}, errors.New("substrate cache dir is required")
	}
	mkfs := strings.TrimSpace(r.MkfsExt4Path)
	if mkfs == "" {
		mkfs = "mkfs.ext4"
	}
	key, err := CacheKey(source)
	if err != nil {
		return Result{}, err
	}
	value, err, _ := r.group.Do(key, func() (any, error) {
		return r.resolveLocked(ctx, imagePath, source, key, cacheDir, mkfs)
	})
	if err != nil {
		return Result{}, err
	}
	result, ok := value.(Result)
	if !ok {
		return Result{}, errors.New("substrate resolver returned unexpected result")
	}
	return result, nil
}

func (r *Resolver) LookupDigest(_ context.Context, digest string) (Result, error) {
	cacheDir := strings.TrimSpace(r.CacheDir)
	if cacheDir == "" {
		return Result{}, errors.New("substrate cache dir is required")
	}
	digest = strings.TrimSpace(digest)
	path, err := digestPath(cacheDir, digest)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, err
	}
	if err := validateCachedDigestFile(path, digest, info.Size()); err != nil {
		return Result{}, err
	}
	if err := localcache.Touch(path); err != nil {
		return Result{}, err
	}
	return Result{
		Path:       path,
		Digest:     digest,
		Format:     Format,
		BuilderABI: BuilderABI,
		LayoutABI:  LayoutABI,
		SizeBytes:  info.Size(),
	}, nil
}

func CacheKey(source Source) (string, error) {
	if err := validateSource(source); err != nil {
		return "", err
	}
	body, err := json.Marshal(cacheIdentity{
		Source:     normalizeSource(source),
		Format:     Format,
		BuilderABI: BuilderABI,
		LayoutABI:  LayoutABI,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return sha256sum.Prefix + hex.EncodeToString(sum[:]), nil
}

func (r *Resolver) resolveLocked(ctx context.Context, imagePath string, source Source, key string, cacheDir string, mkfs string) (Result, error) {
	identity := cacheIdentity{
		Source:     normalizeSource(source),
		Format:     Format,
		BuilderABI: BuilderABI,
		LayoutABI:  LayoutABI,
	}
	if result, err := readCachedResult(cacheDir, key, identity); err == nil {
		return result, nil
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "tmp"), 0o755); err != nil {
		return Result{}, fmt.Errorf("create substrate cache tmp dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "by-digest", "sha256"), 0o755); err != nil {
		return Result{}, fmt.Errorf("create substrate digest dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "by-key", "sha256"), 0o755); err != nil {
		return Result{}, fmt.Errorf("create substrate key dir: %w", err)
	}
	if err := enforceSubstrateCacheBudget(cacheDir, r.MaxCacheBytes, nil); err != nil {
		return Result{}, fmt.Errorf("evict substrate cache: %w", err)
	}
	buildDir, err := os.MkdirTemp(filepath.Join(cacheDir, "tmp"), "build-*")
	if err != nil {
		return Result{}, fmt.Errorf("create substrate build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)
	rootfsDir := filepath.Join(buildDir, "rootfs")
	imageFile, err := os.Open(imagePath)
	if err != nil {
		return Result{}, fmt.Errorf("open substrate image: %w", err)
	}
	_, unpackErr := oci.Unpack(imageFile, rootfsDir)
	closeErr := imageFile.Close()
	if unpackErr != nil {
		return Result{}, fmt.Errorf("unpack substrate image: %w", unpackErr)
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close substrate image: %w", closeErr)
	}
	diskSize, err := substrateDiskSize(rootfsDir)
	if err != nil {
		return Result{}, err
	}
	stagedPath := filepath.Join(buildDir, "substrate.ext4")
	if err := createExt4(ctx, mkfs, rootfsDir, stagedPath, diskSize, key); err != nil {
		return Result{}, err
	}
	digest, sizeBytes, err := fileDigest(stagedPath)
	if err != nil {
		return Result{}, err
	}
	finalPath, err := digestPath(cacheDir, digest)
	if err != nil {
		return Result{}, err
	}
	if err := publishDigestFile(stagedPath, finalPath, digest); err != nil {
		return Result{}, err
	}
	metadata := cacheMetadata{
		CacheKey:   key,
		Digest:     digest,
		Format:     Format,
		BuilderABI: BuilderABI,
		LayoutABI:  LayoutABI,
		Source:     normalizeSource(source),
		SizeBytes:  sizeBytes,
		CreatedAt:  time.Now().UTC(),
		Identity:   identity,
	}
	if err := publishMetadata(cacheDir, key, metadata); err != nil {
		return Result{}, err
	}
	if err := enforceSubstrateCacheBudget(cacheDir, r.MaxCacheBytes, map[string]bool{finalPath: true}); err != nil {
		return Result{}, fmt.Errorf("evict substrate cache: %w", err)
	}
	return resultFromMetadata(cacheDir, metadata)
}

func validateSource(source Source) error {
	source = normalizeSource(source)
	required := map[string]string{
		"sandbox artifact digest": source.SandboxArtifactDigest,
		"sandbox artifact format": source.SandboxArtifactFormat,
		"image digest":            source.ImageDigest,
		"rootfs digest":           source.RootfsDigest,
		"runtime abi":             source.RuntimeABI,
		"guestd abi":              source.GuestdABI,
		"adapter abi":             source.AdapterABI,
	}
	for label, value := range required {
		if value == "" {
			return fmt.Errorf("substrate %s is required", label)
		}
	}
	return nil
}

func normalizeSource(source Source) Source {
	return Source{
		SandboxArtifactDigest: strings.TrimSpace(source.SandboxArtifactDigest),
		SandboxArtifactFormat: strings.TrimSpace(source.SandboxArtifactFormat),
		ImageDigest:           strings.TrimSpace(source.ImageDigest),
		RootfsDigest:          strings.TrimSpace(source.RootfsDigest),
		RuntimeABI:            strings.TrimSpace(source.RuntimeABI),
		GuestdABI:             strings.TrimSpace(source.GuestdABI),
		AdapterABI:            strings.TrimSpace(source.AdapterABI),
		WorkspaceMountPath:    strings.TrimSpace(source.WorkspaceMountPath),
	}
}

func readCachedResult(cacheDir string, key string, expected cacheIdentity) (Result, error) {
	path, err := keyPath(cacheDir, key)
	if err != nil {
		return Result{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var metadata cacheMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		_ = os.Remove(path)
		return Result{}, err
	}
	if strings.TrimSpace(metadata.CacheKey) != strings.TrimSpace(key) || metadata.Identity != expected {
		_ = os.Remove(path)
		return Result{}, errors.New("cached substrate identity mismatch")
	}
	result, err := resultFromMetadata(cacheDir, metadata)
	if err != nil {
		_ = os.Remove(path)
		return Result{}, err
	}
	if err := validateCachedDigestFile(result.Path, result.Digest, result.SizeBytes); err != nil {
		_ = os.Remove(path)
		return Result{}, err
	}
	if err := localcache.Touch(path); err != nil {
		_ = os.Remove(path)
		return Result{}, err
	}
	if err := localcache.Touch(result.Path); err != nil {
		_ = os.Remove(path)
		return Result{}, err
	}
	return result, nil
}

func resultFromMetadata(cacheDir string, metadata cacheMetadata) (Result, error) {
	if metadata.Format != Format {
		return Result{}, fmt.Errorf("cached substrate format %q does not match %q", metadata.Format, Format)
	}
	if metadata.BuilderABI != BuilderABI || metadata.LayoutABI != LayoutABI {
		return Result{}, errors.New("cached substrate builder identity mismatch")
	}
	path, err := digestPath(cacheDir, metadata.Digest)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Path:       path,
		Digest:     strings.TrimSpace(metadata.Digest),
		Format:     metadata.Format,
		BuilderABI: metadata.BuilderABI,
		LayoutABI:  metadata.LayoutABI,
		CacheKey:   metadata.CacheKey,
		SizeBytes:  metadata.SizeBytes,
	}, nil
}

func substrateDiskSize(rootfsDir string) (int64, error) {
	var total int64
	if err := filepath.WalkDir(rootfsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			total += info.Size()
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			total += int64(len(target))
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("measure substrate rootfs: %w", err)
	}
	size := total + defaultExtraBytes
	const minSize = int64(256 * 1024 * 1024)
	if size < minSize {
		size = minSize
	}
	const block = int64(4 * 1024 * 1024)
	if rem := size % block; rem != 0 {
		size += block - rem
	}
	return size, nil
}

func createExt4(ctx context.Context, mkfs string, rootfsDir string, path string, sizeBytes int64, key string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create substrate ext4: %w", err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("size substrate ext4: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close substrate ext4: %w", err)
	}
	uuid := deterministicUUID(key)
	cmd := exec.CommandContext(ctx, mkfs,
		"-F",
		"-q",
		"-U", uuid,
		"-E", "hash_seed="+uuid+",lazy_itable_init=0,lazy_journal_init=0",
		"-d", rootfsDir,
		path,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs substrate ext4: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func deterministicUUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	raw := sum[:16]
	raw[6] = (raw[6] & 0x0f) | 0x50
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(raw)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := copyHash(hash, file)
	if err != nil {
		return "", 0, err
	}
	return sha256sum.DigestHash(hash), size, nil
}

func copyHash(hash hash.Hash, reader io.Reader) (int64, error) {
	return io.Copy(hash, reader)
}

func validateDigestFile(path string, digest string) error {
	actual, _, err := fileDigest(path)
	if err != nil {
		return err
	}
	if actual != strings.TrimSpace(digest) {
		return fmt.Errorf("substrate digest mismatch: got %s want %s", actual, digest)
	}
	return nil
}

func validateCachedDigestFile(path string, digest string, sizeBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("cached substrate is not a regular file")
	}
	if sizeBytes > 0 && info.Size() != sizeBytes {
		return fmt.Errorf("cached substrate size mismatch: got %d want %d", info.Size(), sizeBytes)
	}
	if err := validateDigestFile(path, digest); err != nil {
		return err
	}
	return nil
}

func enforceSubstrateCacheBudget(cacheDir string, maxBytes int64, preserve map[string]bool) error {
	digestDir := filepath.Join(cacheDir, "by-digest", "sha256")
	preserve, err := substrateCachePreserveSet(digestDir, preserve)
	if err != nil {
		return err
	}
	if _, err := localcache.EnforceByteLimit(digestDir, maxBytes, preserve); err != nil {
		return err
	}
	return pruneDanglingSubstrateMetadata(cacheDir)
}

func substrateCachePreserveSet(digestDir string, explicit map[string]bool) (map[string]bool, error) {
	preserve := cleanPathSet(explicit)
	if _, err := os.Stat(digestDir); err != nil {
		if os.IsNotExist(err) {
			return preserve, nil
		}
		return nil, err
	}
	if err := filepath.WalkDir(digestDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !fileHasExternalLinks(info) {
			return nil
		}
		if preserve == nil {
			preserve = make(map[string]bool)
		}
		preserve[filepath.Clean(path)] = true
		return nil
	}); err != nil {
		return nil, err
	}
	return preserve, nil
}

func fileHasExternalLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}

func pruneDanglingSubstrateMetadata(cacheDir string) error {
	keyDir := filepath.Join(cacheDir, "by-key", "sha256")
	if _, err := os.Stat(keyDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(keyDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var metadata cacheMetadata
		if err := json.Unmarshal(body, &metadata); err != nil {
			return os.Remove(path)
		}
		digest, err := digestPath(cacheDir, metadata.Digest)
		if err != nil {
			return os.Remove(path)
		}
		if _, err := os.Stat(digest); err != nil {
			if os.IsNotExist(err) {
				return os.Remove(path)
			}
			return err
		}
		return nil
	})
}

func cleanPathSet(paths map[string]bool) map[string]bool {
	if len(paths) == 0 {
		return nil
	}
	cleaned := make(map[string]bool, len(paths))
	for path, keep := range paths {
		if keep {
			cleaned[filepath.Clean(path)] = true
		}
	}
	return cleaned
}

func publishDigestFile(stagedPath string, finalPath string, digest string) error {
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(stagedPath, 0o444); err != nil {
		return err
	}
	if err := os.Link(stagedPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return validateDigestFile(finalPath, digest)
		}
		return err
	}
	return validateDigestFile(finalPath, digest)
}

func publishMetadata(cacheDir string, key string, metadata cacheMetadata) error {
	path, err := keyPath(cacheDir, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".metadata-*")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	if _, err := staged.Write(body); err != nil {
		_ = staged.Close()
		_ = os.Remove(stagedPath)
		return err
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		_ = os.Remove(stagedPath)
		return err
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(stagedPath)
		return err
	}
	if err := os.Rename(stagedPath, path); err != nil {
		_ = os.Remove(stagedPath)
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func digestPath(cacheDir string, digest string) (string, error) {
	hexDigest, err := parseSHA256Digest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "by-digest", "sha256", hexDigest+".ext4"), nil
}

func keyPath(cacheDir string, key string) (string, error) {
	hexDigest, err := parseSHA256Digest(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "by-key", "sha256", hexDigest+".json"), nil
}

func parseSHA256Digest(digest string) (string, error) {
	hexDigest, ok := strings.CutPrefix(strings.TrimSpace(digest), sha256sum.Prefix)
	if !ok || len(hexDigest) != 64 {
		return "", fmt.Errorf("invalid sha256 digest %q", digest)
	}
	for _, r := range hexDigest {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return "", fmt.Errorf("invalid sha256 digest %q", digest)
		}
	}
	return hexDigest, nil
}
