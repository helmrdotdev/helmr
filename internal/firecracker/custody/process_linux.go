//go:build linux

package custody

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

func Stop(ctx context.Context, state StateRoot, jailerDir string, owner vm.Owner, pid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if exited, err := recoveryProcessExited(fd); err != nil || exited {
		return err
	}
	// Reacquire current owner/root evidence after the process handle. The
	// inventory's numeric PID never carries authority into this operation.
	roots, err := openRecoveryRoots(state, jailerDir)
	if err != nil {
		return err
	}
	defer closeRecoveryRoots(roots)
	known := false
	for _, root := range roots {
		known = known || root.owner == owner
	}
	if !known {
		return errors.New("exact VM owner is unavailable before process stop")
	}
	process, owned, inspectErr := inspectRecoveryProcess(filepath.Join("/proc", strconv.Itoa(pid)), pid, jailerDir, roots)
	if exited, err := recoveryProcessExited(fd); err != nil || exited {
		return err
	}
	if inspectErr != nil {
		return inspectErr
	}
	if !owned || process.ID != owner.ID || process.Problem != "" {
		return errors.New("process does not match the exact VM owner before signal")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	if err := waitRecoveryProcess(ctx, fd, 2*time.Second); err == nil {
		return nil
	} else if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return err
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	if err := waitRecoveryProcess(ctx, fd, 2*time.Second); err != nil {
		return fmt.Errorf("VM process exit remains unproved after SIGKILL: %w", err)
	}
	return nil
}

func recoveryProcessExited(fd int) (bool, error) {
	polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(polls, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		break
	}
	if polls[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
		return false, errors.New("VM process handle observation failed")
	}
	return polls[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
}

func waitRecoveryProcess(ctx context.Context, fd int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if exited, err := recoveryProcessExited(fd); err != nil || exited {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-tick.C:
		}
	}
}
