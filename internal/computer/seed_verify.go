package computer

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const seedRole = "computer-seed"

// VerifySeed checks the complete encoded artifact without allocating its logical
// disk. Deployment admission owns authentication, config, deadlines and concurrency.
func VerifySeed(ctx context.Context, source io.Reader, artifact SeedArtifact, capacity int64) error {
	if err := artifact.Validate(capacity); err != nil {
		return err
	}
	hash := sha256.New()
	bounded := &io.LimitedReader{R: source, N: artifact.Object.SizeBytes + 1}
	if _, err := filepack.VerifyFrom(ctx, io.TeeReader(bounded, hash), seedRole, capacity); err != nil {
		return err
	}
	if bounded.N != 1 || sha256sum.FormatDigest(hash.Sum(nil)) != artifact.Object.Digest {
		return errors.New("seed descriptor mismatch")
	}
	return ctx.Err()
}
