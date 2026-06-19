package guestd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func applySecrets(imageRoot, workspaceRoot string, request *runv0.RunTaskRequest, runtimeUser *resolvedRuntimeUser, env *[]string) error {
	_, err := applySecretsWithWorkspacePaths(imageRoot, workspaceRoot, request, runtimeUser, env)
	return err
}

func applySecretsWithWorkspacePaths(imageRoot, workspaceRoot string, request *runv0.RunTaskRequest, runtimeUser *resolvedRuntimeUser, env *[]string) ([]string, error) {
	var workspaceSecretPaths []string
	for _, secret := range request.Secrets {
		if secret == nil {
			return nil, errors.New("secret injection is required")
		}
		if secret.Placement == nil {
			return nil, fmt.Errorf("secret %s placement is required", secret.Name)
		}
		switch placement := secret.Placement.Kind.(type) {
		case *runv0.Placement_Env:
			if placement.Env == nil || strings.TrimSpace(placement.Env.Name) == "" {
				return nil, fmt.Errorf("secret %s env placement name is required", secret.Name)
			}
			envName := strings.TrimSpace(placement.Env.Name)
			if isDynamicLoaderEnvKey(envName) {
				return nil, fmt.Errorf("secret %s env placement %q conflicts with reserved runtime environment", secret.Name, envName)
			}
			*env = setEnvValue(*env, envName, string(secret.ValueBytes))
		case *runv0.Placement_File:
			if placement.File == nil || strings.TrimSpace(placement.File.Path) == "" {
				return nil, fmt.Errorf("secret %s file placement path is required", secret.Name)
			}
			path, err := materializedPath(imageRoot, workspaceRoot, placement.File.Path)
			if err != nil {
				return nil, err
			}
			uid, gid, err := secretOwner(imageRoot, runtimeUser, placement.File.Owner)
			if err != nil {
				return nil, err
			}
			ownSecret := shouldChownSecret(runtimeUser, placement.File.Owner)
			parentUID, parentGID := uid, gid
			if runtimeUser != nil {
				parentUID, parentGID = runtimeUser.UID, runtimeUser.GID
			}
			if err := mkdirAllOwned(filepath.Dir(path), 0o700, parentUID, parentGID, ownSecret); err != nil {
				return nil, err
			}
			mode := os.FileMode(0o600)
			if placement.File.Mode != nil {
				parsed, err := parseSecretMode(*placement.File.Mode)
				if err != nil {
					return nil, fmt.Errorf("invalid secret file mode for %s: %w", placement.File.Path, err)
				}
				mode = parsed
			}
			if err := writeFileNoFollow(path, secret.ValueBytes, mode); err != nil {
				return nil, err
			}
			if err := os.Chmod(path, mode); err != nil {
				return nil, err
			}
			if ownSecret {
				if err := os.Chown(path, int(uid), int(gid)); err != nil {
					return nil, fmt.Errorf("chown secret file %s: %w", path, err)
				}
			}
			if err := ensureRuntimeCanReadSecretFile(imageRoot, workspaceRoot, path, runtimeUser); err != nil {
				return nil, err
			}
			if workspaceSecretPath(imageRoot, workspaceRoot, path) != "" {
				workspaceSecretPaths = append(workspaceSecretPaths, path)
			}
		case *runv0.Placement_Dir:
			if placement.Dir == nil || strings.TrimSpace(placement.Dir.Path) == "" {
				return nil, fmt.Errorf("secret %s dir placement path is required", secret.Name)
			}
			path, err := materializedPath(imageRoot, workspaceRoot, placement.Dir.Path)
			if err != nil {
				return nil, err
			}
			if filepath.Clean(path) == filepath.Clean(workspaceRoot) {
				return nil, fmt.Errorf("secret %s dir placement path must not target the workspace root", secret.Name)
			}
			uid, gid, err := secretOwner(imageRoot, runtimeUser, placement.Dir.Owner)
			if err != nil {
				return nil, err
			}
			mode := os.FileMode(0o700)
			if placement.Dir.Mode != nil {
				parsed, err := parseSecretMode(*placement.Dir.Mode)
				if err != nil {
					return nil, fmt.Errorf("invalid secret dir mode for %s: %w", placement.Dir.Path, err)
				}
				mode = parsed
			}
			ownSecret := shouldChownSecret(runtimeUser, placement.Dir.Owner)
			if err := mkdirAllOwned(path, mode, uid, gid, ownSecret); err != nil {
				return nil, err
			}
			if err := os.Chmod(path, mode); err != nil {
				return nil, err
			}
			if ownSecret {
				if err := os.Chown(path, int(uid), int(gid)); err != nil {
					return nil, fmt.Errorf("chown secret dir %s: %w", path, err)
				}
			}
			if err := ensureRuntimeCanTraverseSecretDir(imageRoot, workspaceRoot, path, runtimeUser); err != nil {
				return nil, err
			}
			if workspaceSecretPath(imageRoot, workspaceRoot, path) != "" {
				workspaceSecretPaths = append(workspaceSecretPaths, path)
			}
		default:
			return nil, fmt.Errorf("secret %s placement is required", secret.Name)
		}
	}
	return workspaceSecretPaths, nil
}

