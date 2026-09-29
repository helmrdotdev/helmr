//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func (runtime *QualifiedRuntime) Cleanup(ctx context.Context, owner vm.Owner) error {
	return runtime.connector.cleanup(ctx, owner)
}

type connectorCleaner struct{ connector *Connector }

func (cleaner connectorCleaner) Cleanup(ctx context.Context, owner vm.Owner) error {
	return cleaner.connector.cleanup(ctx, owner)
}

func (c *Connector) cleanup(ctx context.Context, owner vm.Owner) error {
	if err := owner.Validate(); err != nil {
		return cleanupUnproven(owner, err)
	}
	retained := c.lockComputerOwner(owner)
	if retained != nil {
		defer retained.mu.Unlock()
	}
	return c.cleanupOwned(ctx, owner, retained)
}

func (c *Connector) cleanupOwned(ctx context.Context, owner vm.Owner, retained *computerDeviceOwner) (retErr error) {
	defer func() {
		if retErr == nil && retained != nil {
			c.computerDevices.CompareAndDelete(owner, retained)
		}
	}()
	statePath := filepath.Join(c.cfg.StateDir, owner.ID)
	jailerPath := filepath.Join(c.cfg.JailerChrootBaseDir, "firecracker", owner.ID)
	pids, err := exactRuntimePIDs(owner.ID)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inventory Firecracker processes: %w", err))
	}
	netns, err := c.runtimeNetNSExists(ctx, owner.ID)
	if err != nil {
		return cleanupUnproven(owner, err)
	}
	stateExists, err := pathExists(statePath)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inspect Firecracker state: %w", err))
	}
	jailerExists, err := pathExists(jailerPath)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inspect Firecracker jailer state: %w", err))
	}
	if !stateExists && !jailerExists && !netns && len(pids) == 0 {
		return c.releaseComputerDevice(ctx, owner, retained)
	}
	if !stateExists {
		return cleanupUnproven(owner, errors.New("the Firecracker ownership marker is missing"))
	}
	if err := validateOwnerMarker(statePath, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	for _, pid := range pids {
		if err := stopExactRuntimePID(ctx, pid); err != nil {
			return cleanupUnproven(owner, fmt.Errorf("stop Firecracker process %d: %w", pid, err))
		}
	}
	if err := c.cleanupNetworkAttachment(ctx, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	if err := os.RemoveAll(jailerPath); err != nil {
		return cleanupUnproven(owner, fmt.Errorf("remove Firecracker jailer state: %w", err))
	}
	remaining, err := exactRuntimePIDs(owner.ID)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker processes absent: %w", err))
	}
	if len(remaining) != 0 {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker processes absent: pids=%v", remaining))
	}
	if exists, verifyErr := c.runtimeNetNSExists(ctx, owner.ID); verifyErr != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker netns absent: %w", verifyErr))
	} else if exists {
		return cleanupUnproven(owner, errors.New("verify Firecracker netns absent: namespace remains"))
	}
	if exists, verifyErr := pathExists(jailerPath); verifyErr != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker jailer state absent: %w", verifyErr))
	} else if exists {
		return cleanupUnproven(owner, errors.New("verify Firecracker jailer state absent: path remains"))
	}
	if err := c.releaseComputerDevice(ctx, owner, retained); err != nil {
		return err
	}
	if err := removeStateRootLast(statePath, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	if exists, verifyErr := pathExists(statePath); verifyErr != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker state absent: %w", verifyErr))
	} else if exists {
		return cleanupUnproven(owner, errors.New("verify Firecracker state absent: path remains"))
	}
	return nil
}

func cleanupUnproven(owner vm.Owner, cause error) error {
	return &vm.CleanupUnprovenError{Owner: owner, Cause: cause}
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func validateOwnerMarker(statePath string, owner vm.Owner) error {
	info, err := os.Stat(statePath)
	if err != nil {
		return fmt.Errorf("inspect Firecracker ownership root: %w", err)
	}
	if !info.IsDir() {
		return errors.New("the Firecracker ownership root is not a directory")
	}
	marker, err := os.ReadFile(filepath.Join(statePath, "owner"))
	if err != nil {
		return fmt.Errorf("read Firecracker ownership marker: %w", err)
	}
	if string(marker) != string(owner.Kind)+"\n"+owner.ID+"\n" {
		return errors.New("the Firecracker ownership marker does not match exact owner")
	}
	return nil
}

func removeStateRootLast(statePath string, owner vm.Owner) error {
	entries, err := os.ReadDir(statePath)
	if err != nil {
		return fmt.Errorf("inventory Firecracker state: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "owner" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(statePath, entry.Name())); err != nil {
			return fmt.Errorf("remove Firecracker state entry %q: %w", entry.Name(), err)
		}
	}
	markerPath := filepath.Join(statePath, "owner")
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("remove Firecracker ownership marker: %w", err)
	}
	if err := os.Remove(statePath); err != nil {
		restoreErr := os.WriteFile(markerPath, []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600)
		return errors.Join(fmt.Errorf("remove Firecracker state root: %w", err), restoreErr)
	}
	return nil
}

func (c *Connector) runtimeNetNSExists(ctx context.Context, runtimeID string) (bool, error) {
	output, err := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "list").Output()
	if err != nil {
		return false, fmt.Errorf("inventory Firecracker cleanup netns: %w", err)
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && fields[0] == runtimeID {
			return true, nil
		}
	}
	return false, nil
}

func exactRuntimePIDs(runtimeID string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if readErr != nil {
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
		if len(args) == 0 || (filepath.Base(args[0]) != "firecracker" && filepath.Base(args[0]) != "jailer") {
			continue
		}
		for _, arg := range args[1:] {
			if arg == runtimeID {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

func stopExactRuntimePID(ctx context.Context, pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		if err := process.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return process.Kill()
		case <-ticker.C:
		}
	}
}
