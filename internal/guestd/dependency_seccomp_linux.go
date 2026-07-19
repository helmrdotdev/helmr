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
	dependencyBPFLdWAbs  = 0x20
	dependencyBPFJmpJEQ  = 0x15
	dependencyBPFJmpJSet = 0x45
	dependencyBPFAluAnd  = 0x54
	dependencyBPFRetK    = 0x06

	dependencySeccompNR     = 0
	dependencySeccompArch   = 4
	dependencySeccompArg0   = 16
	dependencySeccompArg1   = 24
	dependencySocketTypeMax = 0xf
	dependencyX32SyscallBit = 0x40000000
)

func installDependencySeccomp() error {
	filter, err := dependencySeccompFilter()
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
		return fmt.Errorf("install dependency process seccomp: %w", errno)
	}
	return nil
}

func dependencySeccompFilter() ([]unix.SockFilter, error) {
	architecture, err := dependencyAuditArchitecture()
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
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompArch),
		dependencyBPFJump(dependencyBPFJmpJEQ, architecture, 1, 0),
		dependencyBPFStatement(dependencyBPFRetK, kill),
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompNR),
	}
	if runtime.GOARCH == "amd64" {
		filter = append(
			filter,
			dependencyBPFJump(
				dependencyBPFJmpJSet,
				dependencyX32SyscallBit,
				0,
				1,
			),
			dependencyBPFStatement(dependencyBPFRetK, kill),
		)
	}
	filter = append(filter,
		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.SYS_SOCKET), 0, 9),
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompArg0),
		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.AF_VSOCK), 5, 0),
		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.AF_NETLINK), 4, 0),
		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.AF_PACKET), 3, 0),
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompArg1),
		dependencyBPFStatement(dependencyBPFAluAnd, dependencySocketTypeMax),
		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.SOCK_RAW), 0, 1),
		dependencyBPFStatement(dependencyBPFRetK, deny),
		dependencyBPFStatement(dependencyBPFRetK, allow),

		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.SYS_CLONE), 0, 5),
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompArg0),
		dependencyBPFStatement(dependencyBPFAluAnd, namespaceMask),
		dependencyBPFJump(dependencyBPFJmpJEQ, 0, 1, 0),
		dependencyBPFStatement(dependencyBPFRetK, deny),
		dependencyBPFStatement(dependencyBPFRetK, allow),

		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.SYS_UNSHARE), 0, 5),
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompArg0),
		dependencyBPFStatement(dependencyBPFAluAnd, namespaceMask),
		dependencyBPFJump(dependencyBPFJmpJEQ, 0, 1, 0),
		dependencyBPFStatement(dependencyBPFRetK, deny),
		dependencyBPFStatement(dependencyBPFRetK, allow),

		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.SYS_SETNS), 0, 6),
		dependencyBPFStatement(dependencyBPFLdWAbs, dependencySeccompArg1),
		dependencyBPFJump(dependencyBPFJmpJEQ, 0, 2, 0),
		dependencyBPFStatement(dependencyBPFAluAnd, namespaceMask),
		dependencyBPFJump(dependencyBPFJmpJEQ, 0, 1, 0),
		dependencyBPFStatement(dependencyBPFRetK, deny),
		dependencyBPFStatement(dependencyBPFRetK, allow),
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
			dependencyBPFJump(dependencyBPFJmpJEQ, uint32(number), 0, 1),
			dependencyBPFStatement(dependencyBPFRetK, deny),
		)
	}
	filter = append(
		filter,
		dependencyBPFJump(dependencyBPFJmpJEQ, uint32(unix.SYS_CLONE3), 0, 1),
		dependencyBPFStatement(dependencyBPFRetK, noSys),
		dependencyBPFStatement(dependencyBPFRetK, allow),
	)
	if len(filter) > int(^uint16(0)) {
		return nil, errors.New("dependency seccomp filter is too large")
	}
	return filter, nil
}

func dependencyAuditArchitecture() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return uint32(unix.AUDIT_ARCH_X86_64), nil
	case "arm64":
		return uint32(unix.AUDIT_ARCH_AARCH64), nil
	default:
		return 0, fmt.Errorf(
			"dependency process architecture %q has no seccomp contract",
			runtime.GOARCH,
		)
	}
}

func dependencyBPFStatement(code uint16, value uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value}
}

func dependencyBPFJump(
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
