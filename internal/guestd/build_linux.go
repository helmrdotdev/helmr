//go:build linux

package guestd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
)

const (
	buildUID               = 65532
	buildGID               = 65532
	compilerDocumentLimit  = 16 << 20
	stagedBuildOutputLimit = 16 << 20
)

type buildInputs map[string][sha256.Size]byte

func handleBuild(
	ctx context.Context,
	conn io.ReadWriter,
	bodyLen uint64,
) (returnErr error) {
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	raw, err := frameio.ReadMessageFrameBounded(body, 64<<10)
	if err != nil {
		return fmt.Errorf("read build request: %w", err)
	}
	request, err := deployment.ParseBuildGuestRequest(raw)
	if err != nil {
		return err
	}
	if body.N != request.SourceSizeBytes {
		return fmt.Errorf(
			"build source size = %d, want %d",
			body.N,
			request.SourceSizeBytes,
		)
	}
	components := []buildComponent{
		{
			artifact: request.Manager.Artifact,
			device:   "/dev/vdc",
			name:     "manager",
		},
		{
			artifact: request.Runtime.Artifact,
			device:   "/dev/vdd",
			name:     "runtime",
		},
		{
			artifact: request.Toolchain.Artifact,
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

	plan := buildProcessPlan{
		Aliases:  buildAliases(),
		Identity: buildIdentity{UID: buildUID, GID: buildGID},
	}
	root, err := prepareBuildProcessRoot(plan)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, removeBuildProcessRoot(root))
	}()
	project := filepath.Join(root, "work/project")
	output := filepath.Join(root, "work/output")
	hasher := sha256.New()
	source := io.TeeReader(body, hasher)
	if err := archive.ExtractTar(source, project); err != nil {
		return writeBuildFailure(
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
	if err := chownBuildTree(project, buildUID, buildGID); err != nil {
		return err
	}
	protected, err := snapshotBuildInputs(project, request.LockfileName)
	if err != nil {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureInvalidSource,
			err.Error(),
			nil,
		)
	}

	installConfig := buildProcessConfig{
		Aliases:     plan.Aliases,
		Command:     buildInstallCommand(request.Manager),
		Environment: buildProcessEnvironment(),
		Identity:    plan.Identity,
		Manager:     manager,
		OutputLimit: stagedBuildOutputLimit,
		ProcessRoot: root,
		Runtime:     runtimePath,
		Toolchain:   toolchain,
	}
	install, interrupted, err := runBuildPhase(
		ctx,
		installConfig,
		30*time.Minute,
	)
	if err != nil {
		return err
	}
	results := []buildCommandResult{install}
	installLogs := commandLogs(install)
	if interrupted {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return writeBuildFailure(
			conn,
			deployment.BuildFailureInstallLifecycle,
			"install/lifecycle exceeded the 30-minute deadline",
			installLogs,
		)
	}
	if install.waitErr != nil {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureInstallLifecycle,
			fmt.Sprintf(
				"install/lifecycle exited with status %d",
				installLogs.ExitStatus,
			),
			installLogs,
		)
	}
	if err := compareBuildInputs(
		project,
		request.LockfileName,
		protected,
	); err != nil {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureProtectedInput,
			err.Error(),
			installLogs,
		)
	}
	nodeFlags, err := deployment.NodeProgramFlags(request.Runtime.NodeVersion)
	if err != nil {
		return err
	}
	configCommand := buildProcessConfig{
		Aliases: buildAliases(),
		Command: buildCommand{
			Argv: append([]string{
				"/opt/helmr/runtime/bin/node",
			}, append(nodeFlags, []string{
				"/nix/helmr/config-evaluator.mjs",
				"/opt/helmr/program",
				request.Runtime.NodeVersion,
				"/opt/helmr/output",
			}...)...),
			CWD: "/opt/helmr/program",
		},
		Environment: []buildEnvironment{
			{Name: "HELMR_SUPERVISOR_FD", Value: "4"},
			{Name: "HOME", Value: "/work/home"},
			{Name: "LANG", Value: "C.UTF-8"},
			{Name: "PATH", Value: "/opt/helmr/runtime/bin"},
			{Name: "TMPDIR", Value: "/tmp"},
		},
		Identity:    buildIdentity{UID: buildUID, GID: buildGID},
		Output:      output,
		OutputLimit: stagedBuildOutputLimit,
		Project:     project,
		Runtime:     runtimePath,
		Supervisor:  true,
		Toolchain:   toolchain,
	}
	evaluated, err := runBuildVerifier(
		ctx,
		configCommand,
		10*time.Minute,
		nil,
	)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return writeBuildFailure(
			conn,
			deployment.BuildFailureConfigEvaluation,
			err.Error(),
			combinedCommandLogs(results),
		)
	}
	results = append(results, evaluated)
	if evaluated.waitErr != nil || len(evaluated.supervisor) == 0 {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureConfigEvaluation,
			"config evaluation failed without a valid result frame",
			combinedCommandLogs(results),
		)
	}
	config, err := deployment.ReadBuildConfigFrame(
		bytes.NewReader(evaluated.supervisor),
	)
	if err != nil {
		return fmt.Errorf("decode config evaluator result: %w", err)
	}
	canonicalConfig, err := deployment.CanonicalBuildConfig(config)
	if err != nil {
		return err
	}
	declarationCommand := configCommand
	declarationCommand.Command = buildCommand{
		Argv: append([]string{
			"/opt/helmr/runtime/bin/node",
		}, append(nodeFlags, []string{
			"/nix/helmr/program-compiler.mjs",
			"/opt/helmr/program",
			"/work/config.json",
			request.Runtime.NodeVersion,
			"/opt/helmr/output",
		}...)...),
		CWD: "/opt/helmr/program",
	}
	analyzed, err := runBuildVerifier(
		ctx,
		declarationCommand,
		10*time.Minute,
		canonicalConfig,
	)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return writeBuildFailure(
			conn,
			deployment.BuildFailureDeclarationAnalysis,
			err.Error(),
			combinedCommandLogs(results),
		)
	}
	results = append(results, analyzed)
	if analyzed.waitErr != nil || len(analyzed.supervisor) == 0 {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureDeclarationAnalysis,
			"declaration analysis failed without a valid result frame",
			combinedCommandLogs(results),
		)
	}
	verification, err := deployment.ReadVerificationResultFrame(
		bytes.NewReader(analyzed.supervisor),
	)
	if err != nil {
		return fmt.Errorf("decode declaration analyzer result: %w", err)
	}
	if verification.Outcome == deployment.VerificationOutcomeFailed {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureDeclarationAnalysis,
			verification.Failed.Error.Message,
			combinedCommandLogs(results),
		)
	}
	if err := ingestCompilerOutput(project, output); err != nil {
		return writeBuildFailure(
			conn,
			deployment.BuildFailureDeclarationAnalysis,
			err.Error(),
			combinedCommandLogs(results),
		)
	}

	artifact, cleanup, err := archive.CreateTarWithOptions(
		project,
		filepath.Dir(root),
		archive.TarOptions{},
	)
	if err != nil {
		return fmt.Errorf("stream post-build tree: %w", err)
	}
	defer cleanup()
	digest, size, err := frameio.HashFile(artifact.Path)
	if err != nil {
		return fmt.Errorf("hash post-build tree stream: %w", err)
	}
	resultRaw, err := deployment.CanonicalBuildGuestResult(
		deployment.BuildGuestResult{
			FormatVersion: deployment.BuildGuestFormatVersion,
			Outcome:       deployment.BuildGuestSucceeded,
			TreeDigest:    digest,
			TreeSizeBytes: size,
			Config:        &config,
			Verification:  &verification,
			Logs:          combinedCommandLogs(results),
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

func ingestCompilerOutput(project, output string) error {
	resultPath := filepath.Join(output, "helmr/compiler-result.json")
	resultFile, err := os.Open(resultPath)
	if err != nil {
		return fmt.Errorf("open compiler result: %w", err)
	}
	resultRaw, readErr := io.ReadAll(io.LimitReader(
		resultFile,
		compilerDocumentLimit+1,
	))
	closeErr := resultFile.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read compiler result: %w", err)
	}
	if len(resultRaw) > compilerDocumentLimit {
		return errors.New("compiler result exceeds the v0 document bound")
	}
	result, err := deployment.ParseProgramCompilerResult(resultRaw)
	if err != nil {
		return fmt.Errorf("parse compiler result: %w", err)
	}
	filesByPath := map[string]struct{}{
		"helmr/config.json":          {},
		"helmr/compiler-result.json": {},
	}
	for _, generated := range result.Outputs {
		filesByPath[generated.ModulePath] = struct{}{}
		filesByPath[generated.SourceMapPath] = struct{}{}
	}
	directories := map[string]struct{}{".": {}}
	for name := range filesByPath {
		for directory := filepath.ToSlash(filepath.Dir(name)); directory != "."; {
			directories[directory] = struct{}{}
			directory = filepath.ToSlash(filepath.Dir(directory))
		}
	}
	var files int
	var total int64
	if err := filepath.WalkDir(
		output,
		func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(output, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.IsDir() {
				if _, ok := directories[relative]; !ok {
					return fmt.Errorf("compiler output directory %q is not allowed", relative)
				}
				return nil
			}
			if info.Mode()&os.ModeType != 0 {
				return fmt.Errorf("compiler output %q is not a regular file", relative)
			}
			if _, ok := filesByPath[relative]; !ok {
				return fmt.Errorf("compiler output path %q is not allowed", relative)
			}
			files++
			total += info.Size()
			if files > 8194 || info.Size() <= 0 ||
				info.Size() > 16<<20 || total > 128<<20 {
				return errors.New("compiler output exceeds the v0 bounds")
			}
			return nil
		},
	); err != nil {
		return fmt.Errorf("validate compiler output: %w", err)
	}
	if files != len(filesByPath) {
		return errors.New("compiler output is incomplete")
	}
	metadataTarget := filepath.Join(project, "helmr")
	if _, err := os.Lstat(metadataTarget); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("submitted source contains reserved path \"helmr\"")
		}
		return fmt.Errorf("stat program output target: %w", err)
	}
	if err := os.Rename(filepath.Join(output, "helmr"), metadataTarget); err != nil {
		return fmt.Errorf("ingest compiler metadata: %w", err)
	}
	generatedDirectories := make(map[string]struct{}, len(result.Outputs))
	for _, generated := range result.Outputs {
		directory, ok := generatedOutputDirectory(generated.ModulePath)
		if !ok {
			return fmt.Errorf(
				"compiler output path %q has no reserved output directory",
				generated.ModulePath,
			)
		}
		generatedDirectories[directory] = struct{}{}
	}
	orderedDirectories := make([]string, 0, len(generatedDirectories))
	for directory := range generatedDirectories {
		orderedDirectories = append(orderedDirectories, directory)
	}
	sort.Strings(orderedDirectories)
	for _, directory := range orderedDirectories {
		target := filepath.Join(project, filepath.FromSlash(directory))
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf(
					"post-install tree contains reserved path %q",
					directory,
				)
			}
			return fmt.Errorf("stat generated output target: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(target, "modules"), 0o755); err != nil {
			return fmt.Errorf("create generated output target: %w", err)
		}
	}
	orderedFiles := make([]string, 0, len(filesByPath)-2)
	for name := range filesByPath {
		if strings.HasPrefix(name, "helmr/") {
			continue
		}
		orderedFiles = append(orderedFiles, name)
	}
	sort.Strings(orderedFiles)
	for _, name := range orderedFiles {
		source := filepath.Join(output, filepath.FromSlash(name))
		target := filepath.Join(project, filepath.FromSlash(name))
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("ingest compiler output %q: %w", name, err)
		}
	}
	return nil
}

