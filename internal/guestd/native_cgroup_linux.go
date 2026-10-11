//go:build linux

package guestd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The guest owns all descriptors and launches directly into this subgroup.
// Passing a cgroup control descriptor to workload code would also grant it the
// ability to move other visible processes, even if asked to move only itself.
func createNativeProcessCgroup(parent *linuxProcessCgroup, scopeID string) (*linuxProcessCgroup, error) {
	if parent == nil || parent.file == nil || scopeID == "" {
		return nil, errors.New("native process scope requires a live parent and identity")
	}
	if filepath.Dir(parent.path) != processCgroupRoot {
		return nil, errors.New("native process parent must be a Session cgroup")
	}
	if err := validateProcessCgroupLeaf(filepath.Base(parent.path)); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(scopeID))
	leaf := "native-" + hex.EncodeToString(digest[:16])
	fd := int(parent.file.Fd())
	if err := unix.Mkdirat(fd, leaf, 0o755); err != nil {
		// A collision never authorizes killing an existing native process.
		return nil, fmt.Errorf("create native process scope: %w", err)
	}
	child, err := unix.Openat(fd, leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Unlinkat(fd, leaf, unix.AT_REMOVEDIR)
		return nil, fmt.Errorf("open native process scope: %w", err)
	}
	path := filepath.Join(parent.path, leaf)
	return &linuxProcessCgroup{path: path, file: os.NewFile(uintptr(child), path)}, nil
}
