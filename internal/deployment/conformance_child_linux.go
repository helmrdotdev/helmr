//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	conformanceRootSize = "size=3221225472,nr_inodes=300000,mode=0755"
	conformanceWorkSize = "size=536870912,nr_inodes=100000,mode=0700"
)

func isolateConformanceRoot(job verifierJob, uid, gid uint32) error {
	var stat unix.Stat_t
	if err := unix.Lstat(verifierRoot, &stat); err != nil {
		return fmt.Errorf("stat conformance root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("conformance root mount point is not a directory")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make conformance mount namespace private: %w", err)
	}
	if err := unix.Mount(
		"tmpfs",
		verifierRoot,
		"tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV,
		conformanceRootSize,
	); err != nil {
		return fmt.Errorf("mount conformance root: %w", err)
	}
	putOld := verifierRoot + verifierOldRoot
	if err := unix.Mkdir(putOld, 0700); err != nil {
		return fmt.Errorf("create conformance old root: %w", err)
	}
	if err := unix.PivotRoot(verifierRoot, putOld); err != nil {
		return fmt.Errorf("pivot conformance root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir conformance root: %w", err)
	}
	if err := unix.Unmount(verifierOldRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount conformance old root: %w", err)
	}
	if err := unix.Rmdir(verifierOldRoot); err != nil {
		return fmt.Errorf("remove conformance old root: %w", err)
	}
	for _, directory := range []string{"/opt", "/opt/helmr", "/work"} {
		if err := os.Mkdir(directory, 0755); err != nil {
			return err
		}
	}
	workOptions := conformanceWorkSize + ",uid=" + strconv.FormatUint(uint64(uid), 10) +
		",gid=" + strconv.FormatUint(uint64(gid), 10)
	if err := unix.Mount(
		"tmpfs",
		"/work",
		"tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV,
		workOptions,
	); err != nil {
		return fmt.Errorf("mount conformance work: %w", err)
	}
	if err := os.Mkdir("/work/tmp", 0700); err != nil {
		return err
	}
	if err := os.Chown("/work/tmp", int(uid), int(gid)); err != nil {
		return err
	}
	switch job {
	case runtimeConformanceJob:
		if err := materializeConformanceArtifact(
			context.Background(),
			verifierArtifactBaseFD,
			runtimeArtifact,
			"/opt/helmr/runtime",
		); err != nil {
			return err
		}
	case managerConformanceJob:
		if err := materializeConformanceArtifact(
			context.Background(),
			verifierArtifactBaseFD,
			runtimeArtifact,
			"/opt/helmr/runtime",
		); err != nil {
			return err
		}
		if err := materializeConformanceArtifact(
			context.Background(),
			verifierArtifactBaseFD+1,
			managerArtifact,
			"/opt/helmr/manager",
		); err != nil {
			return err
		}
	case toolchainConformanceJob:
		if err := materializeConformanceArtifact(
			context.Background(),
			verifierArtifactBaseFD,
			runtimeArtifact,
			"/opt/helmr/runtime",
		); err != nil {
			return err
		}
		if err := materializeConformanceArtifact(
			context.Background(),
			verifierArtifactBaseFD+1,
			toolchainArtifact,
			"/nix",
		); err != nil {
			return err
		}
	default:
		return fmt.Errorf("conformance job = %q", job)
	}
	if err := unix.Mount(
		"",
		"/",
		"",
		unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV,
		"",
	); err != nil {
		return fmt.Errorf("remount conformance root read-only: %w", err)
	}
	return nil
}

