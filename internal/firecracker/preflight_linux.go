//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (c *Connector) Preflight(ctx context.Context) error {
	var problems []error
	problems = append(problems,
		checkCommand("firecracker", c.cfg.FirecrackerPath),
		checkCommand("firecracker jailer", c.cfg.JailerPath),
		checkCommand("ip", c.cfg.IPPath),
		checkCommand("nft", c.cfg.NFTPath),
		checkReadableFile("guest kernel", c.cfg.KernelPath),
		checkReadableFile("guest initramfs", c.cfg.InitramfsPath),
		checkReadableFile("guest rootfs", c.cfg.RootfsPath),
		checkReadWriteFile("KVM device", c.cfg.KVMPath),
		checkReadWriteFile("TUN device", "/dev/net/tun"),
		checkCgroup(c.cfg.CgroupVersion),
		checkDirectory("CNI config directory", c.cfg.CNIConfDir),
		checkDirectory("CNI plugin directory", c.cfg.CNIBinDir),
		checkCommand("CNI tc-redirect-tap plugin", filepath.Join(c.cfg.CNIBinDir, "tc-redirect-tap")),
		checkCNINetworkConfig(c.cfg.CNIConfDir, c.cfg.CNINetworkName),
	)
	if c.cfg.CNICacheDir != "" {
		problems = append(problems, ensureDirectory("CNI cache directory", c.cfg.CNICacheDir))
	}
	problems = append(problems, ensureDirectory("firecracker state directory", c.cfg.StateDir))
	problems = append(problems, ensureDirectory("firecracker jailer chroot directory", c.cfg.JailerChrootBaseDir))
	if err := ctx.Err(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
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

func checkCNINetworkConfig(confDir string, networkName string) error {
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return fmt.Errorf("read CNI config directory %q: %w", confDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".conf") && !strings.HasSuffix(name, ".conflist") && !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(confDir, name))
		if err != nil {
			return fmt.Errorf("read CNI config %q: %w", name, err)
		}
		var cfg struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("decode CNI config %q: %w", name, err)
		}
		if cfg.Name == networkName {
			return nil
		}
	}
	return fmt.Errorf("CNI network %q was not found in %q", networkName, confDir)
}
