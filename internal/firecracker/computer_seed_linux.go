//go:build linux && computerproof

package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// seedComputerDisk creates an independent writable disk from a verified,
// immutable Sandbox ext4 image. It is for first creation only: existing
// Computers restore their committed disk and never reapply a Sandbox image.
// sizeBytes is the total filesystem capacity, not additional free space.
// The owner must publish this local candidate only after durable storage succeeds.
func seedComputerDisk(ctx context.Context, seed, target string, sizeBytes int64, resize2fs string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(seed)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || sizeBytes < info.Size() || sizeBytes <= 0 || sizeBytes%4096 != 0 {
		return fmt.Errorf("computer disk capacity %d must fit the regular seed image and be block aligned", sizeBytes)
	}
	// Never hard-link the shared seed. The existing sparse copier creates an
	// exclusive target and removes partial copies on failure.
	if err := cloneSparseFile(seed, target); err != nil {
		return fmt.Errorf("clone computer seed: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	if err := ctx.Err(); err != nil {
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
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
