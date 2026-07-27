package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultExtraBytes = int64(128 * 1024 * 1024)

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

func createExt4(
	ctx context.Context,
	mkfs string,
	rootfsDir string,
	path string,
	sizeBytes int64,
	key string,
) error {
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
	cmd := exec.CommandContext(
		ctx,
		mkfs,
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
