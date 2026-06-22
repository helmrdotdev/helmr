//go:build !linux

package guestd

import (
	"errors"
	"os"
	"syscall"
)

func signalWorkspaceProcess(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	pgid, err := syscall.Getpgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if err != nil {
		return err
	}
	selfPGID, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		return err
	}
	target := pid
	if pgid > 0 && pgid != selfPGID {
		target = -pgid
	}
	err = syscall.Kill(target, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
