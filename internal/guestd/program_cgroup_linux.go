//go:build linux

package guestd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const programCgroupTransitionPoll = 10 * time.Millisecond

type linuxProgramCgroup struct {
	path string
	file *os.File
}

func enterProgramCgroupNamespace(leaf string) error {
	if err := validateProgramCgroupLeaf(leaf); err != nil {
		return err
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("read program cgroup identity: %w", err)
	}
	expected := "0::" + strings.TrimPrefix(
		filepath.Join(buildCgroupRoot, leaf),
		"/sys/fs/cgroup",
	)
	if strings.TrimSpace(string(raw)) != expected {
		return fmt.Errorf(
			"program cgroup identity %q does not match assigned subtree",
			strings.TrimSpace(string(raw)),
		)
	}
	if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
		return fmt.Errorf("create program cgroup namespace: %w", err)
	}
	return nil
}

func createProgramCgroup(leaf string) (programCgroup, error) {
	if err := validateProgramCgroupLeaf(leaf); err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(
		buildCgroupRoot,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open program cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	processes, err := os.ReadFile(filepath.Join(buildCgroupRoot, "cgroup.procs"))
	if err != nil {
		return nil, fmt.Errorf("read program cgroup root processes: %w", err)
	}
	if len(bytes.TrimSpace(processes)) != 0 {
		return nil, errors.New("program cgroup root is not process-free")
	}
	path := filepath.Join(buildCgroupRoot, leaf)
	if err := unix.Mkdirat(rootFD, leaf, 0o755); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create program cgroup: %w", err)
		}
		if cleanupErr := cleanupStaleProgramCgroup(path); cleanupErr != nil {
			return nil, cleanupErr
		}
		if err := unix.Mkdirat(rootFD, leaf, 0o755); err != nil {
			return nil, fmt.Errorf("recreate program cgroup: %w", err)
		}
	}
	cgroupFD, err := unix.Openat(
		rootFD,
		leaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(rootFD, leaf, unix.AT_REMOVEDIR)
		return nil, fmt.Errorf("open program cgroup: %w", err)
	}
	return &linuxProgramCgroup{
		path: path,
		file: os.NewFile(uintptr(cgroupFD), path),
	}, nil
}

func cleanupStaleProgramCgroup(path string) error {
	if err := killCgroup(path); err != nil {
		return fmt.Errorf("kill stale program cgroup: %w", err)
	}
	if err := waitCgroupEmpty(path); err != nil {
		return fmt.Errorf("empty stale program cgroup: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale program cgroup: %w", err)
	}
	return nil
}

func (c *linuxProgramCgroup) attach(command *exec.Cmd) error {
	if command == nil || command.SysProcAttr == nil {
		return errors.New("program command process attributes are required")
	}
	if c == nil || c.file == nil {
		return errors.New("program cgroup is required")
	}
	command.SysProcAttr.UseCgroupFD = true
	command.SysProcAttr.CgroupFD = int(c.file.Fd())
	return nil
}

func (c *linuxProgramCgroup) freeze(ctx context.Context) error {
	return c.setFrozen(ctx, true)
}

func (c *linuxProgramCgroup) thaw(ctx context.Context) error {
	return c.setFrozen(ctx, false)
}

func (c *linuxProgramCgroup) setFrozen(ctx context.Context, frozen bool) error {
	if c == nil || c.file == nil {
		return errors.New("program cgroup is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	value := []byte("0")
	state := "thawed"
	if frozen {
		value = []byte("1")
		state = "frozen"
	}
	fd, err := unix.Openat(
		int(c.file.Fd()), "cgroup.freeze",
		unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return fmt.Errorf("open program cgroup freeze control: %w", err)
	}
	written, writeErr := unix.Write(fd, value)
	closeErr := unix.Close(fd)
	if writeErr != nil || written != len(value) || closeErr != nil {
		if writeErr == nil && written != len(value) {
			writeErr = io.ErrShortWrite
		}
		return fmt.Errorf(
			"request program cgroup %s: %w",
			state, errors.Join(writeErr, closeErr),
		)
	}
	for {
		observed, populated, err := c.state()
		if err != nil {
			return err
		}
		complete, err := programCgroupTransitionComplete(observed, populated, frozen)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		timer := time.NewTimer(programCgroupTransitionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("verify program cgroup %s: %w", state, ctx.Err())
		case <-timer.C:
		}
	}
}

func programCgroupTransitionComplete(observed, populated, wantFrozen bool) (bool, error) {
	if wantFrozen && observed && !populated {
		return false, errors.New("verify program cgroup frozen: cgroup is empty")
	}
	return observed == wantFrozen, nil
}

func (c *linuxProgramCgroup) state() (frozen bool, populated bool, err error) {
	if c == nil || c.file == nil {
		return false, false, errors.New("program cgroup is required")
	}
	fd, err := unix.Openat(
		int(c.file.Fd()), "cgroup.events",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return false, false, fmt.Errorf("open program cgroup events: %w", err)
	}
	file := os.NewFile(uintptr(fd), "cgroup.events")
	if file == nil {
		_ = unix.Close(fd)
		return false, false, errors.New("open program cgroup events file")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return false, false, fmt.Errorf("read program cgroup events: %w", errors.Join(readErr, closeErr))
	}
	if len(body) > 64*1024 {
		return false, false, errors.New("program cgroup events exceeded its bound")
	}
	return parseProgramCgroupState(body)
}

func parseProgramCgroupState(body []byte) (frozen bool, populated bool, err error) {
	foundFrozen := false
	foundPopulated := false
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "frozen" && fields[0] != "populated") {
			continue
		}
		if len(fields) != 2 || (fields[1] != "0" && fields[1] != "1") {
			return false, false, errors.New("program cgroup state event is invalid")
		}
		switch fields[0] {
		case "frozen":
			if foundFrozen {
				return false, false, errors.New("program cgroup frozen event is invalid")
			}
			foundFrozen = true
			frozen = fields[1] == "1"
		case "populated":
			if foundPopulated {
				return false, false, errors.New("program cgroup populated event is invalid")
			}
			foundPopulated = true
			populated = fields[1] == "1"
		}
	}
	if !foundFrozen || !foundPopulated {
		return false, false, errors.New("program cgroup state event is missing")
	}
	return frozen, populated, nil
}

func (c *linuxProgramCgroup) kill() error {
	if c == nil || c.path == "" {
		return errors.New("program cgroup is required")
	}
	return killCgroup(c.path)
}

func (c *linuxProgramCgroup) waitEmpty() error {
	if c == nil || c.path == "" {
		return errors.New("program cgroup is required")
	}
	return waitCgroupEmpty(c.path)
}

func (c *linuxProgramCgroup) close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	if c.file != nil {
		closeErr = c.file.Close()
		c.file = nil
	}
	if c.path != "" {
		closeErr = errors.Join(closeErr, os.Remove(c.path))
		c.path = ""
	}
	return closeErr
}
