//go:build linux

package worker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var verifierControllers = []string{"cpu", "memory", "pids"}

func PrepareVerifierHost() (string, error) {
	return checkVerifierHost("/proc/self/cgroup", "/sys/fs/cgroup", os.Getpid(), true)
}

func prepareVerifierHost(procCgroup, cgroupRoot string, pid int) (string, error) {
	return checkVerifierHost(procCgroup, cgroupRoot, pid, true)
}

func programVerifierHostHealthy() bool {
	_, err := checkVerifierHost("/proc/self/cgroup", "/sys/fs/cgroup", os.Getpid(), false)
	return err == nil
}

func checkVerifierHost(procCgroup, cgroupRoot string, pid int, enable bool) (string, error) {
	raw, err := os.ReadFile(procCgroup)
	if err != nil {
		return "", fmt.Errorf("read worker cgroup: %w", err)
	}
	current, err := unifiedCgroupPath(raw)
	if err != nil {
		return "", err
	}
	if filepath.Base(current) != "supervisor" {
		return "", fmt.Errorf("worker cgroup %q is not the delegated supervisor leaf", current)
	}

	currentPath := filepath.Join(cgroupRoot, strings.TrimPrefix(current, "/"))
	unitPath := filepath.Dir(currentPath)
	if !pathWithin(cgroupRoot, unitPath) {
		return "", errors.New("worker cgroup escapes the unified hierarchy")
	}
	if raw, err := os.ReadFile(filepath.Join(unitPath, "cgroup.procs")); err != nil {
		return "", fmt.Errorf("read verifier cgroup root processes: %w", err)
	} else if len(bytes.TrimSpace(raw)) != 0 {
		return "", errors.New("verifier cgroup root is not process-free")
	}
	if raw, err := os.ReadFile(filepath.Join(currentPath, "cgroup.procs")); err != nil {
		return "", fmt.Errorf("read supervisor processes: %w", err)
	} else if !lineSet(raw)[strconv.Itoa(pid)] {
		return "", errors.New("worker process is outside the supervisor leaf")
	}

	available, err := readControllerSet(filepath.Join(unitPath, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read delegated controllers: %w", err)
	}
	for _, controller := range verifierControllers {
		if !available[controller] {
			return "", fmt.Errorf("verifier controller %q is not delegated", controller)
		}
	}
	if _, err := os.Stat(filepath.Join(unitPath, "cgroup.kill")); err != nil {
		return "", fmt.Errorf("verifier subtree kill is unavailable: %w", err)
	}
	if _, err := os.Stat(filepath.Join(unitPath, "memory.swap.max")); err != nil {
		return "", fmt.Errorf("verifier swap limit is unavailable: %w", err)
	}
	if _, err := os.Stat(filepath.Join(unitPath, "memory.peak")); err != nil {
		return "", fmt.Errorf("verifier peak memory accounting is unavailable: %w", err)
	}

	controlPath := filepath.Join(unitPath, "cgroup.subtree_control")
	if enable {
		if err := os.WriteFile(controlPath, []byte("+cpu +memory +pids"), 0o644); err != nil {
			return "", fmt.Errorf("enable verifier controllers: %w", err)
		}
	}
	enabled, err := readControllerSet(controlPath)
	if err != nil {
		return "", fmt.Errorf("read enabled verifier controllers: %w", err)
	}
	for _, controller := range verifierControllers {
		if !enabled[controller] {
			return "", fmt.Errorf("verifier controller %q was not enabled", controller)
		}
	}
	return unitPath, nil
}

func unifiedCgroupPath(raw []byte) (string, error) {
	var current string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" || current != "" {
			return "", errors.New("worker is not in one unified cgroup-v2 hierarchy")
		}
		current = filepath.Clean(parts[2])
	}
	if current == "" || !filepath.IsAbs(current) || current == "/" {
		return "", errors.New("worker unified cgroup path is invalid")
	}
	return current, nil
}

func readControllerSet(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	controllers := make(map[string]bool)
	for _, field := range strings.Fields(string(raw)) {
		controllers[strings.TrimPrefix(field, "+")] = true
	}
	return controllers, nil
}

func lineSet(raw []byte) map[string]bool {
	lines := make(map[string]bool)
	for _, line := range strings.Fields(string(raw)) {
		lines[line] = true
	}
	return lines
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
