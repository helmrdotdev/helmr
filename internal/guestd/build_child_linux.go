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
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	buildReadyFD       = 3
	buildSupervisorFD  = 4
	buildResolverPath  = "/run/resolv.conf"
	buildSecureNoRoot  = 1 << 0
	buildSecureNoRootL = 1 << 1
	buildSecureNoFixup = 1 << 2
	buildSecureNoFixL  = 1 << 3
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == buildProcessInitArg {
		if err := runBuildProcessInit(os.Args[2:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "helmr build process init: %s\n", err)
		}
		os.Exit(127)
	}
}

func runBuildProcessInit(args []string) error {
	if len(args) != 1 {
		return errors.New("build process config is missing")
	}
	raw, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil || len(raw) == 0 || len(raw) > 128<<10 {
		return errors.New("build process config encoding is invalid")
	}
	var config buildProcessConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode build process config: %w", err)
	}
	if err := ensureBuildConfigEOF(decoder); err != nil {
		return err
	}
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("build process did not start as namespaced root PID 1")
	}
	if err := validateBuildProcessConfig(config); err != nil {
		return err
	}
	if err := setupBuildProcessRoot(config); err != nil {
		return err
	}
	if err := syscall.Chdir(config.Command.CWD); err != nil {
		return fmt.Errorf("chdir build process cwd: %w", err)
	}
	if err := applyBuildIdentity(
		uint32(config.Identity.UID),
		uint32(config.Identity.GID),
	); err != nil {
		return err
	}
	if err := closeBuildAmbientDescriptors(config.Supervisor); err != nil {
		return err
	}
	if err := installBuildSeccomp(); err != nil {
		return err
	}
	if _, err := unix.Write(buildReadyFD, []byte{1}); err != nil {
		return fmt.Errorf("write build process readiness: %w", err)
	}
	if err := unix.Close(buildReadyFD); err != nil {
		return fmt.Errorf("close build process readiness: %w", err)
	}
	argv := append([]string(nil), config.Command.Argv...)
	environment := make([]string, 0, len(config.Environment))
	for _, entry := range config.Environment {
		environment = append(environment, entry.Name+"="+entry.Value)
	}
	if err := syscall.Exec(argv[0], argv, environment); err != nil {
		return fmt.Errorf("exec build process: %w", err)
	}
	return nil
}

func ensureBuildConfigEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("build process config contains trailing input")
		}
		return fmt.Errorf("decode build process config trailing input: %w", err)
	}
	return nil
}

func validateBuildProcessConfig(config buildProcessConfig) error {
	for name, path := range map[string]string{
		"processRoot": config.ProcessRoot,
		"runtime":     config.Runtime,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("build process %s path is not absolute and normalized", name)
		}
	}
	for name, path := range map[string]string{
		"manager":   config.Manager,
		"project":   config.Project,
		"toolchain": config.Toolchain,
	} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
			return fmt.Errorf("build process %s path is not absolute and normalized", name)
		}
	}
	if config.Identity.UID != 65532 || config.Identity.GID != 65532 {
		return errors.New("build process identity is not the exact v0 identity")
	}
	if len(config.Command.Argv) == 0 ||
		!filepath.IsAbs(config.Command.Argv[0]) ||
		!filepath.IsAbs(config.Command.CWD) {
		return errors.New("build process command is incomplete")
	}
	for _, entry := range config.Environment {
		if entry.Name == "" || bytes.IndexByte([]byte(entry.Name), '=') >= 0 {
			return errors.New("build process environment is invalid")
		}
	}
	return nil
}

