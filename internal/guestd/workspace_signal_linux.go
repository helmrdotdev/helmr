package guestd

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func signalWorkspaceProcess(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return os.ErrProcessDone
	}
	if err != nil {
		return err
	}
	defer unix.Close(pidfd)

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
	if pgid > 0 && pgid != selfPGID {
		if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		} else if err != nil {
			return err
		}
		err = syscall.Kill(-pgid, signal)
	} else {
		err = unix.PidfdSendSignal(pidfd, unix.Signal(signal), nil, 0)
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, unix.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
