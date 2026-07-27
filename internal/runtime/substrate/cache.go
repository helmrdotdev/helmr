package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/localcache"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

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
		Path: path, Digest: digest, Format: Format,
		BuilderABI: BuilderABI, LayoutABI: LayoutABI, SizeBytes: info.Size(),
	}, nil
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
		Path: path, Digest: strings.TrimSpace(metadata.Digest), Format: metadata.Format,
		BuilderABI: metadata.BuilderABI, LayoutABI: metadata.LayoutABI,
		CacheKey: metadata.CacheKey, SizeBytes: metadata.SizeBytes,
	}, nil
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := copyHash(digest, file)
	if err != nil {
		return "", 0, err
	}
	return sha256sum.DigestHash(digest), size, nil
}

func copyHash(digest hash.Hash, reader io.Reader) (int64, error) {
	return io.Copy(digest, reader)
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
	return validateDigestFile(path, digest)
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
	return syncDir(filepath.Dir(path))
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
	for _, character := range hexDigest {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return "", fmt.Errorf("invalid sha256 digest %q", digest)
		}
	}
	return hexDigest, nil
}
