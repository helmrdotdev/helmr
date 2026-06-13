package guestd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func resolveLaunchCwd(raw string, fallback string) (string, error) {
	cwd := strings.TrimSpace(raw)
	if cwd == "" {
		cwd = fallback
	}
	if strings.TrimSpace(cwd) == "" {
		cwd = defaultRuntimeWorkdir
	}
	if !strings.HasPrefix(cwd, "/") {
		cwd = "/" + cwd
	}
	for part := range strings.SplitSeq(cwd, "/") {
		if part == ".." {
			return "", fmt.Errorf("OCI WorkingDir %q contains unsafe path components", raw)
		}
	}
	clean := path.Clean(cwd)
	if isReservedRuntimePath(clean) {
		return "", fmt.Errorf("OCI WorkingDir %s conflicts with reserved runtime paths", clean)
	}
	return clean, nil
}

func isReservedRuntimePath(value string) bool {
	for _, reserved := range []string{"/dev", "/opt/helmr", "/proc", "/sys", "/.helmr-old-root"} {
		if value == reserved || strings.HasPrefix(value, reserved+"/") {
			return true
		}
	}
	return false
}

func prepareLaunchPath(imageRoot string, launchCwd string, user *resolvedRuntimeUser) error {
	path, err := confinedLayerPath(imageRoot, strings.TrimPrefix(launchCwd, "/"))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if user == nil {
		return nil
	}
	return os.Chown(path, int(user.UID), int(user.GID))
}

func chownTree(root string, uid uint32, gid uint32) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := info.Mode()
		if mode.IsDir() {
			if err := os.Chmod(path, mode.Perm()|0o700); err != nil {
				return err
			}
		} else if mode.IsRegular() {
			if err := os.Chmod(path, mode.Perm()|0o600); err != nil {
				return err
			}
		}
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			return err
		}
		return nil
	})
}
