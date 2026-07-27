//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package project

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockCache(root string) (func(), error) {
	lockPath := filepath.Join(root, ".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open config cache lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock config cache: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
