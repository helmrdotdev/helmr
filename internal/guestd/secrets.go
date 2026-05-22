package guestd

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	runv0 "github.com/helmrdotdev/helmr/internal/gen/helmr/run/v0"
)

func applySecrets(imageRoot, workspaceRoot string, request *runv0.RunTaskRequest, runtimeUser *resolvedRuntimeUser, env *[]string) error {
	for _, secret := range request.Secrets {
		if secret == nil {
			return errors.New("secret injection is required")
		}
		if secret.Placement == nil {
			return fmt.Errorf("secret %s placement is required", secret.Name)
		}
		switch placement := secret.Placement.Kind.(type) {
		case *runv0.Placement_Env:
			if placement.Env == nil || strings.TrimSpace(placement.Env.Name) == "" {
				return fmt.Errorf("secret %s env placement name is required", secret.Name)
			}
			envName := strings.TrimSpace(placement.Env.Name)
			if isDynamicLoaderEnvKey(envName) {
				return fmt.Errorf("secret %s env placement %q conflicts with reserved runtime environment", secret.Name, envName)
			}
			*env = setEnvValue(*env, envName, string(secret.ValueBytes))
		case *runv0.Placement_File:
			if placement.File == nil || strings.TrimSpace(placement.File.Path) == "" {
				return fmt.Errorf("secret %s file placement path is required", secret.Name)
			}
			path, err := materializedPath(imageRoot, workspaceRoot, placement.File.Path)
			if err != nil {
				return err
			}
			uid, gid, err := secretOwner(imageRoot, runtimeUser, placement.File.Owner)
			if err != nil {
				return err
			}
			ownSecret := shouldChownSecret(runtimeUser, placement.File.Owner)
			parentUID, parentGID := uid, gid
			if runtimeUser != nil {
				parentUID, parentGID = runtimeUser.UID, runtimeUser.GID
			}
			if err := mkdirAllOwned(filepath.Dir(path), 0o700, parentUID, parentGID, ownSecret); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if placement.File.Mode != nil {
				parsed, err := parseSecretMode(*placement.File.Mode)
				if err != nil {
					return fmt.Errorf("invalid secret file mode for %s: %w", placement.File.Path, err)
				}
				mode = parsed
			}
			if err := writeFileNoFollow(path, secret.ValueBytes, mode); err != nil {
				return err
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
			if ownSecret {
				if err := os.Chown(path, int(uid), int(gid)); err != nil {
					return fmt.Errorf("chown secret file %s: %w", path, err)
				}
			}
			if err := ensureRuntimeCanReadSecretFile(imageRoot, workspaceRoot, path, runtimeUser); err != nil {
				return err
			}
		case *runv0.Placement_Dir:
			if placement.Dir == nil || strings.TrimSpace(placement.Dir.Path) == "" {
				return fmt.Errorf("secret %s dir placement path is required", secret.Name)
			}
			path, err := materializedPath(imageRoot, workspaceRoot, placement.Dir.Path)
			if err != nil {
				return err
			}
			uid, gid, err := secretOwner(imageRoot, runtimeUser, placement.Dir.Owner)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o700)
			if placement.Dir.Mode != nil {
				parsed, err := parseSecretMode(*placement.Dir.Mode)
				if err != nil {
					return fmt.Errorf("invalid secret dir mode for %s: %w", placement.Dir.Path, err)
				}
				mode = parsed
			}
			ownSecret := shouldChownSecret(runtimeUser, placement.Dir.Owner)
			if err := mkdirAllOwned(path, mode, uid, gid, ownSecret); err != nil {
				return err
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
			if ownSecret {
				if err := os.Chown(path, int(uid), int(gid)); err != nil {
					return fmt.Errorf("chown secret dir %s: %w", path, err)
				}
			}
			if err := ensureRuntimeCanTraverseSecretDir(imageRoot, workspaceRoot, path, runtimeUser); err != nil {
				return err
			}
		default:
			return fmt.Errorf("secret %s placement is required", secret.Name)
		}
	}
	return nil
}

func shouldChownSecret(runtimeUser *resolvedRuntimeUser, owner *string) bool {
	return runtimeUser != nil || (owner != nil && strings.TrimSpace(*owner) != "")
}

func mkdirAllOwned(path string, mode os.FileMode, uid uint32, gid uint32, own bool) error {
	var missing []string
	for current := path; current != "" && current != string(filepath.Separator); current = filepath.Dir(current) {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if !own {
		return nil
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Chmod(missing[i], mode); err != nil {
			return err
		}
		if err := os.Chown(missing[i], int(uid), int(gid)); err != nil {
			return err
		}
	}
	return nil
}

func secretOwner(imageRoot string, runtimeUser *resolvedRuntimeUser, owner *string) (uint32, uint32, error) {
	raw := ""
	if owner != nil {
		raw = strings.TrimSpace(*owner)
	}
	if raw == "" {
		if runtimeUser == nil {
			return 0, 0, nil
		}
		return runtimeUser.UID, runtimeUser.GID, nil
	}
	identity, err := resolveUserSpec(imageRoot, raw)
	if err != nil {
		if isRootUserSpec(raw) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("resolve secret owner %q: %w", raw, err)
	}
	return identity.UID, identity.GID, nil
}

func materializedPath(imageRoot, workspaceRoot, path string) (string, error) {
	if err := validateSecretPath(path); err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		return confinedMaterializedPath(imageRoot, strings.TrimPrefix(path, "/"))
	}
	return confinedMaterializedPath(workspaceRoot, path)
}

func validateSecretPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("secret path is required")
	}
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("secret path must not contain leading or trailing whitespace: %q", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("secret path must target a file or directory: %q", path)
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return fmt.Errorf("secret path must not contain parent components: %q", path)
		}
	}
	if strings.HasPrefix(path, "/") && isReservedRuntimePath(pathpkg.Clean(filepath.ToSlash(path))) {
		return fmt.Errorf("secret path %q conflicts with reserved runtime paths", path)
	}
	return nil
}