func setupBuildProcessRoot(config buildProcessConfig) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make build mount namespace private: %w", err)
	}
	if err := unix.Mount(
		config.ProcessRoot,
		config.ProcessRoot,
		"",
		unix.MS_BIND,
		"",
	); err != nil {
		return fmt.Errorf("bind build process root: %w", err)
	}
	if err := mountBuildWritable(config.ProcessRoot, "work"); err != nil {
		return err
	}
	if err := mountBuildWritable(config.ProcessRoot, "tmp"); err != nil {
		return err
	}
	if err := mountBuildDevices(config.ProcessRoot); err != nil {
		return err
	}
	for _, mount := range []struct {
		source string
		target string
	}{
		{source: buildResolverPath, target: "/etc/resolv.conf"},
		{
			source: filepath.Join(config.Runtime, "lib/nsswitch.conf"),
			target: "/etc/nsswitch.conf",
		},
	} {
		if err := mountBuildFile(config.ProcessRoot, mount.source, mount.target); err != nil {
			return err
		}
	}
	for _, mount := range []struct {
		noexec bool
		source string
		target string
	}{
		{source: config.Manager, target: "/opt/helmr/manager"},
		{source: config.Runtime, target: "/opt/helmr/runtime"},
		{source: config.Toolchain, target: "/nix"},
		{source: config.Project, target: "/opt/helmr/program"},
	} {
		if mount.source == "" {
			continue
		}
		if err := mountBuildComponent(
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
		return fmt.Errorf("remount build process root read-only: %w", err)
	}
	for _, alias := range config.Aliases {
		info, err := os.Stat(alias.Path)
		if err != nil {
			return fmt.Errorf("stat build process alias %q: %w", alias.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("build process alias %q is not a regular file", alias.Path)
		}
	}
	info, err := os.Stat(config.Command.Argv[0])
	if err != nil {
		return fmt.Errorf("stat build process executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("build process executable is not executable")
	}
	return nil
}

func mountBuildFile(root, source, absoluteTarget string) error {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat build process file %q: %w", source, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("build process file %q is not regular", source)
	}
	target := filepath.Join(root, strings.TrimPrefix(absoluteTarget, "/"))
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("stat build process target %q: %w", absoluteTarget, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("build process target %q is not regular", absoluteTarget)
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind build process file %q: %w", absoluteTarget, err)
	}
	if err := unix.Mount(
		"",
		target,
		"",
		unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|
			unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
		"",
	); err != nil {
		return fmt.Errorf("remount build process file %q read-only: %w", absoluteTarget, err)
	}
	return nil
}

func mountBuildWritable(root string, relative string) error {
	path := filepath.Join(root, relative)
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind build writable path %q: %w", relative, err)
	}
	if err := unix.Mount(
		"",
		path,
		"",
		unix.MS_REMOUNT|unix.MS_BIND|unix.MS_NOSUID|unix.MS_NODEV,
		"",
	); err != nil {
		return fmt.Errorf("remount build writable path %q: %w", relative, err)
	}
	return nil
}

func mountBuildDevices(root string) error {
	path := filepath.Join(root, "dev")
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind build process devices: %w", err)
	}
	if err := unix.Mount(
		"",
		path,
		"",
		unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|
			unix.MS_NOSUID|unix.MS_NOEXEC,
		"",
	); err != nil {
		return fmt.Errorf("remount build process devices: %w", err)
	}
	return nil
}

func mountBuildComponent(
	root string,
	source string,
	absoluteTarget string,
	noexec bool,
) error {
	target := filepath.Join(root, absoluteTarget[1:])
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create build component target %q: %w", absoluteTarget, err)
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind build component %q: %w", absoluteTarget, err)
	}
	flags := uintptr(
		unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY |
			unix.MS_NOSUID | unix.MS_NODEV,
	)
	if noexec {
		flags |= unix.MS_NOEXEC
	}
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("remount build component %q: %w", absoluteTarget, err)
	}
	return nil
}