func generatedOutputDirectory(name string) (string, bool) {
	const root = ".helmr/modules/"
	if strings.HasPrefix(name, root) {
		return ".helmr", true
	}
	const nested = "/.helmr/modules/"
	index := strings.LastIndex(name, nested)
	if index <= 0 {
		return "", false
	}
	return name[:index] + "/.helmr", true
}

func buildAliases() []buildAlias {
	return []buildAlias{
		{Path: "/bin/sh", Target: "/nix/bin/sh"},
		{
			Path:   "/lib64/ld-linux-x86-64.so.2",
			Target: "/nix/helmr/manager/lib/ld-linux-x86-64.so.2",
		},
		{Path: "/usr/bin/env", Target: "/nix/bin/env"},
	}
}

func managerCommand(
	manager deployment.BuildManager,
	arguments ...string,
) buildCommand {
	argv := []string{
		"/opt/helmr/runtime/bin/node",
		manager.Entrypoint.Path,
	}
	if manager.PackageManager.Name == deployment.PackageManagerBun {
		argv = argv[1:]
	}
	argv = append(argv, arguments...)
	return buildCommand{Argv: argv, CWD: "/work/project"}
}

func buildInstallCommand(manager deployment.BuildManager) buildCommand {
	switch manager.PackageManager.Name {
	case deployment.PackageManagerNPM:
		return managerCommand(
			manager,
			"ci",
			"--no-audit",
			"--no-fund",
		)
	case deployment.PackageManagerPNPM:
		return managerCommand(
			manager,
			"install",
			"--frozen-lockfile",
			"--no-runtime",
			"--pm-on-fail=error",
		)
	case deployment.PackageManagerBun:
		return managerCommand(
			manager,
			"install",
			"--frozen-lockfile",
		)
	default:
		panic("validated Manager family is unsupported")
	}
}