func ensureRuntimeCanReadSecretFile(imageRoot, workspaceRoot, path string, runtimeUser *resolvedRuntimeUser) error {
	if runtimeUser == nil {
		return nil
	}
	if err := ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, filepath.Dir(path), runtimeUser); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("secret file is not a regular file: %s", path)
	}
	if !runtimeCanRead(info, runtimeUser) {
		return fmt.Errorf("secret file is not readable by runtime user %s: %s", runtimeUser.Name, path)
	}
	return nil
}

func ensureRuntimeCanTraverseSecretDir(imageRoot, workspaceRoot, path string, runtimeUser *resolvedRuntimeUser) error {
	if runtimeUser == nil {
		return nil
	}
	return ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, path, runtimeUser)
}

func ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, path string, runtimeUser *resolvedRuntimeUser) error {
	root, err := materializedRoot(imageRoot, workspaceRoot, path)
	if err != nil {
		return err
	}
	current := filepath.Clean(path)
	root = filepath.Clean(root)
	for current != root {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("secret ancestor is not a directory: %s", current)
		}
		if !runtimeCanTraverse(info, runtimeUser) {
			return fmt.Errorf("secret path is not traversable by runtime user %s: %s", runtimeUser.Name, current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func materializedRoot(imageRoot, workspaceRoot, path string) (string, error) {
	imageRoot = filepath.Clean(imageRoot)
	workspaceRoot = filepath.Clean(workspaceRoot)
	path = filepath.Clean(path)
	if path == imageRoot || strings.HasPrefix(path, imageRoot+string(filepath.Separator)) {
		if path == workspaceRoot || strings.HasPrefix(path, workspaceRoot+string(filepath.Separator)) {
			return workspaceRoot, nil
		}
		return imageRoot, nil
	}
	return "", fmt.Errorf("secret path is outside materialized roots: %s", path)
}

func runtimeCanRead(info os.FileInfo, runtimeUser *resolvedRuntimeUser) bool {
	mode := info.Mode().Perm()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode&0o004 != 0
	}
	return permissionApplies(mode, stat.Uid, stat.Gid, runtimeUser, 0o400, 0o040, 0o004)
}

func runtimeCanTraverse(info os.FileInfo, runtimeUser *resolvedRuntimeUser) bool {
	mode := info.Mode().Perm()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode&0o001 != 0
	}
	return permissionApplies(mode, stat.Uid, stat.Gid, runtimeUser, 0o100, 0o010, 0o001)
}

func permissionApplies(mode os.FileMode, uid uint32, gid uint32, runtimeUser *resolvedRuntimeUser, ownerBit os.FileMode, groupBit os.FileMode, otherBit os.FileMode) bool {
	if runtimeUser.UID == 0 {
		return true
	}
	if uid == runtimeUser.UID {
		return mode&ownerBit != 0
	}
	if gid == runtimeUser.GID {
		return mode&groupBit != 0
	}
	return mode&otherBit != 0
}

func confinedMaterializedPath(root, relative string) (string, error) {
	path, err := confinedLayerPath(root, relative)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("secret path must not be a symlink: %s", path)
	}
	return path, nil
}

func writeFileNoFollow(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func parseSecretMode(raw string) (os.FileMode, error) {
	original := raw
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "0o")
	raw = strings.TrimPrefix(raw, "0O")
	if raw == "" {
		return 0, fmt.Errorf("invalid file mode %q", original)
	}
	mode, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid file mode %q", original)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("file mode %q must only contain permission bits", original)
	}
	return os.FileMode(mode), nil
}
