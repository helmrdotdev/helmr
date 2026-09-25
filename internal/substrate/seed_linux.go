//go:build linux

package substrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/oci"
)

// BuildSeed runs in the client/CI builder with pinned filesystem tools. It emits
// only a capacity-sized packed disk; temporary raw and expanded files are removed
// before return. The caller publishes the returned config with the artifact.
func BuildSeed(ctx context.Context, image, target, scratch, mkfs, config string, capacity int64) (computer.Seed, error) {
	if capacity <= 0 || capacity%4096 != 0 {
		return computer.Seed{}, errors.New("invalid seed capacity")
	}
	if !filepath.IsAbs(mkfs) || !filepath.IsAbs(config) {
		return computer.Seed{}, errors.New("seed generator paths must be absolute")
	}
	if err := ctx.Err(); err != nil {
		return computer.Seed{}, err
	}
	dir, err := os.MkdirTemp(scratch, "seed-build-*")
	if err != nil {
		return computer.Seed{}, err
	}
	defer os.RemoveAll(dir)
	digest, _, err := fileDigest(image)
	if err != nil {
		return computer.Seed{}, err
	}
	source, err := os.Open(image)
	if err != nil {
		return computer.Seed{}, err
	}
	metadata, filesystem, unpackErr := oci.UnpackFilesystem(source, filepath.Join(dir, "contents"))
	if err := errors.Join(unpackErr, source.Close()); err != nil {
		return computer.Seed{}, err
	}
	if metadata.ManifestCount != 1 || metadata.Platform == nil || metadata.Platform.OS != "linux" || metadata.Platform.Architecture != "amd64" {
		return computer.Seed{}, errors.New("seed image must be linux/amd64")
	}
	size, err := filesystem.LogicalBytes()
	if err != nil {
		return computer.Seed{}, err
	}
	if size > capacity {
		return computer.Seed{}, errors.New("image contents exceed seed capacity")
	}
	disk := filepath.Join(dir, "disk.ext4")
	key := fmt.Sprintf("%s:%s:%d", computer.SeedMediaType, digest, capacity)
	if err := createExt4(ctx, mkfs, config, filesystem, disk, capacity, key); err != nil {
		return computer.Seed{}, err
	}
	artifact, err := computer.EncodeSeed(ctx, disk, target)
	if err != nil {
		return computer.Seed{}, err
	}
	return computer.Seed{Artifact: artifact, Config: metadata.Config}, nil
}
