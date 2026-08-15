//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"golang.org/x/sys/unix"
)

func runtimeArtifactCapabilities(artifacts runtimeArtifacts) (RuntimeCapabilities, error) {
	architecture, err := runtimeid.ArchitectureFromGo(artifacts.Arch)
	if err != nil {
		return RuntimeCapabilities{}, err
	}
	return RuntimeCapabilities{
		Arch: architecture, Contract: artifacts.VMRuntimeContract,
		KernelDigest: artifacts.Kernel.Digest, InitramfsDigest: artifacts.Initramfs.Digest,
		RootfsDigest: artifacts.Rootfs.Digest,
	}, nil
}

func defaultRuntimeProbeDependencies(unameRelease func() (string, error)) runtimeProbeDependencies {
	return runtimeProbeDependencies{
		run:          runRuntimeProbeCommand,
		lookPath:     exec.LookPath,
		unameRelease: unameRelease,
		readFile:     os.ReadFile,
	}
}

func (c *Connector) HostRuntimeEvidence(ctx context.Context) (HostRuntimeEvidence, error) {
	if c == nil {
		return HostRuntimeEvidence{}, errors.New("the Firecracker connector is nil")
	}
	evidence, err := inspectHostRuntime(ctx, c.cfg, c.artifacts, defaultRuntimeProbeDependencies(hostUnameRelease))
	if err != nil {
		return HostRuntimeEvidence{}, err
	}
	pinnedPath, err := pinRuntimeExecutable(evidence.firecrackerPath, c.cfg.StateDir, evidence.FirecrackerDigest)
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("pin measured Firecracker executable: %w", err)
	}
	evidence.firecrackerPath = pinnedPath
	if err := c.hostRuntime.bind(evidence, c.cfg.VCPUCount); err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("bind host runtime evidence: %w", err)
	}
	return evidence, nil
}

func hostUnameRelease() (string, error) {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return "", err
	}
	bytes := make([]byte, 0, len(name.Release))
	terminated := false
	for _, character := range name.Release {
		if character == 0 {
			terminated = true
			break
		}
		bytes = append(bytes, byte(character))
	}
	if !terminated {
		return "", errors.New("uname release is not NUL-terminated")
	}
	if len(bytes) == 0 || !utf8.Valid(bytes) {
		return "", fmt.Errorf("uname release is not valid non-empty UTF-8")
	}
	return string(bytes), nil
}
