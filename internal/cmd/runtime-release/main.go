package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func main() {
	if handled, err := dispatchVerifier(os.Args); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func dispatchVerifier(args []string) (bool, error) {
	return deployment.RunVerifierChild(args)
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("runtime release command is required")
	}
	switch args[0] {
	case "compose":
		return runCompose(ctx, args[1:])
	case "archive":
		return runArchive(ctx, args[1:])
	case "worker":
		return runWorker(ctx, args[1:])
	case "verify-worker":
		return runVerifyWorker(ctx, args[1:])
	case "verify-archive":
		return runVerifyArchive(ctx, args[1:])
	default:
		return fmt.Errorf("unknown runtime release command %q", args[0])
	}
}

func runCompose(ctx context.Context, args []string) (returnErr error) {
	flags := flag.NewFlagSet("compose", flag.ContinueOnError)
	tag := flags.String("tag", "", "exact platform release tag")
	releasesDirectory := flags.String("releases", "", "downloaded release root")
	runtimePath := flags.String("runtime", "", "captured managed runtime")
	invalidPath := flags.String("invalid", "", "managed runtime invalid verifier fixture")
	descriptorPath := flags.String("descriptor", "", "managed runtime descriptor")
	toolchainSource := flags.String(
		"toolchain-source",
		"",
		"captured standard-toolchain source directory",
	)
	scratchDirectory := flags.String("scratch", "", "snapshot scratch directory")
	unitCgroupRoot := flags.String("cgroup-root", "", "delegated verifier cgroup root")
	outputDirectory := flags.String("output", "", "new release directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("compose accepts no positional arguments")
	}
	if *tag == "" || *releasesDirectory == "" ||
		*runtimePath == "" || *invalidPath == "" || *descriptorPath == "" ||
		*toolchainSource == "" ||
		*scratchDirectory == "" || *unitCgroupRoot == "" ||
		*outputDirectory == "" {
		return errors.New(
			"compose requires --tag, --releases, --runtime, --invalid, --descriptor, --toolchain-source, --scratch, --cgroup-root, and --output",
		)
	}
	lineageFile, err := deployment.OpenRuntimeReleaseFile(
		".github/runtime-release.json",
		4096,
	)
	if err != nil {
		return err
	}
	lineageBytes, err := io.ReadAll(lineageFile)
	lineageCloseErr := lineageFile.Close()
	if err := errors.Join(err, lineageCloseErr); err != nil {
		return err
	}
	lineage, err := deployment.ParseRuntimeReleaseLineage(lineageBytes, *tag)
	if err != nil {
		return err
	}
	descriptorFile, err := deployment.OpenRuntimeReleaseFile(*descriptorPath, 4096)
	if err != nil {
		return err
	}
	descriptorBytes, err := io.ReadAll(descriptorFile)
	closeErr := descriptorFile.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return err
	}
	descriptor, err := deployment.ParseRuntimeDescriptor(descriptorBytes)
	if err != nil {
		return err
	}
	runtimeFile, err := deployment.OpenRuntimeReleaseFile(
		*runtimePath,
		descriptor.SizeBytes,
	)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, runtimeFile.Close())
	}()
	invalidFile, err := deployment.OpenRuntimeReleaseFile(
		*invalidPath,
		4096,
	)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, invalidFile.Close())
	}()
	var predecessor *deployment.RuntimeReleasePredecessor
	if lineage.Predecessor != nil {
		predecessor, err = deployment.OpenRuntimeReleaseArchive(
			ctx,
			filepath.Join(
				canonicalAbsolute(*releasesDirectory),
				lineage.Predecessor.Release,
				"runtime-release.tar",
			),
			*lineage.Predecessor,
			canonicalAbsolute(*scratchDirectory),
		)
		if err != nil {
			return err
		}
		defer func() {
			returnErr = errors.Join(returnErr, predecessor.Close())
		}()
	}
	release, err := deployment.PrepareRuntimeRelease(ctx, deployment.RuntimeReleaseSource{
		Runtime:                  runtimeFile,
		Invalid:                  invalidFile,
		Descriptor:               descriptor,
		ScratchDirectory:         canonicalAbsolute(*scratchDirectory),
		UnitCgroupRoot:           canonicalAbsolute(*unitCgroupRoot),
		Lineage:                  lineage,
		Predecessor:              predecessor,
		ToolchainSourceDirectory: canonicalAbsolute(*toolchainSource),
	})
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, release.Close())
	}()
	return deployment.WriteRuntimeReleaseDirectory(
		ctx,
		release,
		canonicalAbsolute(*outputDirectory),
	)
}

