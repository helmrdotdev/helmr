//go:build linux

package builder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/oci"
)

// buildSeed runs in the client/CI builder with pinned filesystem tools. It emits
// only a capacity-sized packed disk; temporary raw and expanded files are removed
// before return. The caller publishes the returned config with the artifact.
func buildSeed(ctx context.Context, image, target, scratch, mkfs, config string, capacity int64) (disk.Seed, error) {
	if capacity <= 0 || capacity%4096 != 0 {
		return disk.Seed{}, errors.New("invalid seed capacity")
	}
	if !filepath.IsAbs(mkfs) || !filepath.IsAbs(config) {
		return disk.Seed{}, errors.New("seed generator paths must be absolute")
	}
	if err := ctx.Err(); err != nil {
		return disk.Seed{}, err
	}
	dir, err := os.MkdirTemp(scratch, "seed-build-*")
	if err != nil {
		return disk.Seed{}, err
	}
	defer os.RemoveAll(dir)
	digest, _, err := fileDigest(image)
	if err != nil {
		return disk.Seed{}, err
	}
	source, err := os.Open(image)
	if err != nil {
		return disk.Seed{}, err
	}
	metadata, filesystem, unpackErr := oci.UnpackFilesystem(source, filepath.Join(dir, "contents"))
	if err := errors.Join(unpackErr, source.Close()); err != nil {
		return disk.Seed{}, err
	}
	if metadata.ManifestCount != 1 || metadata.Platform == nil || metadata.Platform.OS != "linux" || metadata.Platform.Architecture != "amd64" {
		return disk.Seed{}, errors.New("seed image must be linux/amd64")
	}
	size, err := filesystem.LogicalBytes()
	if err != nil {
		return disk.Seed{}, err
	}
	if size > capacity {
		return disk.Seed{}, errors.New("image contents exceed seed capacity")
	}
	diskPath := filepath.Join(dir, "disk.ext4")
	key := fmt.Sprintf("%s:%s:%d", definition.ComputerSeedMediaType, digest, capacity)
	if err := createExt4(ctx, mkfs, config, filesystem, diskPath, capacity, key); err != nil {
		return disk.Seed{}, err
	}
	artifact, err := disk.EncodeSeed(ctx, diskPath, target)
	if err != nil {
		return disk.Seed{}, err
	}
	return disk.Seed{Artifact: artifact, Config: metadata.Config}, nil
}
