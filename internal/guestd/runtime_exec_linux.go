//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const imageAdapterInitArg = "__helmr-image-adapter-init"

const (
	secureNoRoot              = 1 << 0
	secureNoRootLocked        = 1 << 1
	secureNoSetuidFixup       = 1 << 2
	secureNoSetuidFixupLocked = 1 << 3
)

type runtimeDevice struct {
	name  string
	major uint32
	minor uint32
	mode  uint32
}

var imageRuntimeDevices = []runtimeDevice{
	{name: "null", major: 1, minor: 3, mode: 0o666},
	{name: "zero", major: 1, minor: 5, mode: 0o666},
	{name: "full", major: 1, minor: 7, mode: 0o666},
	{name: "random", major: 1, minor: 8, mode: 0o666},
	{name: "urandom", major: 1, minor: 9, mode: 0o666},
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == imageAdapterInitArg {
		if err := runImageAdapterInit(os.Args[2:], os.Environ()); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "helmr image adapter init: %s\n", err)
			os.Exit(127)
		}
		os.Exit(127)
	}
}

func adapterCommand(ctx context.Context, runtimePath string, args []string, launchCwd string, env []string, imageRoot string, user *resolvedRuntimeUser, imageMode bool) (*exec.Cmd, error) {
	if !imageMode {
		cmd := exec.CommandContext(ctx, runtimePath, args...)
		cmd.Dir = launchCwd
		cmd.Env = env
		return cmd, nil
	}
	if user == nil {
		return nil, errors.New("image runtime user is required")
	}
	initArgs := []string{
		imageAdapterInitArg,
		imageRoot,
		launchCwd,
		strconv.FormatUint(uint64(user.UID), 10),
		strconv.FormatUint(uint64(user.GID), 10),
		runtimePath,
	}
	initArgs = append(initArgs, args...)
	cmd := exec.CommandContext(ctx, "/proc/self/exe", initArgs...)
	cmd.Dir = "/"
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
	}
	return cmd, nil
}

func runImageAdapterInit(args []string, env []string) error {
	if len(args) < 6 {
		return errors.New("missing image adapter init arguments")
	}
	imageRoot := args[0]
	launchCwd := args[1]
	uid, err := parseInitUint32("uid", args[2])
	if err != nil {
		return err
	}
	gid, err := parseInitUint32("gid", args[3])
	if err != nil {
		return err
	}
	runtimePath := args[4]
	adapterArgs := args[5:]
	if err := setupImageAdapterNamespace(imageRoot); err != nil {
		return err
	}
	if err := syscall.Chroot(imageRoot); err != nil {
		return fmt.Errorf("chroot image root: %w", err)
	}
	if err := syscall.Chdir(launchCwd); err != nil {
		return fmt.Errorf("chdir launch cwd: %w", err)
	}
	if err := dropAdapterPrivileges(uid, gid); err != nil {
		return err
	}
	argv := append([]string{runtimePath}, adapterArgs...)
	if err := syscall.Exec(runtimePath, argv, env); err != nil {
		return fmt.Errorf("exec adapter runtime: %w", err)
	}
	return nil
}

func parseInitUint32(name string, raw string) (uint32, error) {
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return uint32(value), nil
}

func setupImageAdapterNamespace(imageRoot string) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	procTarget, err := imageRuntimeMountTarget(imageRoot, "proc")
	if err != nil {
		return err
	}
	if err := syscall.Mount("proc", procTarget, "proc", syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount private proc: %w", err)
	}
	devTarget, err := imageRuntimeMountTarget(imageRoot, "dev")
	if err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", devTarget, "tmpfs", syscall.MS_NOSUID|syscall.MS_NOEXEC, "mode=755,size=64k"); err != nil {
		return fmt.Errorf("mount private dev tmpfs: %w", err)
	}
	for _, device := range imageRuntimeDevices {
		if err := createRuntimeDevice(imageRoot, device); err != nil {
			return err
		}
	}
	shmTarget, err := safeJoin(imageRoot, "dev/shm")
	if err != nil {
		return err
	}
	if err := os.Mkdir(shmTarget, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create /dev/shm: %w", err)
	}
	if err := os.Chmod(shmTarget, 0o777); err != nil {
		return fmt.Errorf("chmod /dev/shm: %w", err)
	}
	for _, link := range []struct {
		name   string
		target string
	}{
		{name: "fd", target: "/proc/self/fd"},
		{name: "stdin", target: "/proc/self/fd/0"},
		{name: "stdout", target: "/proc/self/fd/1"},
		{name: "stderr", target: "/proc/self/fd/2"},
	} {
		target, err := imageRuntimeDeviceTarget(imageRoot, link.name)
		if err != nil {
			return err
		}
		if err := os.Symlink(link.target, target); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create /dev/%s: %w", link.name, err)
		}
	}
	return nil
}

func createRuntimeDevice(imageRoot string, device runtimeDevice) error {
	target, err := imageRuntimeDeviceTarget(imageRoot, device.name)
	if err != nil {
		return err
	}
	mode := uint32(syscall.S_IFCHR) | device.mode
	if err := syscall.Mknod(target, mode, int(unix.Mkdev(device.major, device.minor))); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create /dev/%s: %w", device.name, err)
	}
	return nil
}

func dropAdapterPrivileges(uid uint32, gid uint32) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	secureBits := secureNoRoot | secureNoRootLocked | secureNoSetuidFixup | secureNoSetuidFixupLocked
	if err := unix.Prctl(unix.PR_SET_SECUREBITS, uintptr(secureBits), 0, 0, 0); err != nil {
		return fmt.Errorf("lock securebits: %w", err)
	}
	for cap := uintptr(0); cap <= unix.CAP_LAST_CAP; cap++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, cap, 0, 0, 0); err != nil && !errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("drop capability bounding set: %w", err)
		}
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("clear supplementary groups: %w", err)
	}
	if err := syscall.Setgid(int(gid)); err != nil {
		return fmt.Errorf("setgid adapter user: %w", err)
	}
	if err := syscall.Setuid(int(uid)); err != nil {
		return fmt.Errorf("setuid adapter user: %w", err)
	}
	data := [2]unix.CapUserData{}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("clear capabilities: %w", err)
	}
	return nil
}
