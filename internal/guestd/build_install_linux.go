//go:build linux

package guestd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
)

const (
	buildInstallUID         = 65532
	buildInstallGID         = 65532
	buildInstallOutputLimit = 16 << 20
)

func handleBuildInstall(
	ctx context.Context,
	conn io.ReadWriter,
	bodyLen uint64,
) (returnErr error) {
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	raw, err := frameio.ReadMessageFrameBounded(body, 64<<10)
	if err != nil {
		return fmt.Errorf("read build install request: %w", err)
	}
	request, err := deployment.ParseBuildInstallRequest(raw)
	if err != nil {
		return err
	}
	if body.N != request.SourceSizeBytes {
		return fmt.Errorf(
			"build install source size = %d, want %d",
			body.N,
			request.SourceSizeBytes,
		)
	}
	components := []buildComponent{
		{
			artifact: request.Manager.Tree,
			device:   "/dev/vdc",
			name:     "manager",
		},
		{
			artifact: runtimeBuildArtifact(request.Runtime),
			device:   "/dev/vdd",
			name:     "runtime",
		},
		{
			artifact: request.StandardToolchain.ToolchainClosure,
			device:   "/dev/vde",
			name:     "toolchain",
		},
	}
	staged, err := stageBuildComponents(ctx, components)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, staged.Close())
	}()

	plan := buildProcessPlan{
		Aliases:  buildInstallAliases(),
		Identity: buildIdentity{UID: buildInstallUID, GID: buildInstallGID},
	}
	root, err := prepareBuildProcessRoot(plan)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, removeBuildProcessRoot(root))
	}()
	project := filepath.Join(root, "work/project")
	hasher := sha256.New()
	source := io.TeeReader(body, hasher)
	if err := archive.ExtractTar(source, project); err != nil {
		return writeBuildInstallFailure(
			conn,
			deployment.BuildFailureInvalidSource,
			fmt.Sprintf("extract submitted source: %s", err),
			nil,
		)
	}
	if body.N != 0 {
		return errors.New("submitted source stream is truncated")
	}
	actualSourceDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actualSourceDigest != request.SourceDigest {
		return errors.New("submitted source stream digest does not match request")
	}
	if err := chownBuildTree(project, buildInstallUID, buildInstallGID); err != nil {
		return err
	}

	manager, err := staged.Path("manager")
	if err != nil {
		return err
	}
	runtimePath, err := staged.Path("runtime")
	if err != nil {
		return err
	}
	toolchain, err := staged.Path("toolchain")
	if err != nil {
		return err
	}
	command := buildInstallCommand(request.Manager)
	config := buildProcessConfig{
		Aliases:     plan.Aliases,
		Command:     command,
		Environment: buildInstallEnvironment(request.Manager.PackageManager.Name),
		Identity:    plan.Identity,
		Manager:     manager,
		Network:     true,
		OutputLimit: buildInstallOutputLimit,
		ProcessRoot: root,
		Runtime:     runtimePath,
		Toolchain:   toolchain,
	}
	limits := buildLimits{
		CPUPeriodMicros: 100000,
		CPUQuotaMicros:  200000,
		MemoryBytes:     2 << 30,
		PIDs:            1024,
	}
	cgroupPath, cgroup, err := createBuildCgroup(limits)
	if err != nil {
		return err
	}
	cgroupClosed := false
	defer func() {
		if !cgroupClosed {
			returnErr = errors.Join(
				returnErr,
				cleanupBuildCgroup(cgroupPath, cgroup),
			)
		}
	}()
	installCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, interrupted, err := runBuildCommand(installCtx, config, cgroup)
	if err != nil {
		return err
	}
	if err := cleanupBuildCgroup(cgroupPath, cgroup); err != nil {
		return err
	}
	cgroupClosed = true
	if interrupted {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return writeBuildInstallFailure(
			conn,
			deployment.BuildFailureManagerFailed,
			"package manager exceeded the 30-minute build deadline",
			buildInstallLogs(result),
		)
	}
	if result.waitErr != nil {
		logs := buildInstallLogs(result)
		return writeBuildInstallFailure(
			conn,
			deployment.BuildFailureManagerFailed,
			fmt.Sprintf("package manager exited with status %d", logs.ExitStatus),
			logs,
		)
	}
	logs := buildInstallLogs(result)

	artifact, cleanup, err := archive.CreateTarWithOptions(
		project,
		filepath.Dir(root),
		archive.TarOptions{},
	)
	if err != nil {
		return fmt.Errorf("stream frozen build tree: %w", err)
	}
	defer cleanup()
	digest, size, err := frameio.HashFile(artifact.Path)
	if err != nil {
		return fmt.Errorf("hash frozen build tree stream: %w", err)
	}
	resultRaw, err := deployment.CanonicalBuildInstallResult(
		deployment.BuildInstallResult{
			FormatVersion: deployment.BuildGuestFormatVersion,
			Outcome:       deployment.BuildInstallSucceeded,
			TreeDigest:    digest,
			TreeSizeBytes: size,
			Logs:          logs,
		},
	)
	if err != nil {
		return err
	}
	if err := frameio.WriteMessageFrame(conn, resultRaw); err != nil {
		return err
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(conn, file)
	closeErr := file.Close()
	return errors.Join(copyErr, closeErr)
}

