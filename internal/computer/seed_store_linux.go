//go:build linux

package computer

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// SeedStore reads admitted deployment disks. It grants no execution authority
// and never mounts, repairs, resizes or executes the submitted filesystem.
type SeedStore struct{ CAS cas.Reader }

// EncodeSeed writes a deployment artifact inside the builder, before export.
// The caller owns the stable source and exclusive target and publishes its config
// together with this descriptor. No platform key is available or required here.
func EncodeSeed(ctx context.Context, source, target string) (_ SeedArtifact, retErr error) {
	if err := ctx.Err(); err != nil {
		return SeedArtifact{}, err
	}
	input, err := os.Open(source)
	if err != nil {
		return SeedArtifact{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return SeedArtifact{}, err
	}
	if !info.Mode().IsRegular() {
		return SeedArtifact{}, errors.New("seed must be a regular file")
	}
	limit, err := diskArtifactLimit(info.Size())
	if err != nil {
		return SeedArtifact{}, err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return SeedArtifact{}, err
	}
	defer func() {
		if retErr != nil {
			_ = output.Close()
			_ = os.Remove(target)
		}
	}()
	hash := sha256.New()
	writer := &boundedWriter{writer: io.MultiWriter(output, hash), remaining: limit}
	stats, err := filepack.PackTo(ctx, input, writer, seedRole)
	if err != nil {
		return SeedArtifact{}, err
	}
	if stats.LogicalBytes != info.Size() {
		return SeedArtifact{}, errors.New("seed changed during encoding")
	}
	if err := output.Chmod(0400); err != nil {
		return SeedArtifact{}, err
	}
	if err := output.Close(); err != nil {
		return SeedArtifact{}, err
	}
	return SeedArtifact{Object: cas.Descriptor{Digest: sha256sum.FormatDigest(hash.Sum(nil)), SizeBytes: limit - writer.remaining, MediaType: SeedMediaType}, LogicalBytes: info.Size()}, nil
}

// Decode verifies exact capacity and descriptor before exposing an independent
// writable disk. The owner supplies bounded CAS I/O and its operation deadline.
func (s SeedStore) Decode(ctx context.Context, artifact SeedArtifact, target string, capacity int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := artifact.Validate(capacity); err != nil {
		return err
	}
	if s.CAS == nil {
		return errors.New("seed storage is required")
	}
	if _, err := os.Lstat(target); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(target), ".seed-decode-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	body, err := s.CAS.Get(ctx, artifact.Object.Digest)
	if err != nil {
		return err
	}
	defer body.Close()
	hash := sha256.New()
	bounded := &io.LimitedReader{R: &contextReader{ctx, body}, N: artifact.Object.SizeBytes + 1}
	restored := filepath.Join(dir, "disk.raw")
	if _, err := filepack.UnpackFrom(ctx, io.TeeReader(bounded, hash), restored, seedRole, capacity); err != nil {
		return err
	}
	if bounded.N != 1 || sha256sum.FormatDigest(hash.Sum(nil)) != artifact.Object.Digest {
		return errors.New("seed descriptor mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Link(restored, target)
}