func runWorker(ctx context.Context, args []string) (returnErr error) {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	tag := flags.String("tag", "", "exact platform release tag")
	inputDirectory := flags.String("input", "", "signed runtime release directory")
	architectureValue := flags.String("architecture", "", "runtime architecture")
	scratchDirectory := flags.String("scratch", "", "snapshot scratch directory")
	unitCgroupRoot := flags.String("cgroup-root", "", "delegated verifier cgroup root")
	outputPath := flags.String("output", "", "worker package tar")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("worker accepts no positional arguments")
	}
	if *tag == "" || *inputDirectory == "" || *architectureValue == "" ||
		*scratchDirectory == "" || *unitCgroupRoot == "" ||
		*outputPath == "" {
		return errors.New(
			"worker requires --tag, --input, --architecture, --scratch, --cgroup-root, and --output",
		)
	}
	architecture := deployment.RuntimeArchitecture(*architectureValue)
	if err := deployment.ValidateRuntimeArchitecture(architecture); err != nil {
		return err
	}
	release, bundle, trustedRoot, err := deployment.LoadRuntimeReleaseDirectory(
		ctx,
		canonicalAbsolute(*inputDirectory),
		canonicalAbsolute(*scratchDirectory),
		canonicalAbsolute(*unitCgroupRoot),
		*tag,
		architecture,
	)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, release.Close())
	}()
	output, err := os.OpenFile(
		canonicalAbsolute(*outputPath),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return err
	}
	removeOutput := true
	defer func() {
		if output != nil {
			returnErr = errors.Join(returnErr, output.Close())
		}
		if removeOutput {
			returnErr = errors.Join(
				returnErr,
				os.Remove(canonicalAbsolute(*outputPath)),
			)
		}
	}()
	result, err := deployment.WriteRuntimeWorkerPackage(
		ctx,
		release,
		bundle,
		trustedRoot,
		canonicalAbsolute(*scratchDirectory),
		output,
	)
	if err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Chmod(0o444); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	output = nil
	removeOutput = false
	return printResult(result)
}

func runArchive(ctx context.Context, args []string) (returnErr error) {
	flags := flag.NewFlagSet("archive", flag.ContinueOnError)
	tag := flags.String("tag", "", "exact platform release tag")
	inputDirectory := flags.String("input", "", "signed runtime release directory")
	scratchDirectory := flags.String("scratch", "", "snapshot scratch directory")
	unitCgroupRoot := flags.String("cgroup-root", "", "delegated verifier cgroup root")
	outputPath := flags.String("output", "", "complete runtime release archive")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *tag == "" || *inputDirectory == "" ||
		*scratchDirectory == "" || *unitCgroupRoot == "" || *outputPath == "" {
		return errors.New(
			"archive requires --tag, --input, --scratch, --cgroup-root, and --output",
		)
	}
	release, bundle, trustedRoot, err := deployment.LoadRuntimeReleaseDirectory(
		ctx,
		canonicalAbsolute(*inputDirectory),
		canonicalAbsolute(*scratchDirectory),
		canonicalAbsolute(*unitCgroupRoot),
		*tag,
		deployment.ArchitectureX8664,
	)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, release.Close())
	}()
	output, err := os.OpenFile(
		canonicalAbsolute(*outputPath),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if output != nil {
			returnErr = errors.Join(returnErr, output.Close())
		}
		if remove {
			returnErr = errors.Join(returnErr, os.Remove(canonicalAbsolute(*outputPath)))
		}
	}()
	result, err := deployment.WriteRuntimeReleaseArchive(
		ctx,
		release,
		bundle,
		trustedRoot,
		canonicalAbsolute(*scratchDirectory),
		output,
	)
	if err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Chmod(0o444); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	output = nil
	remove = false
	return printResult(result)
}

