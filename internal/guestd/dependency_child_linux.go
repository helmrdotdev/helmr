//go:build linux

package guestd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	dependencyReadyFD       = 3
	dependencySecureNoRoot  = 1 << 0
	dependencySecureNoRootL = 1 << 1
	dependencySecureNoFixup = 1 << 2
	dependencySecureNoFixL  = 1 << 3
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == dependencyProcessInitArg {
		if err := runDependencyProcessInit(os.Args[2:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "helmr dependency process init: %s\n", err)
		}
		os.Exit(127)
	}
}

func runDependencyProcessInit(args []string) error {
	if len(args) != 1 {
		return errors.New("dependency process config is missing")
	}
	raw, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil || len(raw) == 0 || len(raw) > 128<<10 {
		return errors.New("dependency process config encoding is invalid")
	}
	var config dependencyProcessConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode dependency process config: %w", err)
	}
	if err := ensureDependencyConfigEOF(decoder); err != nil {
		return err
	}
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("dependency process did not start as namespaced root PID 1")
	}
	if err := validateDependencyProcessConfig(config); err != nil {
		return err
	}
	if err := setupDependencyProcessRoot(config); err != nil {
		return err
	}
	if err := syscall.Chdir(config.Command.CWD); err != nil {
		return fmt.Errorf("chdir dependency process cwd: %w", err)
	}
	if err := applyDependencyIdentity(
		uint32(config.Identity.UID),
		uint32(config.Identity.GID),
	); err != nil {
		return err
	}
	if err := closeDependencyAmbientDescriptors(); err != nil {
		return err
	}
	if err := installDependencySeccomp(); err != nil {
		return err
	}
	if _, err := unix.Write(dependencyReadyFD, []byte{1}); err != nil {
		return fmt.Errorf("write dependency process readiness: %w", err)
	}
	if err := unix.Close(dependencyReadyFD); err != nil {
		return fmt.Errorf("close dependency process readiness: %w", err)
	}
	argv := append([]string(nil), config.Command.Argv...)
	environment := make([]string, 0, len(config.Environment))
	for _, entry := range config.Environment {
		environment = append(environment, entry.Name+"="+entry.Value)
	}
	if err := syscall.Exec(argv[0], argv, environment); err != nil {
		return fmt.Errorf("exec dependency process: %w", err)
	}
	return nil
}

func ensureDependencyConfigEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("dependency process config contains trailing input")
		}
		return fmt.Errorf("decode dependency process config trailing input: %w", err)
	}
	return nil
}

func validateDependencyProcessConfig(config dependencyProcessConfig) error {
	for name, path := range map[string]string{
		"manager":     config.Manager,
		"processRoot": config.ProcessRoot,
		"runtime":     config.Runtime,
		"toolchain":   config.Toolchain,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("dependency process %s path is not absolute and normalized", name)
		}
	}
	if config.Identity.UID != 65532 || config.Identity.GID != 65532 {
		return errors.New("dependency process identity is not the exact v0 identity")
	}
	if len(config.Command.Argv) == 0 ||
		!filepath.IsAbs(config.Command.Argv[0]) ||
		!filepath.IsAbs(config.Command.CWD) {
		return errors.New("dependency process command is incomplete")
	}
	for _, entry := range config.Environment {
		if entry.Name == "" || bytes.IndexByte([]byte(entry.Name), '=') >= 0 {
			return errors.New("dependency process environment is invalid")
		}
	}
	return nil
}

func setupDependencyProcessRoot(config dependencyProcessConfig) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make dependency mount namespace private: %w", err)
	}
	if err := unix.Mount(
		config.ProcessRoot,
		config.ProcessRoot,
		"",
		unix.MS_BIND,
		"",
	); err != nil {
		return fmt.Errorf("bind dependency process root: %w", err)
	}
	if err := mountDependencyWritable(config.ProcessRoot, "work"); err != nil {
		return err
	}
	if err := mountDependencyWritable(config.ProcessRoot, "tmp"); err != nil {
		return err
	}
	if err := mountDependencyDevices(config.ProcessRoot); err != nil {
		return err
	}
	for _, mount := range []struct {
		noexec bool
		source string
		target string
	}{
		{source: config.Manager, target: "/opt/helmr/manager"},
		{source: config.Runtime, target: "/opt/helmr/runtime"},
		{source: config.Toolchain, target: "/nix"},
	} {
		if mount.source == "" {
			continue
		}
		if err := mountDependencyComponent(
			config.ProcessRoot,
			mount.source,
			mount.target,
			mount.noexec,
		); err != nil {
			return err
		}
	}
	if err := pivotIntoImageRoot(config.ProcessRoot); err != nil {
		return err
	}
	if err := unix.Mount(
		"",
		"/",
		"",
		unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|
			unix.MS_NOSUID|unix.MS_NODEV,
		"",
	); err != nil {
		return fmt.Errorf("remount dependency process root read-only: %w", err)
	}
	for _, alias := range config.Aliases {
		info, err := os.Stat(alias.Path)
		if err != nil {
			return fmt.Errorf("stat dependency process alias %q: %w", alias.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("dependency process alias %q is not a regular file", alias.Path)
		}
	}
	info, err := os.Stat(config.Command.Argv[0])
	if err != nil {
		return fmt.Errorf("stat dependency process executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("dependency process executable is not executable")
	}
	return nil
}

func mountDependencyWritable(root string, relative string) error {
	path := filepath.Join(root, relative)
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind dependency writable path %q: %w", relative, err)
	}
	if err := unix.Mount(
		"",
		path,
		"",
		unix.MS_REMOUNT|unix.MS_BIND|unix.MS_NOSUID|unix.MS_NODEV,
		"",
	); err != nil {
		return fmt.Errorf("remount dependency writable path %q: %w", relative, err)
	}
	return nil
}

