//go:build linux

package computer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/substrate"
)

// seedDisk creates an independent writable disk from a verified,
// immutable Sandbox ext4 image. It is for first creation only: existing
// Computers restore their committed disk and never reapply a Sandbox image.
// sizeBytes is the total filesystem capacity, not additional free space.
// The owner must publish this local candidate only after durable storage succeeds.
func seedDisk(ctx context.Context, seed *substrate.DiskSource, target string, sizeBytes int64, resize2fs string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if seed == nil || sizeBytes <= 0 || sizeBytes%4096 != 0 {
		return errors.New("computer disk source and positive block-aligned capacity are required")
	}
	// This is the same verified, cancellable projection boundary used by VM
	// startup. Do not clone that projection a second time.
	target = filepath.Clean(target)
	projected, err := seed.MaterializeInto(ctx, filepath.Dir(target), filepath.Base(target), os.Getuid(), os.Getgid())
	if err != nil {
		return fmt.Errorf("materialize computer seed: %w", err)
	}
	if projected != target {
		return errors.New("computer seed projection returned an unexpected path")
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
