//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func (c *Connector) Preflight(ctx context.Context) error {
	var problems []error
	problems = append(problems,
		checkCommand("firecracker", c.cfg.FirecrackerPath),
		checkCommand("firecracker jailer", c.cfg.JailerPath),
		checkCommand("ip", c.cfg.IPPath),
		checkCommand("nft", c.cfg.NFTPath),
		checkCommand("mkfs.ext4", c.cfg.MkfsExt4Path),
		checkReadableFile("guest kernel", c.cfg.KernelPath),
		checkReadableFile("guest initramfs", c.cfg.InitramfsPath),
		checkReadableFile("guest rootfs", c.cfg.RootfsPath),
		checkReadWriteFile("KVM device", c.cfg.KVMPath),
		checkReadWriteFile("TUN device", "/dev/net/tun"),
		checkCgroup(c.cfg.CgroupVersion),
	)
	_, _, poolErr := validateNetworkPools(c.cfg)
	problems = append(problems, poolErr)
	problems = append(problems, ensureDirectory("firecracker state directory", c.cfg.StateDir))
	problems = append(problems, ensureDirectory("firecracker jailer chroot directory", c.cfg.JailerChrootBaseDir))
	problems = append(problems, checkHardLinkLayout(c.cfg))
	problems = append(problems, c.datapath.VerifyKernel())
	if err := ctx.Err(); err != nil {
		problems = append(problems, err)
	}
	if err := errors.Join(problems...); err != nil {
		return err
	}
	return c.proveRoutedNetworkLifecycle(ctx)
}

func (c *Connector) proveRoutedNetworkLifecycle(ctx context.Context) error {
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: uuid.Must(uuid.NewV7()).String()}
	statePath := filepath.Join(c.cfg.StateDir, owner.ID)
	if err := os.Mkdir(statePath, 0o700); err != nil {
		return fmt.Errorf("create routed network proof owner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600); err != nil {
		_ = os.Remove(statePath)
		return fmt.Errorf("write routed network proof owner: %w", err)
	}
	binding, err := c.prepareNetworkBinding(ctx, owner, vm.WorkloadBinding{
		WorkerEpoch: 1, OwnerID: owner.ID, Generation: 1,
		RuntimeInstanceID: owner.ID, RuntimeIdentityID: NetworkABIV0,
	})
	if err != nil {
		cleanupErr := c.Cleanup(context.Background(), owner)
		return fmt.Errorf("exercise routed network lifecycle: %w", errors.Join(err, cleanupErr))
	}
	deactivateErr := binding.Deactivate()
	closeErr := binding.Close()
	if closeErr != nil {
		return fmt.Errorf("close routed network proof: %w", errors.Join(deactivateErr, closeErr))
	}
	if err := c.cleanupNetworkAttachment(ctx, owner); err != nil {
		return fmt.Errorf("reclaim routed network proof: %w", errors.Join(deactivateErr, err))
	}
	if err := removeStateRootLast(statePath, owner); err != nil {
		return fmt.Errorf("remove routed network proof state: %w", err)
	}
	if err := syncDirectory(c.cfg.StateDir); err != nil {
		return err
	}
	if exists, err := pathExists(statePath); err != nil || exists {
		return fmt.Errorf("prove routed network proof state absence: exists=%t: %v", exists, err)
	}
	return deactivateErr
}

func checkHardLinkLayout(cfg Config) error {
	paths := []struct {
		name string
		path string
	}{
		{name: "guest kernel", path: cfg.KernelPath},
		{name: "guest initramfs", path: cfg.InitramfsPath},
		{name: "guest rootfs", path: cfg.RootfsPath},
		{name: "firecracker state directory", path: cfg.StateDir},
		{name: "firecracker jailer chroot directory", path: cfg.JailerChrootBaseDir},
	}
	var device uint64
	for index, item := range paths {
		info, err := os.Stat(item.path)
		if err != nil {
			return fmt.Errorf("inspect %s filesystem: %w", item.name, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("inspect %s filesystem: unsupported stat result", item.name)
		}
		if index == 0 {
			device = uint64(stat.Dev)
			continue
		}
		if uint64(stat.Dev) != device {
			return fmt.Errorf("%s is on a different filesystem than the guest kernel", item.name)
		}
	}

	for _, item := range paths[:3] {
		if err := proveHardLink(item.name, item.path, cfg.JailerChrootBaseDir); err != nil {
			return err
		}
	}
	probe, err := os.CreateTemp(cfg.StateDir, ".hardlink-")
	if err != nil {
		return fmt.Errorf("create firecracker hard-link probe: %w", err)
	}
	source := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(source)
		return fmt.Errorf("close firecracker hard-link probe: %w", err)
	}
	defer os.Remove(source)
	return proveHardLink("firecracker state", source, cfg.JailerChrootBaseDir)
}

func proveHardLink(label, source, directory string) error {
	target := filepath.Join(directory, ".hardlink-"+uuid.Must(uuid.NewV7()).String())
	defer os.Remove(target)
	if err := os.Link(source, target); err != nil {
		return fmt.Errorf("prove %s hard-link layout: %w", label, err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat %s hard-link source: %w", label, err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat %s hard-link target: %w", label, err)
	}
	sourceStat, sourceOK := sourceInfo.Sys().(*syscall.Stat_t)
	targetStat, targetOK := targetInfo.Sys().(*syscall.Stat_t)
	if !sourceOK || !targetOK || sourceStat.Dev != targetStat.Dev || sourceStat.Ino != targetStat.Ino {
		return fmt.Errorf("%s hard-link probe did not preserve inode identity", label)
	}
	return nil
}

func checkCommand(label string, path string) error {
	if filepath.Base(path) == path {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return fmt.Errorf("%s command %q was not found in PATH: %w", label, path, err)
		}
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s command %q is not available: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s command %q is a directory", label, path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s command %q is not executable", label, path)
	}
	return nil
}

func checkReadableFile(label string, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s %q is not readable: %w", label, path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%s %q close failed: %w", label, path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %q is not available: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %q is a directory", label, path)
	}
	return nil
}

func checkReadWriteFile(label string, path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%s %q is not readable and writable: %w", label, path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%s %q close failed: %w", label, path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %q is not available: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %q is a directory", label, path)
	}
	return nil
}

func checkDirectory(label string, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %q is not available: %w", label, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", label, path)
	}
	return nil
}

func ensureDirectory(label string, path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("%s %q could not be created: %w", label, path, err)
	}
	return checkDirectory(label, path)
}

func checkCgroup(version string) error {
	switch version {
	case "2":
		if err := checkReadableFile("cgroup v2 controllers", "/sys/fs/cgroup/cgroup.controllers"); err != nil {
			return err
		}
	case "1":
		if err := checkDirectory("cgroup filesystem", "/sys/fs/cgroup"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported firecracker cgroup version %q", version)
	}
	return nil
}