func mountDependencyDevices(root string) error {
	path := filepath.Join(root, "dev")
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind dependency process devices: %w", err)
	}
	if err := unix.Mount(
		"",
		path,
		"",
		unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|
			unix.MS_NOSUID|unix.MS_NOEXEC,
		"",
	); err != nil {
		return fmt.Errorf("remount dependency process devices: %w", err)
	}
	return nil
}

func mountDependencyComponent(
	root string,
	source string,
	absoluteTarget string,
	noexec bool,
) error {
	target := filepath.Join(root, absoluteTarget[1:])
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create dependency component target %q: %w", absoluteTarget, err)
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind dependency component %q: %w", absoluteTarget, err)
	}
	flags := uintptr(
		unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY |
			unix.MS_NOSUID | unix.MS_NODEV,
	)
	if noexec {
		flags |= unix.MS_NOEXEC
	}
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("remount dependency component %q: %w", absoluteTarget, err)
	}
	return nil
}

func applyDependencyIdentity(uid, gid uint32) error {
	if uid == 0 || gid == 0 {
		return errors.New("dependency process identity must be unprivileged")
	}
	if err := dependencyAllThreadsPrctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set dependency process no_new_privs: %w", err)
	}
	secureBits := dependencySecureNoRoot |
		dependencySecureNoRootL |
		dependencySecureNoFixup |
		dependencySecureNoFixL
	if err := dependencyAllThreadsPrctl(
		unix.PR_SET_SECUREBITS,
		uintptr(secureBits),
		0,
		0,
		0,
	); err != nil {
		return fmt.Errorf("lock dependency process securebits: %w", err)
	}
	if err := dependencyAllThreadsPrctl(
		unix.PR_CAP_AMBIENT,
		unix.PR_CAP_AMBIENT_CLEAR_ALL,
		0,
		0,
		0,
	); err != nil {
		return fmt.Errorf("clear dependency process ambient capabilities: %w", err)
	}
	lastCapability, err := dropDependencyBoundingCapabilities()
	if err != nil {
		return err
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("clear dependency process supplementary groups: %w", err)
	}
	if err := syscall.Setresgid(int(gid), int(gid), int(gid)); err != nil {
		return fmt.Errorf("set dependency process GID: %w", err)
	}
	if err := syscall.Setresuid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("set dependency process UID: %w", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := dependencyAllThreadsCapset(&header, &data[0]); err != nil {
		return fmt.Errorf("clear dependency process capabilities: %w", err)
	}
	return validateDependencyIdentity(uid, gid, lastCapability)
}

func dependencyAllThreadsPrctl(
	option int,
	argument2 uintptr,
	argument3 uintptr,
	argument4 uintptr,
	argument5 uintptr,
) error {
	_, _, errno := syscall.AllThreadsSyscall6(
		syscall.SYS_PRCTL,
		uintptr(option),
		argument2,
		argument3,
		argument4,
		argument5,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func dropDependencyBoundingCapabilities() (uintptr, error) {
	const searchLimit = 1024
	for capability := uintptr(0); capability < searchLimit; capability++ {
		err := dependencyAllThreadsPrctl(
			unix.PR_CAPBSET_DROP,
			capability,
			0,
			0,
			0,
		)
		if errors.Is(err, syscall.EINVAL) {
			if capability == 0 {
				return 0, errors.New("kernel exposes no capability bounding set")
			}
			return capability - 1, nil
		}
		if err != nil {
			return 0, fmt.Errorf(
				"drop dependency process capability %d: %w",
				capability,
				err,
			)
		}
	}
	return 0, fmt.Errorf("kernel capability domain reaches search limit %d", searchLimit)
}

func dependencyAllThreadsCapset(
	header *unix.CapUserHeader,
	data *unix.CapUserData,
) error {
	_, _, errno := syscall.AllThreadsSyscall(
		syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(header)),
		uintptr(unsafe.Pointer(data)),
		0,
	)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	if errno != 0 {
		return errno
	}
	return nil
}

func validateDependencyIdentity(uid, gid uint32, lastCapability uintptr) error {
	ruid, euid, suid := unix.Getresuid()
	rgid, egid, sgid := unix.Getresgid()
	if ruid != int(uid) || euid != int(uid) || suid != int(uid) ||
		rgid != int(gid) || egid != int(gid) || sgid != int(gid) {
		return errors.New("dependency process identity did not become permanent")
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || noNewPrivileges != 1 {
		return errors.New("dependency process no_new_privs is not active")
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read dependency process capabilities: %w", err)
	}
	for _, word := range data {
		if word.Effective != 0 || word.Permitted != 0 || word.Inheritable != 0 {
			return errors.New("dependency process retained capabilities")
		}
	}
	for capability := uintptr(0); capability <= lastCapability; capability++ {
		present, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, capability, 0, 0, 0)
		if err != nil || present != 0 {
			return fmt.Errorf(
				"dependency process retained bounding capability %d",
				capability,
			)
		}
	}
	return nil
}

func closeDependencyAmbientDescriptors() error {
	if err := unix.CloseRange(4, ^uint(0), 0); err != nil {
		return fmt.Errorf("close dependency process ambient descriptors: %w", err)
	}
	for fd := 0; fd <= dependencyReadyFD; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			return fmt.Errorf("dependency process descriptor %d is closed: %w", fd, err)
		}
	}
	return nil
}
