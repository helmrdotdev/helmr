//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const nativeLauncherDirectory = "/opt/helmr/native"
const nativeLauncherExecutable = nativeLauncherDirectory + "/launch"
const nativeLauncherSocket = nativeLauncherDirectory + "/broker.sock"

// The empty launch file is only a mount target. Bind the currently executing
// static guest binary in this private namespace; no binary copy enters the disk.
func mountNativeLauncher(imageRoot, source string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("native launcher source is not a directory")
	}
	target, err := imageRuntimeMountTarget(imageRoot, "opt/helmr/native")
	if err != nil {
		return err
	}
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind native launcher directory: %w", err)
	}
	executable := filepath.Join(target, "launch")
	info, err = os.Lstat(executable)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("native launcher mount target is not a regular file")
	}
	sourceExecutable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := syscall.Mount(sourceExecutable, executable, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind native launcher executable: %w", err)
	}
	for _, path := range []string{executable, target} {
		if err := syscall.Mount("", path, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
			return fmt.Errorf("seal native launcher mount: %w", err)
		}
	}
	return nil
}
