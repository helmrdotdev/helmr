//go:build linux && computerproof

package guestd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// mountComputerRoot assembles a view of an already mounted customer filesystem.
// The supervisor owns a private mount namespace and must exclude all user scopes
// while assembling or removing this view. Sources are supervisor-owned mounts;
// runtimeFiles contains only guest-visible files, never supervisor journals.
// This primitive is not yet wired into the production Workspace launcher.
func mountComputerRoot(customerRoot, target, program, runtime, runtimeFiles string) (func() error, error) {
	var mounted []string
	cleanup := func() error {
		// Do not detach busy mounts or delete customer data. A failed unmount retains
		// its entry so the owner can stop remaining users and retry cleanup.
		for len(mounted) > 0 {
			path := mounted[len(mounted)-1]
			if err := unix.Unmount(path, 0); err != nil {
				return fmt.Errorf("unmount computer %s: %w", path, err)
			}
			mounted = mounted[:len(mounted)-1]
		}
		return nil
	}
	fail := func(err error) (func() error, error) { return cleanup, errors.Join(err, cleanup()) }
	for _, path := range []string{customerRoot, target, program, runtime, runtimeFiles} {
		info, err := os.Lstat(path)
		if err != nil {
			return cleanup, err
		}
		if !filepath.IsAbs(path) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return cleanup, fmt.Errorf("computer mount source/target must be an absolute directory: %s", path)
		}
	}
	if err := unix.Mount(customerRoot, target, "", unix.MS_BIND, ""); err != nil {
		return cleanup, fmt.Errorf("bind computer root: %w", err)
	}
	mounted = append(mounted, target)
	if err := unix.Mount("", target, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fail(err)
	}
	for _, rel := range []string{"opt/helmr", "run/helmr", "var/lib/helmr"} {
		path, err := imageRuntimeMountTarget(target, rel)
		if err != nil {
			return fail(err)
		}
		if err := unix.Mount("tmpfs", path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=755"); err != nil {
			return fail(fmt.Errorf("mask computer reserved path %s: %w", rel, err))
		}
		mounted = append(mounted, path)
	}
	for _, mount := range []struct {
		source, relative string
		noexec           bool
	}{
		{program, "opt/helmr/program", false},
		{runtime, "opt/helmr/runtime", false},
		{runtimeFiles, "run/helmr", true},
	} {
		path, err := imageRuntimeMountTarget(target, mount.relative)
		if err != nil {
			return fail(err)
		}
		// A nonrecursive bind imports this source filesystem only. Managed artifacts
		// and runtime files must not import arbitrary supervisor submounts.
		if err := unix.Mount(mount.source, path, "", unix.MS_BIND, ""); err != nil {
			return fail(err)
		}
		mounted = append(mounted, path)
		flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
		if mount.noexec {
			flags |= unix.MS_NOEXEC
		}
		if err := unix.Mount("", path, "", flags, ""); err != nil {
			return fail(err)
		}
	}
	// Reserved parent directories are supervisor-owned too; callers must not be
	// able to create untracked files beside the pinned artifact mounts.
	for _, rel := range []string{"opt/helmr", "var/lib/helmr"} {
		if err := unix.Mount("", filepath.Join(target, rel), "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return fail(err)
		}
	}
	return cleanup, nil
}
