//go:build linux

package deployment

import (
	"context"
	"encoding/binary"
	"encoding/json"
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
		ConformanceSet: "pending",
		FormatVersion:  PlatformArtifactDocumentFormatVersion,
		Inputs:         []PlatformEvidenceFile{},
		Results:        results,
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
	launch := append([]string(nil), descriptor.ProgramNodeFlags...)
	if _, err := runConformanceCommand(ctx, nil, node, append(launch, "-e", "0")...); err != nil {
		return nil, errors.New("Runtime Node does not accept its Program launch flags")
	}
	program := []byte(`const value: string = "helmr"; if (value !== "helmr") process.exit(1);`)
	if err := os.WriteFile("/work/program.ts", program, 0600); err != nil {
		return nil, err
	}
	if _, err := runConformanceCommand(
		ctx,
		nil,
		node,
		append(launch, "/work/program.ts")...,
	); err == nil {
		return nil, errors.New("Runtime Program mode accepted TypeScript")
	}
	if err := os.WriteFile(
		"/work/source-map.mjs",
		[]byte("throw new Error('source-map-fixture')\n//# sourceMappingURL=source-map.mjs.map\n"),
		0600,
	); err != nil {
		return nil, err
	}
	if err := os.WriteFile(
		"/work/source-map.mjs.map",
		[]byte(`{"file":"source-map.mjs","mappings":"AAAA","names":[],"sources":["file:///work/source-map.ts"],"version":3}`),
		0600,
	); err != nil {
		return nil, err
	}
	if _, err := runConformanceCommand(
		ctx,
		nil,
		node,
		append(launch, "/work/source-map.mjs")...,
	); err == nil || !strings.Contains(err.Error(), "source-map.ts:1") {
		return nil, errors.New("Runtime Program mode did not apply source maps")
	}
	if _, err := runConformanceCommand(ctx, nil, node, "--check", descriptor.Entrypoint); err != nil {
		return nil, errors.New("Runtime entrypoint is not valid JavaScript")
	}
	return passedConformanceResults(
		"network-denied",
		"node-architecture",
		"node-disable-types",
		"node-module-abi",
		"node-reported-version",
		"node-source-maps",
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
	if descriptor.PackageManager.Name == PackageManagerPNPM {
		if err := pnpmReplacementConformance(ctx, descriptor, executable); err != nil {
			return nil, err
		}
	}
	return passedConformanceResults(
		managerConformanceNames(descriptor.PackageManager.Name)...,
	), nil
}

func pnpmReplacementConformance(
	ctx context.Context,
	descriptor ManagerArtifactDescriptor,
	executable string,
) error {
	if err := pnpmRuntimeReplacementConformance(ctx, descriptor, executable); err != nil {
		return err
	}
	return pnpmManagerReplacementConformance(ctx, descriptor, executable)
}

func pnpmRuntimeReplacementConformance(
	ctx context.Context,
	descriptor ManagerArtifactDescriptor,
	executable string,
) error {
	project := "/work/pnpm-runtime"
	if err := os.Mkdir(project, 0700); err != nil {
		return err
	}
	packageJSON := `{"devEngines":{"runtime":{"name":"node","onFail":"download","version":"24.0.0"}},"name":"helmr-pnpm-runtime-conformance","private":true,"version":"1.0.0"}`
	if err := os.WriteFile(
		filepath.Join(project, "package.json"),
		[]byte(packageJSON),
		0600,
	); err != nil {
		return err
	}
	lock := "lockfileVersion: '9.0'\n\nimporters:\n  .:\n    devDependencies:\n      node:\n        specifier: runtime:24.0.0\n        version: runtime:24.0.0\n"
	if err := os.WriteFile(
		filepath.Join(project, "pnpm-lock.yaml"),
		[]byte(lock),
		0600,
	); err != nil {
		return err
	}
	arguments := pnpmInstallArguments("error")
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
		return fmt.Errorf("pnpm runtime replacement fixture failed: %w", err)
	}
	nodeBin := filepath.Join(project, "node_modules", ".bin", "node")
	if _, err := os.Lstat(nodeBin); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("pnpm installed a replacement Node runtime")
		}
		return err
	}
	return nil
}