func buildInstallAliases() []buildAlias {
	return []buildAlias{
		{Path: "/bin/sh", Target: "/nix/bin/sh"},
		{
			Path:   "/lib64/ld-linux-x86-64.so.2",
			Target: "/nix/helmr/manager/lib/ld-linux-x86-64.so.2",
		},
		{Path: "/usr/bin/env", Target: "/nix/bin/env"},
	}
}

func buildInstallCommand(
	manager deployment.Manager,
) buildCommand {
	argv := []string{
		"/opt/helmr/runtime/bin/node",
		manager.Entrypoint.Path,
		"ci",
	}
	if manager.PackageManager.Name == deployment.PackageManagerBun {
		argv = []string{
			manager.Entrypoint.Path,
			"install",
			"--frozen-lockfile",
		}
	}
	return buildCommand{Argv: argv, CWD: "/work/project"}
}

func buildInstallEnvironment(
	manager deployment.PackageManagerName,
) []buildEnvironment {
	environment := []buildEnvironment{
		{Name: "HOME", Value: "/work/home"},
		{Name: "PATH", Value: "/opt/helmr/manager/bin:/opt/helmr/runtime/bin:/nix/bin"},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "XDG_CACHE_HOME", Value: "/work/cache"},
	}
	if manager == deployment.PackageManagerNPM {
		environment = append(environment,
			buildEnvironment{Name: "npm_config_audit", Value: "false"},
			buildEnvironment{Name: "npm_config_fund", Value: "false"},
			buildEnvironment{Name: "npm_config_progress", Value: "false"},
			buildEnvironment{Name: "npm_config_update_notifier", Value: "false"},
		)
	}
	return environment
}

func chownBuildTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("own build path %q: %w", path, err)
		}
		return nil
	})
}

func buildInstallLogs(result buildCommandResult) *deployment.BuildLogs {
	exitStatus := int32(0)
	if result.waitErr != nil {
		exitStatus = -1
		var exitError *exec.ExitError
		if errors.As(result.waitErr, &exitError) {
			exitStatus = int32(exitError.ExitCode())
		}
	}
	return &deployment.BuildLogs{
		ExitStatus:   exitStatus,
		StderrBase64: base64.StdEncoding.EncodeToString(result.stderr),
		StdoutBase64: base64.StdEncoding.EncodeToString(result.stdout),
		Truncated:    result.overflow,
	}
}

func writeBuildInstallFailure(
	conn io.Writer,
	reason deployment.BuildFailureReason,
	message string,
	logs *deployment.BuildLogs,
) error {
	raw, err := deployment.CanonicalBuildInstallResult(
		deployment.BuildInstallResult{
			FormatVersion: deployment.BuildGuestFormatVersion,
			Outcome:       deployment.BuildInstallFailed,
			Error: &deployment.BuildError{
				ReasonCode: reason,
				Message:    message,
			},
			Logs: logs,
		},
	)
	if err != nil {
		return err
	}
	return frameio.WriteMessageFrame(conn, raw)
}

func runtimeBuildArtifact(
	runtime deployment.RuntimeDescriptor,
) deployment.ArtifactDescriptor {
	return deployment.ArtifactDescriptor{
		Digest:    runtime.Digest,
		MediaType: runtime.MediaType,
		SizeBytes: runtime.SizeBytes,
	}
}
