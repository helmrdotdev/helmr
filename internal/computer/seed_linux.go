//go:build linux

package computer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// materialize creates a private writable disk from a published shared seed.
// It is only for first creation; continuation uses committed Computer artifacts.
func (s SeedStore) materialize(ctx context.Context, seed Seed, target string, sizeBytes int64, resize2fs string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := diskArtifactLimit(sizeBytes); err != nil {
		return err
	}
	target = filepath.Clean(target)
	if err := s.Decode(ctx, seed.Identity, seed.Artifact, target, sizeBytes); err != nil {
		return fmt.Errorf("decode computer seed: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || sizeBytes < info.Size() {
		return errors.New("computer disk capacity must fit the verified seed")
	}
	if err := os.Chmod(target, 0600); err != nil {
		return err
	}
	if sizeBytes != info.Size() {
		if err := os.Truncate(target, sizeBytes); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, resize2fs, target)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("grow computer filesystem: %w: %s", err, output)
		}
	}
	// No local durability barrier is needed: the owner must durably upload the
	// encoded candidate before publication. This working file is not authority.
	return ctx.Err()
}