func pnpmManagerReplacementConformance(
	ctx context.Context,
	descriptor ManagerArtifactDescriptor,
	executable string,
) error {
	project := "/work/pnpm-manager"
	if err := os.Mkdir(project, 0700); err != nil {
		return err
	}
	packageJSON := `{"devEngines":{"packageManager":{"name":"pnpm","onFail":"download","version":"0.0.1"}},"name":"helmr-pnpm-manager-conformance","private":true,"version":"1.0.0"}`
	if err := os.WriteFile(
		filepath.Join(project, "package.json"),
		[]byte(packageJSON),
		0600,
	); err != nil {
		return err
	}
	if err := os.WriteFile(
		filepath.Join(project, "pnpm-lock.yaml"),
		[]byte("lockfileVersion: '9.0'\n\nimporters:\n  .: {}\n"),
		0600,
	); err != nil {
		return err
	}
	arguments := pnpmInstallArguments("ignore")
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
		return fmt.Errorf("pnpm Manager replacement control fixture failed: %w", err)
	}
	arguments = pnpmInstallArguments("error")
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		arguments = append([]string{descriptor.Entrypoint.Path}, arguments...)
	}
	if _, err := runConformanceCommandIn(
		ctx,
		managerEnvironment(descriptor),
		project,
		executable,
		arguments...,
	); err == nil {
		return errors.New("pnpm launched or accepted a replacement Manager")
	} else {
		var exitErr *exec.ExitError
		diagnostic := err.Error()
		if !errors.As(err, &exitErr) ||
			exitErr.ExitCode() != 1 ||
			!strings.Contains(
				diagnostic,
				"configured to use 0.0.1 of pnpm",
			) ||
			!strings.Contains(
				diagnostic,
				"current pnpm is v"+descriptor.PackageManager.Version,
			) {
			return errors.New(
				"pnpm did not report a local Manager version rejection",
			)
		}
	}
	entries, err := os.ReadDir("/work/pnpm-home")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(entries) != 0 {
		return errors.New("pnpm created a replacement Manager artifact")
	}
	return nil
}

func pnpmInstallArguments(managerFailure string) []string {
	return []string{
		"install",
		"--frozen-lockfile",
		"--no-runtime",
		"--pm-on-fail=" + managerFailure,
	}
}

func toolchainConformance(
	ctx context.Context,
	descriptor ToolchainArtifactDescriptor,
) ([]PlatformConformanceResult, error) {
	if err := compilerConformance(ctx, descriptor); err != nil {
		return nil, err
	}
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
		"compiler-aggregate",
		"compiler-config",
		"compiler-final-modules",
		"compiler-options",
		"esbuild-api",
		"esbuild-binary",
		"native-addon",
		"network-denied",
		"node-headers",
		"runtime-binding",
	), nil
}

