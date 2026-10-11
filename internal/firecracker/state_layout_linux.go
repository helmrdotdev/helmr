//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func networkAllocationLockPath(stateDir string) string {
	return filepath.Clean(stateDir) + ".network.lock"
}

func stateCoordinationDir(stateDir string) string {
	return filepath.Dir(filepath.Clean(stateDir))
}

func createOwnerStateRoot(stateDir string, owner vm.Owner) (string, error) {
	if err := owner.Validate(); err != nil {
		return "", fmt.Errorf("the Firecracker owner: %w", err)
	}
	if err := ensureSecureDirectory("the Firecracker coordination directory", stateCoordinationDir(stateDir)); err != nil {
		return "", err
	}
	if err := ensureSecureDirectory("the Firecracker state directory", stateDir); err != nil {
		return "", err
	}
	if err := claimInstanceIdentity(stateDir, owner); err != nil {
		return "", err
	}
	statePath := filepath.Join(stateDir, owner.ID)
	if err := os.Mkdir(statePath, 0o700); err != nil {
		return "", fmt.Errorf("create Firecracker owner state root: %w", err)
	}
	markerPath := filepath.Join(statePath, "owner")
	marker, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.Remove(statePath)
		return "", fmt.Errorf("create Firecracker ownership evidence: %w", err)
	}
	_, writeErr := marker.WriteString(string(owner.Kind) + "\n" + owner.ID + "\n")
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(markerPath)
		_ = os.Remove(statePath)
		return "", fmt.Errorf("persist Firecracker ownership evidence: %w", err)
	}
	if err := syncDirectory(statePath); err != nil {
		_ = os.Remove(markerPath)
		_ = os.Remove(statePath)
		return "", err
	}
	if err := syncDirectory(stateDir); err != nil {
		_ = os.Remove(markerPath)
		_ = os.Remove(statePath)
		return "", err
	}
	return statePath, nil
}

// An instance names one physical launch, including a failed launch. Cleanup
// removes its runtime files but must never permit the same immutable checkpoint
// to be loaded again under that identity. Keep the claim outside the runtime
// inventory so recovery/cleanup cannot erase it. A replacement requires a new
// instance and control-plane lease; reconnecting to a live VM makes no claim.
// Claims survive worker restarts and have the lifetime of the host state volume.
func claimInstanceIdentity(stateDir string, owner vm.Owner) error {
	claimsDir := filepath.Clean(stateDir) + ".instances"
	if err := ensureSecureDirectory("the Firecracker instance claims directory", claimsDir); err != nil {
		return err
	}
	if err := syncDirectory(stateCoordinationDir(stateDir)); err != nil {
		return err
	}
	claim, err := os.OpenFile(filepath.Join(claimsDir, owner.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("claim single-use Firecracker instance: %w", err)
	}
	// Never remove an uncertain claim, even if persistence or later preparation
	// fails. No process can start until both the file and directory are durable.
	if err := errors.Join(claim.Sync(), claim.Close()); err != nil {
		return fmt.Errorf("persist Firecracker instance claim: %w", err)
	}
	return syncDirectory(claimsDir)
}

func ensureSecureDirectory(label string, path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("%s %q could not be created: %w", label, path, err)
	}
	return checkSecureDirectory(label, path)
}

func checkSecureDirectory(label string, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s %q is not available: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s %q must be a non-symlink directory", label, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s %q has an unsupported stat result", label, path)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s %q must be owned by worker uid %d", label, path, os.Geteuid())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %q must not be writable by group or world", label, path)
	}
	return nil
}

func checkResolvedStateLayout(cfg Config) error {
	stateDir, err := filepath.EvalSymlinks(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("resolve Firecracker state directory: %w", err)
	}
	jailerDir, err := filepath.EvalSymlinks(cfg.JailerChrootBaseDir)
	if err != nil {
		return fmt.Errorf("resolve Firecracker jailer chroot directory: %w", err)
	}
	if pathsOverlap(stateDir, jailerDir) {
		return errors.New("resolved Firecracker state dir and jailer chroot base directory must be disjoint")
	}
	return nil
}