func workspaceSecretPath(imageRoot, workspaceRoot, hostPath string) string {
	root, err := materializedRoot(imageRoot, workspaceRoot, hostPath)
	if err != nil || filepath.Clean(root) != filepath.Clean(workspaceRoot) {
		return ""
	}
	return hostPath
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

func materializedPath(imageRoot, workspaceRoot, secretPath string) (string, error) {
	if err := validateSecretPath(secretPath); err != nil {
		return "", err
	}
	if filepath.IsAbs(secretPath) {
		return confinedMaterializedPath(imageRoot, strings.TrimPrefix(secretPath, "/"))
	}
	return confinedMaterializedPath(workspaceRoot, secretPath)
}

func validateSecretPath(secretPath string) error {
	if strings.TrimSpace(secretPath) == "" {
		return errors.New("secret path is required")
	}
	if secretPath != strings.TrimSpace(secretPath) {
		return fmt.Errorf("secret path must not contain leading or trailing whitespace: %q", secretPath)
	}
	clean := filepath.Clean(filepath.FromSlash(secretPath))
	if clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("secret path must target a file or directory: %q", secretPath)
	}
	for part := range strings.SplitSeq(filepath.ToSlash(secretPath), "/") {
		if part == ".." {
			return fmt.Errorf("secret path must not contain parent components: %q", secretPath)
		}
	}
	if strings.HasPrefix(secretPath, "/") && isReservedRuntimePath(path.Clean(filepath.ToSlash(secretPath))) {
		return fmt.Errorf("secret path %q conflicts with reserved runtime paths", secretPath)
	}
	return nil
}

func ensureRuntimeCanReadSecretFile(imageRoot, workspaceRoot, hostPath string, runtimeUser *resolvedRuntimeUser) error {
	if runtimeUser == nil {
		return nil
	}
	if err := ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, filepath.Dir(hostPath), runtimeUser); err != nil {
		return err
	}
	info, err := os.Lstat(hostPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("secret file is not a regular file: %s", hostPath)
	}
	if !runtimeCanRead(info, runtimeUser) {
		return fmt.Errorf("secret file is not readable by runtime user %s: %s", runtimeUser.Name, hostPath)
	}
	return nil
}

func ensureRuntimeCanTraverseSecretDir(imageRoot, workspaceRoot, hostPath string, runtimeUser *resolvedRuntimeUser) error {
	if runtimeUser == nil {
		return nil
	}
	return ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, hostPath, runtimeUser)
}

func ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, hostPath string, runtimeUser *resolvedRuntimeUser) error {
	root, err := materializedRoot(imageRoot, workspaceRoot, hostPath)
	if err != nil {
		return err
	}
	current := filepath.Clean(hostPath)
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

func materializedRoot(imageRoot, workspaceRoot, hostPath string) (string, error) {
	imageRoot = filepath.Clean(imageRoot)
	workspaceRoot = filepath.Clean(workspaceRoot)
	hostPath = filepath.Clean(hostPath)
	if hostPath == imageRoot || strings.HasPrefix(hostPath, imageRoot+string(filepath.Separator)) {
		if hostPath == workspaceRoot || strings.HasPrefix(hostPath, workspaceRoot+string(filepath.Separator)) {
			return workspaceRoot, nil
		}
		return imageRoot, nil
	}
	return "", fmt.Errorf("secret path is outside materialized roots: %s", hostPath)
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
	hostPath, err := confinedLayerPath(root, relative)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(hostPath)
	if errors.Is(err, os.ErrNotExist) {
		return hostPath, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("secret path must not be a symlink: %s", hostPath)
	}
	return hostPath, nil
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