func applyBuildIdentity(uid, gid uint32) error {
	if uid == 0 || gid == 0 {
		return errors.New("build process identity must be unprivileged")
	}
	if err := buildAllThreadsPrctl(unix.PR_SET_NO_NEW_PRIVS, 1); err != nil {
		return fmt.Errorf("set build process no_new_privs: %w", err)
	}
	secureBits := buildSecureNoRoot |
		buildSecureNoRootL |
		buildSecureNoFixup |
		buildSecureNoFixL
	if err := buildAllThreadsPrctl(
		unix.PR_SET_SECUREBITS,
		uintptr(secureBits),
	); err != nil {
		return fmt.Errorf("lock build process securebits: %w", err)
	}
	if err := buildAllThreadsPrctl(
		unix.PR_CAP_AMBIENT,
		unix.PR_CAP_AMBIENT_CLEAR_ALL,
	); err != nil {
		return fmt.Errorf("clear build process ambient capabilities: %w", err)
	}
	lastCapability, err := dropBuildBoundingCapabilities()
	if err != nil {
		return err
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("clear build process supplementary groups: %w", err)
	}
	if err := syscall.Setresgid(int(gid), int(gid), int(gid)); err != nil {
		return fmt.Errorf("set build process GID: %w", err)
	}
	if err := syscall.Setresuid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("set build process UID: %w", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := buildAllThreadsCapset(&header, &data[0]); err != nil {
		return fmt.Errorf("clear build process capabilities: %w", err)
	}
	return validateBuildIdentity(uid, gid, lastCapability)
}

func buildAllThreadsPrctl(
	option int,
	argument2 uintptr,
) error {
	_, _, errno := syscall.AllThreadsSyscall6(
		syscall.SYS_PRCTL,
		uintptr(option),
		argument2,
		0,
		0,
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func dropBuildBoundingCapabilities() (uintptr, error) {
	const searchLimit = 1024
	for capability := uintptr(0); capability < searchLimit; capability++ {
		err := buildAllThreadsPrctl(
			unix.PR_CAPBSET_DROP,
			capability,
		)
		if errors.Is(err, syscall.EINVAL) {
			if capability == 0 {
				return 0, errors.New("kernel exposes no capability bounding set")
			}
			return capability - 1, nil
		}
		if err != nil {
			return 0, fmt.Errorf(
				"drop build process capability %d: %w",
				capability,
				err,
			)
		}
	}
	return 0, fmt.Errorf("kernel capability domain reaches search limit %d", searchLimit)
}

func buildAllThreadsCapset(
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

func validateBuildIdentity(uid, gid uint32, lastCapability uintptr) error {
	ruid, euid, suid := unix.Getresuid()
	rgid, egid, sgid := unix.Getresgid()
	if ruid != int(uid) || euid != int(uid) || suid != int(uid) ||
		rgid != int(gid) || egid != int(gid) || sgid != int(gid) {
		return errors.New("build process identity did not become permanent")
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || noNewPrivileges != 1 {
		return errors.New("build process no_new_privs is not active")
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read build process capabilities: %w", err)
	}
	for _, word := range data {
		if word.Effective != 0 || word.Permitted != 0 || word.Inheritable != 0 {
			return errors.New("build process retained capabilities")
		}
	}
	for capability := uintptr(0); capability <= lastCapability; capability++ {
		present, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, capability, 0, 0, 0)
		if err != nil || present != 0 {
			return fmt.Errorf(
				"build process retained bounding capability %d",
				capability,
			)
		}
	}
	return nil
}

func closeBuildAmbientDescriptors(supervisor bool) error {
	firstClosed := uint(4)
	if supervisor {
		firstClosed = 5
	}
	if err := unix.CloseRange(firstClosed, ^uint(0), 0); err != nil {
		return fmt.Errorf("close build process ambient descriptors: %w", err)
	}
	lastOpen := buildReadyFD
	if supervisor {
		lastOpen = buildSupervisorFD
	}
	for fd := 0; fd <= lastOpen; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			return fmt.Errorf("build process descriptor %d is closed: %w", fd, err)
		}
	}
	return nil
}
