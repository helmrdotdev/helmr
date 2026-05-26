//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// defaultImageRuntimeDevices mirrors the minimal default device set exposed by
// OCI-style Linux runtimes. Image-mode execution happens inside a Firecracker
// VM, but the user-selected rootfs should still see a conventional process
// runtime surface instead of Helmr host internals.
var defaultImageRuntimeDevices = []runtimeDevice{
	{name: "null", major: 1, minor: 3, mode: 0o666},
	{name: "zero", major: 1, minor: 5, mode: 0o666},
	{name: "full", major: 1, minor: 7, mode: 0o666},
	{name: "random", major: 1, minor: 8, mode: 0o666},
	{name: "urandom", major: 1, minor: 9, mode: 0o666},
	{name: "tty", major: 5, minor: 0, mode: 0o666},
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
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
		Setpgid:    true,
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
	if err := pivotIntoImageRoot(imageRoot); err != nil {
		return err
	}
	if err := syscall.Chdir(launchCwd); err != nil {
		return fmt.Errorf("chdir launch cwd: %w", err)
	}
	if err := applyAdapterRuntimeIdentity(uid, gid); err != nil {
		return err
	}
	argv := append([]string{runtimePath}, adapterArgs...)
	if err := syscall.Exec(runtimePath, argv, env); err != nil {
		return fmt.Errorf("exec adapter runtime: %w", err)
	}
	return nil
}

func pivotIntoImageRoot(imageRoot string) error {
	putOld, err := os.MkdirTemp(imageRoot, ".helmr-pivot-old-root-")
	if err != nil {
		return fmt.Errorf("create old root mount point: %w", err)
	}
	if err := syscall.PivotRoot(imageRoot, putOld); err != nil {
		_ = os.Remove(putOld)
		return fmt.Errorf("pivot image root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir image root: %w", err)
	}
	oldRoot := "/" + filepath.Base(putOld)
	if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Remove(oldRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old root mount point: %w", err)
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
	if err := syscall.Mount(imageRoot, imageRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind image root mount: %w", err)
	}
	if err := syscall.Mount("", imageRoot, "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make image root private: %w", err)
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
	if err := setupImageRuntimePTY(imageRoot); err != nil {
		return err
	}
	for _, device := range defaultImageRuntimeDevices {
		if err := createRuntimeDevice(imageRoot, device); err != nil {
			return err
		}
	}
	if err := setupImageRuntimeShm(imageRoot); err != nil {
		return err
	}
	if err := setupImageRuntimeNetworkFiles(imageRoot); err != nil {
		return err
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

func setupImageRuntimeShm(imageRoot string) error {
	shmTarget, err := imageRuntimeMountTarget(imageRoot, "dev/shm")
	if err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", shmTarget, "tmpfs", syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "mode=1777,size=64m"); err != nil {
		return fmt.Errorf("mount private /dev/shm: %w", err)
	}
	return nil
}

func setupImageRuntimePTY(imageRoot string) error {
	ptsTarget, err := imageRuntimeMountTarget(imageRoot, "dev/pts")
	if err != nil {
		return err
	}
	if err := syscall.Mount("devpts", ptsTarget, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,mode=0620,ptmxmode=0666"); err != nil {
		return fmt.Errorf("mount private devpts: %w", err)
	}
	ptmxTarget, err := imageRuntimeDeviceTarget(imageRoot, "ptmx")
	if err != nil {
		return err
	}
	if err := os.Symlink("pts/ptmx", ptmxTarget); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create /dev/ptmx: %w", err)
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

func applyAdapterRuntimeIdentity(uid uint32, gid uint32) error {
	if uid == 0 {
		// Root inside an image-mode VM is the user-defined runtime root. The
		// Firecracker VM is the isolation boundary, so keep root capabilities
		// available for tools such as Nix that need Linux namespaces.
		if err := syscall.Setgroups([]int{}); err != nil {
			return fmt.Errorf("clear supplementary groups: %w", err)
		}
		if err := syscall.Setgid(int(gid)); err != nil {
			return fmt.Errorf("setgid adapter user: %w", err)
		}
		if err := syscall.Setuid(0); err != nil {
			return fmt.Errorf("setuid adapter user: %w", err)
		}
		if err := enableRootCapabilities(); err != nil {
			return err
		}
		return nil
	}

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

func enableRootCapabilities() error {
	data := [2]unix.CapUserData{}
	for cap := uint(0); cap <= uint(unix.CAP_LAST_CAP); cap++ {
		word := cap / 32
		bit := uint32(1) << (cap % 32)
		data[word].Effective |= bit
		data[word].Permitted |= bit
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("enable root capabilities: %w", err)
	}
	return nil
}
