//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	buildBPFLdWAbs  = 0x20
	buildBPFJmpJEQ  = 0x15
	buildBPFJmpJSet = 0x45
	buildBPFAluAnd  = 0x54
	buildBPFRetK    = 0x06

	buildSeccompNR     = 0
	buildSeccompArch   = 4
	buildSeccompArg0   = 16
	buildSeccompArg1   = 24
	buildSocketTypeMax = 0xf
	buildX32SyscallBit = 0x40000000
)

func installBuildSeccomp() error {
	filter, err := buildSeccompFilter()
	if err != nil {
		return err
	}
	program := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&program)),
		0,
		0,
		0,
	)
	runtime.KeepAlive(filter)
	runtime.KeepAlive(program)
	if errno != 0 {
		return fmt.Errorf("install build process seccomp: %w", errno)
	}
	return nil
}

func buildSeccompFilter() ([]unix.SockFilter, error) {
	architecture, err := buildAuditArchitecture()
	if err != nil {
		return nil, err
	}
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(syscall.EPERM))
	noSys := uint32(unix.SECCOMP_RET_ERRNO | uint32(syscall.ENOSYS))
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	kill := uint32(unix.SECCOMP_RET_KILL_PROCESS)
	namespaceMask := uint32(
		unix.CLONE_NEWUSER |
			unix.CLONE_NEWNS |
			unix.CLONE_NEWPID |
			unix.CLONE_NEWIPC |
			unix.CLONE_NEWNET,
	)

	filter := []unix.SockFilter{
		buildBPFStatement(buildBPFLdWAbs, buildSeccompArch),
		buildBPFJump(buildBPFJmpJEQ, architecture, 1, 0),
		buildBPFStatement(buildBPFRetK, kill),
		buildBPFStatement(buildBPFLdWAbs, buildSeccompNR),
	}
	if runtime.GOARCH == "amd64" {
		filter = append(
			filter,
			buildBPFJump(
				buildBPFJmpJSet,
				buildX32SyscallBit,
				0,
				1,
			),
			buildBPFStatement(buildBPFRetK, kill),
		)
	}
	filter = append(filter,
		buildBPFJump(buildBPFJmpJEQ, uint32(unix.SYS_SOCKET), 0, 9),
		buildBPFStatement(buildBPFLdWAbs, buildSeccompArg0),
		buildBPFJump(buildBPFJmpJEQ, uint32(unix.AF_VSOCK), 5, 0),
		buildBPFJump(buildBPFJmpJEQ, uint32(unix.AF_NETLINK), 4, 0),
		buildBPFJump(buildBPFJmpJEQ, uint32(unix.AF_PACKET), 3, 0),
		buildBPFStatement(buildBPFLdWAbs, buildSeccompArg1),
		buildBPFStatement(buildBPFAluAnd, buildSocketTypeMax),
		buildBPFJump(buildBPFJmpJEQ, uint32(unix.SOCK_RAW), 0, 1),
		buildBPFStatement(buildBPFRetK, deny),
		buildBPFStatement(buildBPFRetK, allow),

		buildBPFJump(buildBPFJmpJEQ, uint32(unix.SYS_CLONE), 0, 5),
		buildBPFStatement(buildBPFLdWAbs, buildSeccompArg0),
		buildBPFStatement(buildBPFAluAnd, namespaceMask),
		buildBPFJump(buildBPFJmpJEQ, 0, 1, 0),
		buildBPFStatement(buildBPFRetK, deny),
		buildBPFStatement(buildBPFRetK, allow),

		buildBPFJump(buildBPFJmpJEQ, uint32(unix.SYS_UNSHARE), 0, 5),
		buildBPFStatement(buildBPFLdWAbs, buildSeccompArg0),
		buildBPFStatement(buildBPFAluAnd, namespaceMask),
		buildBPFJump(buildBPFJmpJEQ, 0, 1, 0),
		buildBPFStatement(buildBPFRetK, deny),
		buildBPFStatement(buildBPFRetK, allow),

		buildBPFJump(buildBPFJmpJEQ, uint32(unix.SYS_SETNS), 0, 6),
		buildBPFStatement(buildBPFLdWAbs, buildSeccompArg1),
		buildBPFJump(buildBPFJmpJEQ, 0, 2, 0),
		buildBPFStatement(buildBPFAluAnd, namespaceMask),
		buildBPFJump(buildBPFJmpJEQ, 0, 1, 0),
		buildBPFStatement(buildBPFRetK, deny),
		buildBPFStatement(buildBPFRetK, allow),
	)
	for _, number := range []uintptr{
		unix.SYS_MOUNT,
		unix.SYS_UMOUNT2,
		unix.SYS_PIVOT_ROOT,
		unix.SYS_CHROOT,
		unix.SYS_OPEN_TREE,
		unix.SYS_MOVE_MOUNT,
		unix.SYS_FSOPEN,
		unix.SYS_FSCONFIG,
		unix.SYS_FSMOUNT,
		unix.SYS_FSPICK,
		unix.SYS_MOUNT_SETATTR,
		unix.SYS_STATMOUNT,
		unix.SYS_LISTMOUNT,
		unix.SYS_OPEN_TREE_ATTR,
		unix.SYS_IO_URING_SETUP,
	} {
		filter = append(
			filter,
			buildBPFJump(buildBPFJmpJEQ, uint32(number), 0, 1),
			buildBPFStatement(buildBPFRetK, deny),
		)
	}
	filter = append(
		filter,
		buildBPFJump(buildBPFJmpJEQ, uint32(unix.SYS_CLONE3), 0, 1),
		buildBPFStatement(buildBPFRetK, noSys),
		buildBPFStatement(buildBPFRetK, allow),
	)
	if len(filter) > int(^uint16(0)) {
		return nil, errors.New("build seccomp filter is too large")
	}
	return filter, nil
}

func buildAuditArchitecture() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return uint32(unix.AUDIT_ARCH_X86_64), nil
	case "arm64":
		return uint32(unix.AUDIT_ARCH_AARCH64), nil
	default:
		return 0, fmt.Errorf(
			"build process architecture %q has no seccomp contract",
			runtime.GOARCH,
		)
	}
}

func buildBPFStatement(code uint16, value uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value}
}

func buildBPFJump(
	code uint16,
	value uint32,
	jumpTrue uint8,
	jumpFalse uint8,
) unix.SockFilter {
	return unix.SockFilter{
		Code: code,
		Jt:   jumpTrue,
		Jf:   jumpFalse,
		K:    value,
	}
}
