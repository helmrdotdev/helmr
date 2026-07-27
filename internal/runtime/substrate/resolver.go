package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sync/singleflight"
)

const (
	Format     = "ext4"
	BuilderABI = "helmr-substrate-ext4-v0"
	LayoutABI  = "helmr-overlay-lower-rootfs-v0"
)

type Resolver struct {
	CacheDir      string
	MkfsExt4Path  string
	MaxCacheBytes int64

	group singleflight.Group
}

type Source struct {
	WorkspaceImageDigest    string `json:"workspace_image_digest"`
	WorkspaceImageMediaType string `json:"workspace_image_media_type"`
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

func OptionalCacheKey(source Source) string {
	key, err := CacheKey(source)
	if err != nil {
		return ""
	}
	return key
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
		"workspace image digest":     source.WorkspaceImageDigest,
		"workspace image media type": source.WorkspaceImageMediaType,
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
		WorkspaceImageDigest:    strings.TrimSpace(source.WorkspaceImageDigest),
		WorkspaceImageMediaType: strings.TrimSpace(source.WorkspaceImageMediaType),
	}
}
