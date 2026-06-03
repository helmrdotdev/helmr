//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func temporaryCacheRoot() (string, error) {
	root := filepath.Join(os.TempDir(), fmt.Sprintf("helmr-adapter-cache-%d", os.Getuid()))
	return root, ensurePrivateDir(root)
}

func ensurePrivateDir(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("adapter cache root is a symlink: %s", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("adapter cache root is not a directory: %s", root)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("adapter cache root owner is unavailable: %s", root)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("adapter cache root is owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(root, 0o700); err != nil {
			return fmt.Errorf("restrict adapter cache root permissions: %w", err)
		}
		info, err = os.Lstat(root)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("adapter cache root permissions are too open: %o", info.Mode().Perm())
		}
	}
	return nil
}
