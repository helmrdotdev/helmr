//go:build linux

package guestd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const programCgroupLeaf = "run"

type linuxProgramCgroup struct {
	path string
	file *os.File
}

func enterProgramCgroupNamespace() error {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("read Program cgroup identity: %w", err)
	}
	expected := "0::" + strings.TrimPrefix(
		filepath.Join(dependencyCgroupRoot, programCgroupLeaf),
		"/sys/fs/cgroup",
	)
	if strings.TrimSpace(string(raw)) != expected {
		return fmt.Errorf(
			"Program cgroup identity %q does not match assigned subtree",
			strings.TrimSpace(string(raw)),
		)
	}
	if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
		return fmt.Errorf("create Program cgroup namespace: %w", err)
	}
	return nil
}

func createProgramCgroup() (programCgroup, error) {
	rootFD, err := unix.Open(
		dependencyCgroupRoot,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open Program cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	processes, err := os.ReadFile(filepath.Join(dependencyCgroupRoot, "cgroup.procs"))
	if err != nil {
		return nil, fmt.Errorf("read Program cgroup root processes: %w", err)
	}
	if len(bytes.TrimSpace(processes)) != 0 {
		return nil, errors.New("Program cgroup root is not process-free")
	}
	path := filepath.Join(dependencyCgroupRoot, programCgroupLeaf)
	if err := unix.Mkdirat(rootFD, programCgroupLeaf, 0o755); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create Program cgroup: %w", err)
		}
		if cleanupErr := cleanupStaleProgramCgroup(path); cleanupErr != nil {
			return nil, cleanupErr
		}
		if err := unix.Mkdirat(rootFD, programCgroupLeaf, 0o755); err != nil {
			return nil, fmt.Errorf("recreate Program cgroup: %w", err)
		}
	}
	cgroupFD, err := unix.Openat(
		rootFD,
		programCgroupLeaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(rootFD, programCgroupLeaf, unix.AT_REMOVEDIR)
		return nil, fmt.Errorf("open Program cgroup: %w", err)
	}
	return &linuxProgramCgroup{
		path: path,
		file: os.NewFile(uintptr(cgroupFD), path),
	}, nil
}

func cleanupStaleProgramCgroup(path string) error {
	if err := killCgroup(path); err != nil {
		return fmt.Errorf("kill stale Program cgroup: %w", err)
	}
	if err := waitCgroupEmpty(path); err != nil {
		return fmt.Errorf("empty stale Program cgroup: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Program cgroup: %w", err)
	}
	return nil
}

func (c *linuxProgramCgroup) attach(command *exec.Cmd) error {
	if command == nil || command.SysProcAttr == nil {
		return errors.New("Program command process attributes are required")
	}
	if c == nil || c.file == nil {
		return errors.New("Program cgroup is required")
	}
	command.SysProcAttr.UseCgroupFD = true
	command.SysProcAttr.CgroupFD = int(c.file.Fd())
	return nil
}

func (c *linuxProgramCgroup) kill() error {
	if c == nil || c.path == "" {
		return errors.New("Program cgroup is required")
	}
	return killCgroup(c.path)
}

func (c *linuxProgramCgroup) waitEmpty() error {
	if c == nil || c.path == "" {
		return errors.New("Program cgroup is required")
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