func compilerConformance(
	ctx context.Context,
	descriptor ToolchainArtifactDescriptor,
) error {
	version, err := runConformanceCommand(
		ctx,
		nil,
		descriptor.Compiler.Esbuild.BinaryPath,
		"--version",
	)
	if err != nil || strings.TrimSpace(version) != descriptor.Compiler.Esbuild.Version {
		return errors.New("toolchain esbuild binary reported the wrong version")
	}
	node := "/opt/helmr/runtime/bin/node"
	description, err := runConformanceCommand(
		ctx,
		nil,
		node,
		descriptor.Compiler.ProgramCompiler.Entrypoint,
		"--describe",
	)
	if err != nil {
		return errors.New("Program Compiler descriptor failed")
	}
	var contract struct {
		APIVersion            string `json:"apiVersion"`
		EsbuildVersion        string `json:"esbuildVersion"`
		OptionsContractDigest string `json:"optionsContractDigest"`
	}
	if err := json.Unmarshal([]byte(description), &contract); err != nil ||
		contract.APIVersion != descriptor.Compiler.APIVersion ||
		contract.EsbuildVersion != descriptor.Compiler.Esbuild.Version ||
		contract.OptionsContractDigest != descriptor.Compiler.OptionsContractDigest {
		return errors.New("Program Compiler descriptor does not match authority")
	}
	project := "/opt/helmr/program"
	for _, directory := range []string{
		filepath.Join(project, "node_modules", "@helmr", "sdk"),
		filepath.Join(project, "tasks"),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	files := map[string]string{
		"helmr.config.ts":                      `import { defineConfig } from "@helmr/sdk"; export default defineConfig({ dirs: ["tasks"] });`,
		"node_modules/@helmr/sdk/package.json": `{"exports":"./index.mjs","name":"@helmr/sdk","type":"module"}`,
		"node_modules/@helmr/sdk/index.mjs":    `const brand=Symbol.for("helmr.sdk.v0.definition");export const defineConfig=(value)=>value;export const task=(value)=>({[brand]:{kind:"task",id:value.id,hasPayload:false,handler:value.run}});`,
		"tasks/example.ts":                     `import { task } from "@helmr/sdk"; export const example=task({id:"example",run:()=>null});`,
	}
	for path, contents := range files {
		if err := os.WriteFile(filepath.Join(project, path), []byte(contents), 0600); err != nil {
			return err
		}
	}
	output := "/work/compiler-output"
	if err := os.Mkdir(output, 0700); err != nil {
		return err
	}
	flags, err := NodeProgramFlags(descriptor.NodeVersion)
	if err != nil {
		return err
	}
	configCommand := fmt.Sprintf(
		`%s %s %s %s %s %s npm 3>/work/config.frame`,
		node,
		strings.Join(flags, " "),
		descriptor.Compiler.ConfigEvaluator.Entrypoint,
		project,
		descriptor.NodeVersion,
		output,
	)
	if _, err := runConformanceCommand(
		ctx,
		[]string{"HOME=/work", "PATH=/nix/bin:/opt/helmr/runtime/bin", "TMPDIR=/work"},
		"/nix/bin/bash",
		"-c",
		configCommand,
	); err != nil {
		return errors.New("Config Evaluator fixture failed")
	}
	frame, err := os.ReadFile("/work/config.frame")
	if err != nil || len(frame) < 4 || int(binary.BigEndian.Uint32(frame[:4])) != len(frame)-4 {
		return errors.New("Config Evaluator fixture returned an invalid frame")
	}
	if err := os.WriteFile("/work/config.json", frame[4:], 0600); err != nil {
		return err
	}
	programCommand := fmt.Sprintf(
		`%s %s %s %s /work/config.json %s %s npm 3>/work/program.frame`,
		node,
		strings.Join(flags, " "),
		descriptor.Compiler.ProgramCompiler.Entrypoint,
		project,
		descriptor.NodeVersion,
		output,
	)
	if _, err := runConformanceCommand(
		ctx,
		[]string{"HOME=/work", "PATH=/nix/bin:/opt/helmr/runtime/bin", "TMPDIR=/work"},
		"/nix/bin/bash",
		"-c",
		programCommand,
	); err != nil {
		return errors.New("Program Compiler fixture failed")
	}
	programFrame, err := os.ReadFile("/work/program.frame")
	if err != nil || len(programFrame) < 4 ||
		int(binary.BigEndian.Uint32(programFrame[:4])) != len(programFrame)-4 {
		return errors.New("Program Compiler fixture returned an invalid frame")
	}
	for _, path := range []string{
		"helmr/compiler-result.json",
		"helmr/config.json",
	} {
		if info, err := os.Stat(filepath.Join(output, path)); err != nil ||
			!info.Mode().IsRegular() {
			return fmt.Errorf("Program Compiler output %q is missing", path)
		}
	}
	matches, err := filepath.Glob(
		filepath.Join(output, "tasks", ".helmr", "modules", "*.mjs"),
	)
	if err != nil || len(matches) != 1 {
		return errors.New("Program Compiler did not emit one independent final module")
	}
	chunks, err := filepath.Glob(
		filepath.Join(output, "tasks", ".helmr", "modules", "*chunk*"),
	)
	if err != nil || len(chunks) != 0 {
		return errors.New("Program Compiler emitted a shared chunk")
	}
	return nil
}

func managerEnvironment(descriptor ManagerArtifactDescriptor) []string {
	path := "/opt/helmr/runtime/bin:/opt/helmr/manager/bin"
	environment := []string{
		"HOME=/work",
		"PATH=" + path,
		"TMPDIR=/work/tmp",
		"npm_config_update_notifier=false",
	}
	if descriptor.PackageManager.Name == PackageManagerPNPM {
		environment = append(environment, "PNPM_HOME=/work/pnpm-home")
	}
	return environment
}

func managerHelpArguments(descriptor ManagerArtifactDescriptor) []string {
	if descriptor.Entrypoint.Kind == ManagerEntrypointNode {
		command := "install"
		if descriptor.PackageManager.Name == PackageManagerNPM {
			command = "ci"
		}
		return []string{descriptor.Entrypoint.Path, command, "--help"}
	}
	return []string{"install", "--help"}
}

func managerRequiredOptions(name PackageManagerName) []string {
	switch name {
	case PackageManagerNPM:
		return []string{"--no-audit", "--no-fund"}
	case PackageManagerPNPM:
		return []string{"--frozen-lockfile", "--no-runtime", "--pm-on-fail"}
	case PackageManagerBun:
		return []string{"--frozen-lockfile"}
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