func runVerifyWorker(ctx context.Context, args []string) (returnErr error) {
	flags := flag.NewFlagSet("verify-worker", flag.ContinueOnError)
	tag := flags.String("tag", "", "exact platform release tag")
	inputPath := flags.String("input", "", "worker package tar")
	architectureValue := flags.String("architecture", "", "runtime architecture")
	scratchDirectory := flags.String("scratch", "", "snapshot scratch directory")
	unitCgroupRoot := flags.String("cgroup-root", "", "delegated verifier cgroup root")
	trustedRootPath := flags.String(
		"trusted-root",
		"",
		"tag-pinned trusted root",
	)
	outputDirectory := flags.String(
		"output",
		"",
		"optional verified six-member directory",
	)
	snapshotPath := flags.String(
		"snapshot",
		"",
		"optional immutable verified package snapshot",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("verify-worker accepts no positional arguments")
	}
	if *tag == "" || *inputPath == "" || *architectureValue == "" ||
		*scratchDirectory == "" || *unitCgroupRoot == "" ||
		*trustedRootPath == "" {
		return errors.New(
			"verify-worker requires --tag, --input, --architecture, --scratch, --cgroup-root, and --trusted-root",
		)
	}
	architecture := deployment.RuntimeArchitecture(*architectureValue)
	if err := deployment.ValidateRuntimeArchitecture(architecture); err != nil {
		return err
	}
	lineage, err := readLineage(*tag)
	if err != nil {
		return err
	}
	trustedRoot, err := readTrustedRoot(*trustedRootPath)
	if err != nil {
		return err
	}
	input, err := os.Open(canonicalAbsolute(*inputPath))
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, input.Close())
	}()
	materialize := ""
	if *outputDirectory != "" {
		materialize = canonicalAbsolute(*outputDirectory)
	}
	result, err := deployment.VerifyRuntimeWorkerPackage(
		ctx,
		input,
		architecture,
		lineage,
		trustedRoot,
		canonicalAbsolute(*unitCgroupRoot),
		canonicalAbsolute(*scratchDirectory),
		materialize,
		canonicalOptional(*snapshotPath),
	)
	if err != nil {
		return err
	}
	return printResult(result)
}

func runVerifyArchive(ctx context.Context, args []string) (returnErr error) {
	flags := flag.NewFlagSet("verify-archive", flag.ContinueOnError)
	tag := flags.String("tag", "", "exact platform release tag")
	inputPath := flags.String("input", "", "complete runtime release archive")
	scratchDirectory := flags.String("scratch", "", "snapshot scratch directory")
	unitCgroupRoot := flags.String("cgroup-root", "", "delegated verifier cgroup root")
	trustedRootPath := flags.String(
		"trusted-root",
		"",
		"tag-pinned trusted root",
	)
	outputDirectory := flags.String("output", "", "optional verified release directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *tag == "" || *inputPath == "" ||
		*scratchDirectory == "" || *unitCgroupRoot == "" ||
		*trustedRootPath == "" {
		return errors.New(
			"verify-archive requires --tag, --input, --scratch, --cgroup-root, and --trusted-root",
		)
	}
	lineage, err := readLineage(*tag)
	if err != nil {
		return err
	}
	trustedRoot, err := readTrustedRoot(*trustedRootPath)
	if err != nil {
		return err
	}
	input, err := os.Open(canonicalAbsolute(*inputPath))
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, input.Close())
	}()
	output := ""
	if *outputDirectory != "" {
		output = canonicalAbsolute(*outputDirectory)
	}
	result, err := deployment.VerifyRuntimeReleaseArchive(
		ctx,
		input,
		lineage,
		trustedRoot,
		canonicalAbsolute(*scratchDirectory),
		canonicalAbsolute(*unitCgroupRoot),
		output,
	)
	if err != nil {
		return err
	}
	return printResult(result)
}

func readTrustedRoot(path string) ([]byte, error) {
	file, err := deployment.OpenRuntimeReleaseFile(
		canonicalAbsolute(path),
		1<<20,
	)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	return raw, nil
}

func canonicalOptional(path string) string {
	if path == "" {
		return ""
	}
	return canonicalAbsolute(path)
}

func readLineage(tag string) (deployment.RuntimeReleaseLineage, error) {
	file, err := deployment.OpenRuntimeReleaseFile(
		".github/runtime-release.json",
		4096,
	)
	if err != nil {
		return deployment.RuntimeReleaseLineage{}, err
	}
	raw, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return deployment.RuntimeReleaseLineage{}, err
	}
	return deployment.ParseRuntimeReleaseLineage(raw, tag)
}

func canonicalAbsolute(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return filepath.Clean(absolute)
}

func printResult(result deployment.RuntimeReleasePackage) error {
	return json.NewEncoder(os.Stdout).Encode(struct {
		Digest    string `json:"digest"`
		SizeBytes int64  `json:"sizeBytes"`
	}{
		Digest:    result.Digest,
		SizeBytes: result.SizeBytes,
	})
}
