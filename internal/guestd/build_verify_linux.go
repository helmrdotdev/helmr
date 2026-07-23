//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
)

func handleBuildAnalysis(
	ctx context.Context,
	conn io.ReadWriter,
	bodyLen uint64,
) (returnErr error) {
	raw, err := readExactBuildRequest(conn, bodyLen)
	if err != nil {
		return err
	}
	request, err := deployment.ParseBuildAnalysisRequest(raw)
	if err != nil {
		return err
	}
	components := []buildComponent{
		{
			artifact: runtimeBuildArtifact(request.Runtime),
			device:   "/dev/vdc",
			name:     "runtime",
		},
		{
			artifact: request.StandardToolchain.ToolchainClosure,
			device:   "/dev/vdd",
			name:     "toolchain",
		},
		{
			artifact: deployment.ArtifactDescriptor{
				Digest:    request.Tree.Digest,
				MediaType: "application/vnd.helmr.internal-build-tree.v0+squashfs",
				SizeBytes: request.Tree.SizeBytes,
			},
			device: "/dev/vde",
			name:   "project",
		},
	}
	staged, err := stageBuildComponents(ctx, components)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, staged.Close())
	}()
	runtimePath, err := staged.Path("runtime")
	if err != nil {
		return err
	}
	toolchain, err := staged.Path("toolchain")
	if err != nil {
		return err
	}
	project, err := staged.Path("project")
	if err != nil {
		return err
	}
	result, err := runBuildVerifier(ctx, buildProcessConfig{
		Aliases: buildInstallAliases(request.Runtime.Architecture),
		Command: buildCommand{
			Argv: []string{
				"/opt/helmr/runtime/bin/node",
				"--experimental-transform-types",
				"--import=file:///opt/helmr/runtime/helmr/preload.mjs",
				"--input-type=module",
				"--eval",
				`import { runAnalysis } from "file:///opt/helmr/runtime/helmr/entry.mjs"; await runAnalysis("/opt/helmr/program", process.arch === "arm64" ? "aarch64" : "x86_64");`,
			},
			CWD: "/opt/helmr/program",
		},
		Environment: []buildEnvironment{
			{Name: "HELMR_SUPERVISOR_FD", Value: "4"},
			{Name: "HOME", Value: "/work/home"},
			{Name: "PATH", Value: "/opt/helmr/runtime/bin:/nix/bin"},
			{Name: "TMPDIR", Value: "/tmp"},
		},
		Identity:    buildIdentity{UID: buildInstallUID, GID: buildInstallGID},
		Network:     false,
		OutputLimit: buildInstallOutputLimit,
		Project:     project,
		Runtime:     runtimePath,
		Supervisor:  true,
		Toolchain:   toolchain,
	}, 10*time.Minute)
	if err != nil {
		return err
	}
	if result.waitErr != nil || len(result.supervisor) == 0 {
		return errors.New("build analysis Runtime failed without a valid result frame")
	}
	_, err = conn.Write(result.supervisor)
	return err
}

func handleProgramProof(
	ctx context.Context,
	conn io.ReadWriter,
	bodyLen uint64,
) (returnErr error) {
	raw, err := readExactBuildRequest(conn, bodyLen)
	if err != nil {
		return err
	}
	request, err := deployment.ParseProgramProofRequest(raw)
	if err != nil {
		return err
	}
	components := []buildComponent{
		{
			artifact: runtimeBuildArtifact(request.Runtime),
			device:   "/dev/vdc",
			name:     "runtime",
		},
		{
			artifact: programBuildArtifact(request.Code),
			device:   "/dev/vdd",
			name:     "code",
		},
		{
			artifact: programBuildArtifact(request.Dependencies),
			device:   "/dev/vde",
			name:     "dependencies",
		},
	}
	staged, err := stageBuildComponents(ctx, components)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, staged.Close())
	}()
	runtimePath, err := staged.Path("runtime")
	if err != nil {
		return err
	}
	code, err := staged.Path("code")
	if err != nil {
		return err
	}
	dependencies, err := staged.Path("dependencies")
	if err != nil {
		return err
	}
	result, err := runBuildVerifier(ctx, buildProcessConfig{
		Command: buildCommand{
			Argv: []string{
				"/opt/helmr/runtime/bin/node",
				"--experimental-transform-types",
				"--import=file:///opt/helmr/runtime/helmr/preload.mjs",
				"/opt/helmr/program/helmr/entry.mjs",
			},
			CWD: "/opt/helmr/program",
		},
		Dependencies: dependencies,
		Environment: []buildEnvironment{
			{Name: "HELMR_PROGRAM_MODE", Value: "proof"},
			{Name: "HELMR_SUPERVISOR_FD", Value: "4"},
			{Name: "HOME", Value: "/work/home"},
			{Name: "PATH", Value: "/opt/helmr/runtime/bin"},
			{Name: "TMPDIR", Value: "/tmp"},
		},
		Identity:    buildIdentity{UID: buildInstallUID, GID: buildInstallGID},
		Network:     false,
		OutputLimit: buildInstallOutputLimit,
		Project:     code,
		Runtime:     runtimePath,
		Supervisor:  true,
	}, 10*time.Minute)
	if err != nil {
		return err
	}
	if result.waitErr != nil || len(result.supervisor) == 0 {
		return errors.New("Program proof Runtime failed without a valid result frame")
	}
	_, err = conn.Write(result.supervisor)
	return err
}

func readExactBuildRequest(
	source io.Reader,
	bodyLen uint64,
) ([]byte, error) {
	body := &io.LimitedReader{R: source, N: int64(bodyLen)}
	raw, err := frameio.ReadMessageFrameBounded(body, 64<<10)
	if err != nil {
		return nil, err
	}
	if body.N != 0 {
		return nil, errors.New("build request contains trailing data")
	}
	return raw, nil
}

func runBuildVerifier(
	ctx context.Context,
	config buildProcessConfig,
	deadline time.Duration,
) (_ buildCommandResult, returnErr error) {
	plan := buildProcessPlan{
		Aliases:  config.Aliases,
		Identity: config.Identity,
	}
	root, err := prepareBuildProcessRoot(plan)
	if err != nil {
		return buildCommandResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, removeBuildProcessRoot(root))
	}()
	config.ProcessRoot = root
	limits := buildLimits{
		CPUPeriodMicros: 100000,
		CPUQuotaMicros:  200000,
		MemoryBytes:     2 << 30,
		PIDs:            1024,
	}
	cgroupPath, cgroup, err := createBuildCgroup(limits)
	if err != nil {
		return buildCommandResult{}, err
	}
	cgroupClosed := false
	defer func() {
		if !cgroupClosed {
			returnErr = errors.Join(
				returnErr,
				cleanupBuildCgroup(cgroupPath, cgroup, true),
			)
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	result, interrupted, err := runBuildCommand(runCtx, config, cgroup)
	if err != nil {
		return buildCommandResult{}, err
	}
	if err := cleanupBuildCgroup(cgroupPath, cgroup, true); err != nil {
		return buildCommandResult{}, err
	}
	cgroupClosed = true
	if interrupted {
		if ctx.Err() != nil {
			return buildCommandResult{}, ctx.Err()
		}
		return buildCommandResult{}, fmt.Errorf(
			"build verifier exceeded its %s deadline",
			deadline,
		)
	}
	return result, nil
}

func programBuildArtifact(
	descriptor deployment.ProgramDescriptor,
) deployment.ArtifactDescriptor {
	return deployment.ArtifactDescriptor{
		Digest:    descriptor.Digest,
		MediaType: descriptor.MediaType,
		SizeBytes: descriptor.SizeBytes,
	}
}