func materializeConformanceArtifact(
	ctx context.Context,
	fd int,
	role artifactRole,
	destination string,
) error {
	artifact, err := artifactFromDescriptor(
		ctx,
		fd,
		role,
		platformMediaType(role),
	)
	if err != nil {
		return err
	}
	maxLogical, err := platformLogicalLimit(role)
	if err != nil {
		return err
	}
	inspected, err := inspectArtifact(ctx, artifact.Reader, role, maxLogical, artifact.SizeBytes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	for _, entry := range inspected.ordered {
		if entry.Path == "." {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(entry.Path))
		switch entry.Kind {
		case artifactEntryDirectory:
			if err := os.Mkdir(target, os.FileMode(entry.Mode)); err != nil {
				return err
			}
		case artifactEntryRegular:
			input, err := inspected.reader.Open(ctx, entry.Path)
			if err != nil {
				return err
			}
			output, err := os.OpenFile(
				target,
				os.O_CREATE|os.O_EXCL|os.O_WRONLY,
				os.FileMode(entry.Mode),
			)
			if err != nil {
				_ = input.Close()
				return err
			}
			written, copyErr := io.Copy(output, io.LimitReader(input, entry.SizeBytes+1))
			closeErr := errors.Join(input.Close(), output.Close())
			if copyErr != nil || closeErr != nil || written != entry.SizeBytes {
				return errors.Join(copyErr, closeErr, errors.New("materialized Artifact size changed"))
			}
		case artifactEntrySymlink:
			if err := os.Symlink(entry.LinkTarget, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Artifact entry %q cannot be materialized", entry.Path)
		}
	}
	return nil
}

func platformMediaType(role artifactRole) string {
	switch role {
	case runtimeArtifact:
		return RuntimeArtifactMediaType
	case managerArtifact:
		return ManagerTreeMediaType
	case toolchainArtifact:
		return ToolchainMediaType
	default:
		return ""
	}
}

func platformLogicalLimit(role artifactRole) (int64, error) {
	switch role {
	case runtimeArtifact:
		return maxRuntimeLogicalBytes, nil
	case managerArtifact:
		return maxManagerTreeBytes, nil
	case toolchainArtifact:
		return maxToolArtifactBytes, nil
	default:
		return 0, fmt.Errorf("Platform Artifact role = %d", role)
	}
}

func verifyPlatformConformance(
	ctx context.Context,
	job verifierJob,
) ([]byte, error) {
	if _, err := net.DialTimeout("tcp", "1.1.1.1:443", 100*time.Millisecond); err == nil {
		return nil, errors.New("conformance validator has physical network access")
	}
	var results []PlatformConformanceResult
	switch job {
	case runtimeConformanceJob:
		raw, err := readConformanceDescriptor(verifierArtifactBaseFD + 1)
		if err != nil {
			return nil, err
		}
		var descriptor RuntimeArtifactDescriptor
		if err := parsePlatformDocument(raw, "Runtime acquisition descriptor", &descriptor); err != nil {
			return nil, err
		}
		results, err = runtimeConformance(ctx, descriptor)
		if err != nil {
			return nil, err
		}
	case managerConformanceJob:
		raw, err := readConformanceDescriptor(verifierArtifactBaseFD + 2)
		if err != nil {
			return nil, err
		}
		var descriptor ManagerArtifactDescriptor
		if err := parsePlatformDocument(raw, "Manager acquisition descriptor", &descriptor); err != nil {
			return nil, err
		}
		results, err = managerConformance(ctx, descriptor)
		if err != nil {
			return nil, err
		}
	case toolchainConformanceJob:
		raw, err := readConformanceDescriptor(verifierArtifactBaseFD + 2)
		if err != nil {
			return nil, err
		}
		var descriptor ToolchainArtifactDescriptor
		if err := parsePlatformDocument(raw, "toolchain acquisition descriptor", &descriptor); err != nil {
			return nil, err
		}
		results, err = toolchainConformance(ctx, descriptor)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("conformance job = %q", job)
	}
	return CanonicalPlatformDocument(PlatformConformance{
		FixtureSet:    "pending",
		FormatVersion: PlatformArtifactDocumentFormatVersion,
		Inputs:        []PlatformEvidenceFile{},
		Results:       results,
	})
}

func readConformanceDescriptor(fd int) ([]byte, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Size < 1 || stat.Size > maxPlatformArtifactDocumentBytes {
		return nil, errors.New("conformance descriptor size is invalid")
	}
	raw := make([]byte, int(stat.Size))
	if _, err := (verifierFDReader{fd: fd}).ReadAt(raw, 0); err != nil {
		return nil, err
	}
	return raw, nil
}

func runtimeConformance(
	ctx context.Context,
	descriptor RuntimeArtifactDescriptor,
) ([]PlatformConformanceResult, error) {
	node := "/opt/helmr/runtime/bin/node"
	version, err := runConformanceCommand(ctx, nil, node, "--version")
	if err != nil || strings.TrimSpace(version) != "v"+descriptor.NodeVersion {
		return nil, errors.New("Runtime Node reported the wrong version")
	}
	architecture, err := runConformanceCommand(ctx, nil, node, "-p", "process.arch")
	if err != nil || strings.TrimSpace(architecture) != "x64" {
		return nil, errors.New("Runtime Node reported the wrong architecture")
	}
	abi, err := runConformanceCommand(ctx, nil, node, "-p", "process.versions.modules")
	if err != nil || strings.TrimSpace(abi) != descriptor.NodeModuleABI {
		return nil, errors.New("Runtime Node reported the wrong module ABI")
	}
	if _, err := runConformanceCommand(ctx, nil, node, descriptor.ProgramNodeFlag, "-e", "0"); err != nil {
		return nil, errors.New("Runtime Node does not accept its TypeScript-disable flag")
	}
	program := []byte(`const value: string = "helmr"; if (value !== "helmr") process.exit(1);`)
	if err := os.WriteFile("/work/program.ts", program, 0600); err != nil {
		return nil, err
	}
	if _, err := runConformanceCommand(
		ctx,
		nil,
		node,
		descriptor.ProgramNodeFlag,
		"/work/program.ts",
	); err == nil {
		return nil, errors.New("Runtime Program mode accepted TypeScript")
	}
	if _, err := runConformanceCommand(ctx, nil, node, "--check", descriptor.Entrypoint); err != nil {
		return nil, errors.New("Runtime entrypoint is not valid JavaScript")
	}
	if _, err := runConformanceCommand(ctx, nil, node, "--check", descriptor.ConfigEvaluatorEntrypoint); err != nil {
		return nil, errors.New("Config Evaluator is not valid JavaScript")
	}
	if err := os.WriteFile("/work/config.ts", program, 0600); err != nil {
		return nil, err
	}
	if _, err := runConformanceCommand(ctx, nil, node, "/work/config.ts"); err != nil {
		return nil, errors.New("Runtime Node native TypeScript mode failed")
	}
	return passedConformanceResults(
		"config-native-typescript",
		"network-denied",
		"node-architecture",
		"node-disable-types",
		"node-module-abi",
		"node-reported-version",
		"runtime-entrypoint",
	), nil
}

func managerConformance(
	ctx context.Context,
	descriptor ManagerArtifactDescriptor,
) ([]PlatformConformanceResult, error) {
	entrypoint := descriptor.Entrypoint.Path
	executable := entrypoint
	var command []string
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		executable = "/opt/helmr/runtime/bin/node"
		command = []string{entrypoint, "--version"}
	} else {
		command = []string{"--version"}
	}
	version, err := runConformanceCommand(ctx, managerEnvironment(descriptor), executable, command...)
	if err != nil || strings.TrimSpace(version) != descriptor.PackageManager.Version {
		return nil, errors.New("Manager reported the wrong version")
	}
	help, err := runConformanceCommand(ctx, managerEnvironment(descriptor), executable, managerHelpArguments(descriptor)...)
	if err != nil {
		return nil, errors.New("Manager help failed")
	}
	for _, option := range managerRequiredOptions(descriptor.PackageManager.Name) {
		if !strings.Contains(help, option) {
			return nil, fmt.Errorf("Manager required option %q is missing", option)
		}
	}
	if err := managerAdapterFixtures(ctx, descriptor, executable); err != nil {
		return nil, err
	}
	return passedConformanceResults(
		"cache-miss-fails",
		"entrypoint",
		"executable-config-suppression",
		"network-denied",
		"protected-input-preservation",
		"reported-version",
		"required-options",
		"runtime-download-suppression",
		"scriptless-fetch",
	), nil
}

func managerAdapterFixtures(
	ctx context.Context,
	descriptor ManagerArtifactDescriptor,
	executable string,
) error {
	project := "/work/project"
	if err := os.Mkdir(project, 0700); err != nil {
		return err
	}
	packageJSON := []byte(`{"name":"helmr-manager-fixture","private":true,"scripts":{"preinstall":"node lifecycle.js"},"version":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(project, "package.json"), packageJSON, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(
		filepath.Join(project, "lifecycle.js"),
		[]byte(`require("node:fs").writeFileSync("/work/lifecycle-ran","1")`),
		0600,
	); err != nil {
		return err
	}
	lockName, lock := managerFixtureLock(descriptor.PackageManager.Name)
	if lockName == "" {
		return errors.New("Manager fixture family is unsupported")
	}
	lockPath := filepath.Join(project, lockName)
	if err := os.WriteFile(lockPath, []byte(lock), 0600); err != nil {
		return err
	}
	if descriptor.PackageManager.Name == PackageManagerPNPM {
		if err := os.WriteFile(
			filepath.Join(project, ".pnpmfile.cjs"),
			[]byte(`throw new Error("pnpmfile executed")`),
			0600,
		); err != nil {
			return err
		}
	}
	beforePackage, err := digestFile(ctx, filepath.Join(project, "package.json"))
	if err != nil {
		return err
	}
	beforeLock, err := digestFile(ctx, lockPath)
	if err != nil {
		return err
	}
	arguments := managerFixtureFetchArguments(descriptor)
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		arguments = append([]string{descriptor.Entrypoint.Path}, arguments...)
	}
	if _, err := runConformanceCommandIn(
		ctx,
		managerEnvironment(descriptor),
		project,
		executable,
		arguments...,
	); err != nil {
		return fmt.Errorf("Manager scriptless Fetch fixture failed: %w", err)
	}
	if _, err := os.Stat("/work/lifecycle-ran"); !os.IsNotExist(err) {
		return errors.New("Manager scriptless Fetch executed a lifecycle script")
	}
	if after, err := digestFile(ctx, filepath.Join(project, "package.json")); err != nil ||
		after != beforePackage {
		return errors.New("Manager Fetch changed package.json")
	}
	if after, err := digestFile(ctx, lockPath); err != nil || after != beforeLock {
		return errors.New("Manager Fetch changed its lockfile")
	}
	if _, err := os.Stat("/work/pnpmfile-ran"); !os.IsNotExist(err) {
		return errors.New("Manager Fetch executed package-manager configuration")
	}
	install := managerFixtureInstallArguments(descriptor)
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		install = append([]string{descriptor.Entrypoint.Path}, install...)
	}
	if _, err := runConformanceCommandIn(
		ctx,
		managerEnvironment(descriptor),
		project,
		executable,
		install...,
	); err != nil {
		return fmt.Errorf("Manager offline install fixture failed: %w", err)
	}
	if err := rejectDownloadedRuntime(project); err != nil {
		return err
	}
	missing := "/work/cache-miss"
	if err := os.Mkdir(missing, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(
		filepath.Join(missing, "package.json"),
		[]byte(`{"dependencies":{"helmr-cache-miss-fixture":"1.0.0"},"name":"cache-miss","private":true,"version":"1.0.0"}`),
		0600,
	); err != nil {
		return err
	}
	if descriptor.PackageManager.Name == PackageManagerBun {
		negative := managerFixtureNetworkArguments(descriptor)
		if _, err := runConformanceCommandIn(
			ctx,
			managerEnvironment(descriptor),
			missing,
			executable,
			negative...,
		); err == nil {
			return errors.New("Bun network-attempt fixture unexpectedly succeeded")
		}
	} else {
		missingLockName, missingLock := managerFixtureMissingLock(descriptor.PackageManager.Name)
		if err := os.WriteFile(
			filepath.Join(missing, missingLockName),
			[]byte(missingLock),
			0600,
		); err != nil {
			return err
		}
		negative := managerFixtureInstallArguments(descriptor)
		if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
			negative = append([]string{descriptor.Entrypoint.Path}, negative...)
		}
		if _, err := runConformanceCommandIn(
			ctx,
			managerEnvironment(descriptor),
			missing,
			executable,
			negative...,
		); err == nil {
			return errors.New("Manager offline cache-miss fixture unexpectedly succeeded")
		}
	}
	return nil
}

func managerFixtureLock(name PackageManagerName) (string, string) {
	switch name {
	case PackageManagerNPM:
		return "package-lock.json", `{"lockfileVersion":3,"name":"helmr-manager-fixture","packages":{"":{"name":"helmr-manager-fixture","version":"1.0.0"}},"requires":true,"version":"1.0.0"}`
	case PackageManagerPNPM:
		return "pnpm-lock.yaml", "lockfileVersion: '9.0'\n\nimporters:\n  .: {}\n"
	case PackageManagerBun:
		return "bun.lock", `{"lockfileVersion":1,"packages":{},"workspaces":{"":{"name":"helmr-manager-fixture"}}}`
	default:
		return "", ""
	}
}

func managerFixtureMissingLock(name PackageManagerName) (string, string) {
	switch name {
	case PackageManagerNPM:
		return "package-lock.json", `{"lockfileVersion":3,"name":"cache-miss","packages":{"":{"dependencies":{"helmr-cache-miss-fixture":"1.0.0"},"name":"cache-miss","version":"1.0.0"},"node_modules/helmr-cache-miss-fixture":{"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==","resolved":"https://registry.npmjs.org/helmr-cache-miss-fixture/-/helmr-cache-miss-fixture-1.0.0.tgz","version":"1.0.0"}},"requires":true,"version":"1.0.0"}`
	case PackageManagerPNPM:
		return "pnpm-lock.yaml", "lockfileVersion: '9.0'\n\nimporters:\n  .:\n    dependencies:\n      helmr-cache-miss-fixture:\n        specifier: 1.0.0\n        version: 1.0.0\n\npackages:\n  helmr-cache-miss-fixture@1.0.0:\n    resolution: {integrity: sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==}\n\nsnapshots:\n  helmr-cache-miss-fixture@1.0.0: {}\n"
	default:
		return "", ""
	}
}

func managerFixtureFetchArguments(descriptor ManagerArtifactDescriptor) []string {
	switch descriptor.PackageManager.Name {
	case PackageManagerNPM:
		return []string{"ci", "--cache", "/work/cache", "--ignore-scripts", "--no-audit", "--no-fund"}
	case PackageManagerPNPM:
		return []string{
			"fetch",
			"--frozen-lockfile",
			"--ignore-pnpmfile",
			"--ignore-scripts",
			"--no-runtime",
			"--pm-on-fail=error",
			"--store-dir",
			"/work/cache",
		}
	case PackageManagerBun:
		return []string{"install", "--cache-dir", "/work/cache", "--frozen-lockfile", "--ignore-scripts"}
	default:
		return []string{"unsupported"}
	}
}

func managerFixtureInstallArguments(descriptor ManagerArtifactDescriptor) []string {
	switch descriptor.PackageManager.Name {
	case PackageManagerNPM:
		return []string{"ci", "--cache", "/work/cache", "--ignore-scripts", "--no-audit", "--no-fund", "--offline"}
	case PackageManagerPNPM:
		return []string{
			"install",
			"--frozen-lockfile",
			"--no-runtime",
			"--offline",
			"--store-dir",
			"/work/cache",
		}
	case PackageManagerBun:
		return []string{"install", "--cache-dir", "/work/cache", "--frozen-lockfile", "--ignore-scripts"}
	default:
		return []string{"unsupported"}
	}
}

func managerFixtureNetworkArguments(descriptor ManagerArtifactDescriptor) []string {
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		return []string{descriptor.Entrypoint.Path, "install", "--no-audit", "--no-fund"}
	}
	return []string{"install"}
}

func rejectDownloadedRuntime(root string) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		base := strings.ToLower(entry.Name())
		if (base == "node" || strings.HasPrefix(base, "node-v")) &&
			!strings.Contains(name, "node_modules") {
			return fmt.Errorf("Manager fixture downloaded a Runtime at %q", name)
		}
		return nil
	})
}

func toolchainConformance(
	ctx context.Context,
	descriptor ToolchainArtifactDescriptor,
) ([]PlatformConformanceResult, error) {
	source := `
#include <node_api.h>
static napi_value init(napi_env env, napi_value exports) {
  napi_value value;
  if (napi_create_string_utf8(env, "helmr", NAPI_AUTO_LENGTH, &value) != napi_ok) return NULL;
  if (napi_set_named_property(env, exports, "value", value) != napi_ok) return NULL;
  return exports;
}
NAPI_MODULE(NODE_GYP_MODULE_NAME, init)
`
	if err := os.WriteFile("/work/addon.c", []byte(source), 0600); err != nil {
		return nil, err
	}
	cc := "/nix/bin/cc"
	if _, err := runConformanceCommand(
		ctx,
		[]string{"HOME=/work", "PATH=/nix/bin", "TMPDIR=/work/tmp"},
		cc,
		"-shared",
		"-fPIC",
		"-I/nix/include/node",
		"/work/addon.c",
		"-o",
		"/work/addon.node",
	); err != nil {
		return nil, errors.New("toolchain native addon compilation failed")
	}
	node := "/opt/helmr/runtime/bin/node"
	output, err := runConformanceCommand(
		ctx,
		nil,
		node,
		"-e",
		`if (require("/work/addon.node").value !== "helmr") process.exit(1); process.stdout.write(process.versions.modules)`,
	)
	if err != nil || strings.TrimSpace(output) != descriptor.NodeModuleABI {
		return nil, errors.New("toolchain native addon ABI validation failed")
	}
	return passedConformanceResults(
		"native-addon",
		"network-denied",
		"node-headers",
		"runtime-binding",
	), nil
}

func managerEnvironment(descriptor ManagerArtifactDescriptor) []string {
	path := "/opt/helmr/runtime/bin:/opt/helmr/manager/bin"
	return []string{
		"HOME=/work",
		"PATH=" + path,
		"TMPDIR=/work/tmp",
		"npm_config_update_notifier=false",
	}
}

func managerHelpArguments(descriptor ManagerArtifactDescriptor) []string {
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		return []string{descriptor.Entrypoint.Path, "install", "--help"}
	}
	return []string{"install", "--help"}
}

func managerRequiredOptions(name PackageManagerName) []string {
	switch name {
	case PackageManagerNPM:
		return []string{"--ignore-scripts", "--offline"}
	case PackageManagerPNPM:
		return []string{"--frozen-lockfile", "--offline"}
	case PackageManagerBun:
		return []string{"--frozen-lockfile", "--ignore-scripts"}
	default:
		return []string{"unsupported"}
	}
}

func passedConformanceResults(names ...string) []PlatformConformanceResult {
	results := make([]PlatformConformanceResult, len(names))
	for index, name := range names {
		results[index] = PlatformConformanceResult{Name: name, Outcome: "passed"}
	}
	return results
}

func runConformanceCommand(
	ctx context.Context,
	environment []string,
	executable string,
	arguments ...string,
) (string, error) {
	return runConformanceCommandIn(ctx, environment, "/work", executable, arguments...)
}

func runConformanceCommandIn(
	ctx context.Context,
	environment []string,
	directory string,
	executable string,
	arguments ...string,
) (string, error) {
	stdout := &limitedEncoderOutput{remaining: maxEncoderDiagnosticBytes}
	stderr := &limitedEncoderOutput{remaining: maxEncoderDiagnosticBytes}
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = directory
	if environment == nil {
		environment = []string{
			"HOME=/work",
			"PATH=/opt/helmr/runtime/bin",
			"TMPDIR=/work/tmp",
		}
	}
	command.Env = environment
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if stdout.exceeded || stderr.exceeded {
			return "", errors.New("conformance command output is excessive")
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.exceeded || stderr.exceeded {
		return "", errors.New("conformance command output is excessive")
	}
	return stdout.String(), nil
}