func buildProcessEnvironment() []buildEnvironment {
	return []buildEnvironment{
		{Name: "HOME", Value: "/work/home"},
		{
			Name:  "PATH",
			Value: "/opt/helmr/manager/bin:/opt/helmr/runtime/bin:/nix/bin",
		},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "XDG_CACHE_HOME", Value: "/work/home/cache"},
	}
}

func runBuildPhase(
	ctx context.Context,
	config buildProcessConfig,
	deadline time.Duration,
) (_ buildCommandResult, interrupted bool, returnErr error) {
	limits := buildLimits{
		CPUPeriodMicros: 100000,
		CPUQuotaMicros:  200000,
		MemoryBytes:     2 << 30,
		PIDs:            1024,
	}
	cgroupPath, cgroup, err := createBuildCgroup(limits)
	if err != nil {
		return buildCommandResult{}, false, err
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
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	result, interrupted, err := runBuildCommand(runCtx, config, cgroup)
	if err != nil {
		return buildCommandResult{}, false, err
	}
	if err := cleanupBuildCgroup(cgroupPath, cgroup); err != nil {
		return buildCommandResult{}, false, err
	}
	cgroupClosed = true
	return result, interrupted, nil
}

func runBuildVerifier(
	ctx context.Context,
	config buildProcessConfig,
	deadline time.Duration,
	canonicalConfig []byte,
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
	if canonicalConfig != nil {
		path := filepath.Join(root, "work/config.json")
		if err := os.WriteFile(path, canonicalConfig, 0o444); err != nil {
			return buildCommandResult{}, fmt.Errorf(
				"write declaration analyzer config: %w",
				err,
			)
		}
		if err := os.Chown(path, buildUID, buildGID); err != nil {
			return buildCommandResult{}, fmt.Errorf(
				"own declaration analyzer config: %w",
				err,
			)
		}
	}
	config.ProcessRoot = root
	result, interrupted, err := runBuildPhase(ctx, config, deadline)
	if err != nil {
		return buildCommandResult{}, err
	}
	if interrupted {
		if ctx.Err() != nil {
			return buildCommandResult{}, ctx.Err()
		}
		return buildCommandResult{}, fmt.Errorf(
			"build analysis exceeded its %s deadline",
			deadline,
		)
	}
	return result, nil
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

func snapshotBuildInputs(root, lockfile string) (buildInputs, error) {
	inputs := make(buildInputs, 3)
	for _, name := range []string{
		"package.json",
		lockfile,
		"helmr.config.ts",
	} {
		digest, err := digestBuildInput(filepath.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("read protected build input %q: %w", name, err)
		}
		inputs[name] = digest
	}
	return inputs, nil
}

func compareBuildInputs(
	root string,
	lockfile string,
	expected buildInputs,
) error {
	actual, err := snapshotBuildInputs(root, lockfile)
	if err != nil {
		return err
	}
	for name, digest := range expected {
		if actual[name] != digest {
			return fmt.Errorf("protected build input %q changed", name)
		}
	}
	return nil
}

func digestBuildInput(path string) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	info, err := os.Lstat(path)
	if err != nil {
		return empty, err
	}
	if !info.Mode().IsRegular() {
		return empty, errors.New("input is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return empty, err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return empty, errors.Join(copyErr, closeErr)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func commandLogs(result buildCommandResult) *deployment.BuildLogs {
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

func combinedCommandLogs(results []buildCommandResult) *deployment.BuildLogs {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	truncated := false
	exitStatus := int32(0)
	for _, result := range results {
		logs := commandLogs(result)
		exitStatus = logs.ExitStatus
		truncated = truncated || logs.Truncated
		appendBuildOutput(
			&stdout,
			result.stdout,
			stdout.Len()+stderr.Len(),
			&truncated,
		)
		appendBuildOutput(
			&stderr,
			result.stderr,
			stdout.Len()+stderr.Len(),
			&truncated,
		)
	}
	return &deployment.BuildLogs{
		ExitStatus:   exitStatus,
		StderrBase64: base64.StdEncoding.EncodeToString(stderr.Bytes()),
		StdoutBase64: base64.StdEncoding.EncodeToString(stdout.Bytes()),
		Truncated:    truncated,
	}
}

func appendBuildOutput(
	output *bytes.Buffer,
	raw []byte,
	used int,
	truncated *bool,
) {
	remaining := stagedBuildOutputLimit - used
	if remaining <= 0 {
		if len(raw) != 0 {
			*truncated = true
		}
		return
	}
	if len(raw) > remaining {
		raw = raw[:remaining]
		*truncated = true
	}
	_, _ = output.Write(raw)
}

func writeBuildFailure(
	conn io.Writer,
	reason deployment.BuildFailureReason,
	message string,
	logs *deployment.BuildLogs,
) error {
	raw, err := deployment.CanonicalBuildGuestResult(
		deployment.BuildGuestResult{
			FormatVersion: deployment.BuildGuestFormatVersion,
			Outcome:       deployment.BuildGuestFailed,
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
