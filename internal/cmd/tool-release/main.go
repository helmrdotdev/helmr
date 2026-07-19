package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

const toolRegistryFile = "tool-registry.json"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("dependency tool candidate command is required")
	}
	switch args[0] {
	case "components":
		return runComponents(args[1:])
	case "registry":
		return runRegistry(args[1:])
	default:
		return fmt.Errorf("unknown dependency tool release command %q", args[0])
	}
}

func runComponents(args []string) error {
	flags := flag.NewFlagSet("components", flag.ContinueOnError)
	input := flags.String("input", "", "raw component document")
	output := flags.String("output", "", "canonical component document")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || *output == "" {
		return errors.New("components requires --input and --output")
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	var components deployment.ToolComponents
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&components); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	canonical, err := deployment.CanonicalToolComponents(components)
	if err != nil {
		return err
	}
	return writeExclusive(*output, canonical)
}

func runRegistry(args []string) error {
	flags := flag.NewFlagSet("registry", flag.ContinueOnError)
	componentsPath := flags.String("components", "", "canonical component document")
	managerPath := flags.String("manager", "", "package manager closure")
	toolchainPath := flags.String("toolchain", "", "standard toolchain closure")
	toolsetPath := flags.String("toolset", "", "composed dependency tools")
	output := flags.String("output", "", "dependency tool candidate directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *componentsPath == "" || *managerPath == "" ||
		*toolchainPath == "" || *toolsetPath == "" || *output == "" {
		return errors.New(
			"registry requires --components, --manager, --toolchain, --toolset, and --output",
		)
	}
	componentsRaw, err := os.ReadFile(*componentsPath)
	if err != nil {
		return err
	}
	components, err := deployment.ParseToolComponents(componentsRaw)
	if err != nil {
		return err
	}
	manager, err := describe(*managerPath, deployment.ManagerComponentMediaType)
	if err != nil {
		return err
	}
	toolchain, err := describe(*toolchainPath, deployment.ToolchainMediaType)
	if err != nil {
		return err
	}
	toolsetArtifact, err := describe(
		*toolsetPath,
		deployment.ManagerDependencyToolsMediaType,
	)
	if err != nil {
		return err
	}
	if manager != components.Manager.ManagerClosure {
		return errors.New("package manager closure does not match the component document")
	}
	if toolchain != components.Toolchain.ToolchainClosure {
		return errors.New("standard toolchain closure does not match the component document")
	}
	managerDigest, err := deployment.ManagerRegistrationDigest(components.Manager)
	if err != nil {
		return err
	}
	toolchainDigest, err := deployment.StandardToolchainDigest(components.Toolchain)
	if err != nil {
		return err
	}
	componentDigest, err := deployment.ComponentManifestDigest(components)
	if err != nil {
		return err
	}
	toolset := deployment.Toolset{
		Architecture:              components.Architecture,
		Artifact:                  toolsetArtifact,
		ComponentManifestDigest:   componentDigest,
		Environment:               append([]deployment.ToolEnvironment(nil), components.Environment...),
		FormatVersion:             deployment.ToolsetFormatVersion,
		ManagedRuntimeDigest:      components.ManagedRuntimeDigest,
		ManagerRegistrationDigest: managerDigest,
		MaterializerVersion:       components.MaterializerVersion,
		PackageManager:            components.PackageManager,
		StandardToolchainDigest:   toolchainDigest,
	}
	registry, err := deployment.CanonicalToolRegistry(
		[]deployment.ManagerRegistration{components.Manager},
		[]deployment.Toolchain{components.Toolchain},
		[]deployment.Toolset{toolset},
	)
	if err != nil {
		return err
	}
	return writeRegistry(
		*output,
		registry,
		map[deployment.ManagerArtifact]string{
			manager:         *managerPath,
			toolchain:       *toolchainPath,
			toolsetArtifact: *toolsetPath,
		},
	)
}

func describe(path, mediaType string) (deployment.ManagerArtifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return deployment.ManagerArtifact{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return deployment.ManagerArtifact{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 {
		return deployment.ManagerArtifact{}, errors.New("dependency tool object is not a non-empty regular file")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return deployment.ManagerArtifact{}, err
	}
	if written != info.Size() {
		return deployment.ManagerArtifact{}, errors.New("dependency tool object changed while hashing")
	}
	after, err := file.Stat()
	if err != nil {
		return deployment.ManagerArtifact{}, err
	}
	if !os.SameFile(info, after) ||
		info.Size() != after.Size() ||
		info.ModTime() != after.ModTime() {
		return deployment.ManagerArtifact{}, errors.New("dependency tool object changed while hashing")
	}
	return deployment.ManagerArtifact{
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		MediaType: mediaType,
		SizeBytes: written,
	}, nil
}

func writeRegistry(
	directory string,
	registry []byte,
	objects map[deployment.ManagerArtifact]string,
) (returnErr error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("dependency tool candidate output is not canonical absolute")
	}
	if _, err := os.Lstat(directory); err == nil {
		return errors.New("dependency tool candidate output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(directory)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(directory)+"-*")
	if err != nil {
		return err
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return errors.Join(err, os.RemoveAll(staging))
	}
	defer func() {
		if staging != "" {
			returnErr = errors.Join(returnErr, os.RemoveAll(staging))
		}
	}()
	objectRoot := filepath.Join(staging, "objects", "sha256")
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		return err
	}
	if err := writeExclusive(
		filepath.Join(staging, toolRegistryFile),
		registry,
	); err != nil {
		return err
	}
	for descriptor, source := range objects {
		name := strings.TrimPrefix(descriptor.Digest, "sha256:")
		if err := copyExclusive(
			filepath.Join(objectRoot, name),
			source,
			descriptor,
		); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, directory); err != nil {
		return err
	}
	staging = ""
	return nil
}

func copyExclusive(
	destination,
	source string,
	descriptor deployment.ManagerArtifact,
) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != descriptor.SizeBytes {
		return errors.New("dependency tool object changed before copying")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, digest), input)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	after, err := input.Stat()
	if err != nil {
		return err
	}
	actualDigest := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if written != descriptor.SizeBytes ||
		actualDigest != descriptor.Digest ||
		!os.SameFile(before, after) ||
		before.Size() != after.Size() ||
		before.ModTime() != after.ModTime() {
		return errors.New("dependency tool object changed while copying")
	}
	return nil
}

func writeExclusive(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains trailing values")
		}
		return err
	}
	return nil
}
