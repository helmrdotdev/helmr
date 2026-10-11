//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/firecracker/custody"
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

func (c *Connector) cleanupOwned(ctx context.Context, owner vm.Owner, retained *computerDeviceOwner) error {
	state := custody.StateRoot{Base: c.cfg.StateDir}
	return c.cleanupOwnedProcesses(ctx, owner, retained,
		func() ([]int, error) { return custody.MatchingPIDs(state, c.cfg.JailerChrootBaseDir, owner) },
		func(ctx context.Context, pid int) error {
			return custody.Stop(ctx, state, c.cfg.JailerChrootBaseDir, owner, pid)
		},
	)
}

// The process boundary is substitutable to verify failed inventories and unproved
// stops preserve custody. Production always uses the shared exact-owner mechanics.
func (c *Connector) cleanupOwnedProcesses(ctx context.Context, owner vm.Owner, retained *computerDeviceOwner, inventory func() ([]int, error), stop func(context.Context, int) error) (retErr error) {
	defer func() {
		if retErr == nil && retained != nil {
			c.computerDevices.CompareAndDelete(owner, retained)
		}
	}()
	if err := ctx.Err(); err != nil {
		return cleanupUnproven(owner, err)
	}
	statePath := filepath.Join(c.cfg.StateDir, owner.ID)
	jailerPath := filepath.Join(c.cfg.JailerChrootBaseDir, "firecracker", owner.ID)
	pids, err := inventory()
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
	cgroupPath, err := runtimeCgroupPath(c.cfg.CgroupVersion, owner.ID)
	if err != nil {
		return cleanupUnproven(owner, err)
	}
	cgroupExists, err := pathExists(cgroupPath)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inspect Firecracker cgroup: %w", err))
	}
	if !stateExists && !jailerExists && !netns && len(pids) == 0 && !cgroupExists {
		return c.releaseComputerDevice(ctx, owner, retained)
	}
	if !stateExists {
		return cleanupUnproven(owner, errors.New("the Firecracker ownership marker is missing"))
	}
	if err := validateOwnerMarker(statePath, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	for _, pid := range pids {
		if err := stop(ctx, pid); err != nil {
			return cleanupUnproven(owner, fmt.Errorf("stop Firecracker process %d: %w", pid, err))
		}
	}
	if err := c.cleanupNetworkAttachment(ctx, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	remaining, err := inventory()
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
	if err := validateOwnerMarker(statePath, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	// Kernel rmdir rejects populated groups and child groups. Never recursively
	// remove cgroups or delete the shared jailer parent. Keep the owner marker and
	// retained device until this exact owner is physically absent.
	if err := removeRuntimeCgroup(cgroupPath); err != nil {
		return cleanupUnproven(owner, err)
	}
	if err := os.RemoveAll(jailerPath); err != nil {
		return cleanupUnproven(owner, fmt.Errorf("remove Firecracker jailer state: %w", err))
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
	recorded, err := (custody.StateRoot{Base: filepath.Dir(statePath)}).ReadOwner(filepath.Base(statePath))
	if err != nil {
		return fmt.Errorf("read Firecracker ownership marker: %w", err)
	}
	if recorded != owner {
		return errors.New("the Firecracker ownership marker does not match exact owner")
	}
	return nil
}

func removeStateRootLast(statePath string, owner vm.Owner) error {
	if err := validateOwnerMarker(statePath, owner); err != nil {
		return err
	}
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
