package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const toolchainFile = "toolchain.json"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("standard-toolchain release command is required")
	}
	switch args[0] {
	case "candidate":
		return runCandidate(args[1:])
	default:
		return fmt.Errorf("unknown standard-toolchain release command %q", args[0])
	}
}

func runCandidate(args []string) error {
	flags := flag.NewFlagSet("candidate", flag.ContinueOnError)
	input := flags.String("input", "", "raw standard-toolchain descriptor")
	closure := flags.String("closure", "", "standard-toolchain closure")
	output := flags.String("output", "", "standard-toolchain candidate directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || *closure == "" || *output == "" {
		return errors.New("candidate requires --input, --closure, and --output")
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return err
	}
	toolchain, err := deployment.ParseToolchain(canonical)
	if err != nil {
		return err
	}
	artifact, err := describe(*closure, deployment.ToolchainMediaType)
	if err != nil {
		return err
	}
	if artifact != toolchain.ToolchainClosure {
		return errors.New(
			"standard-toolchain closure does not match the descriptor",
		)
	}
	return writeCandidate(*output, canonical, artifact, *closure)
}

func describe(path, mediaType string) (deployment.ArtifactDescriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return deployment.ArtifactDescriptor{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return deployment.ArtifactDescriptor{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 {
		return deployment.ArtifactDescriptor{}, errors.New(
			"standard-toolchain object is not a non-empty regular file",
		)
	}
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return deployment.ArtifactDescriptor{}, err
	}
	if written != info.Size() {
		return deployment.ArtifactDescriptor{}, errors.New(
			"standard-toolchain object changed while hashing",
		)
	}
	after, err := file.Stat()
	if err != nil {
		return deployment.ArtifactDescriptor{}, err
	}
	if !os.SameFile(info, after) ||
		info.Size() != after.Size() ||
		info.ModTime() != after.ModTime() {
		return deployment.ArtifactDescriptor{}, errors.New(
			"standard-toolchain object changed while hashing",
		)
	}
	return deployment.ArtifactDescriptor{
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		MediaType: mediaType,
		SizeBytes: written,
	}, nil
}

func writeCandidate(
	directory string,
	toolchain []byte,
	descriptor deployment.ArtifactDescriptor,
	source string,
) (returnErr error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New(
			"standard-toolchain candidate output is not canonical absolute",
		)
	}
	if _, err := os.Lstat(directory); err == nil {
		return errors.New("standard-toolchain candidate output already exists")
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
	if err := writeExclusive(filepath.Join(staging, toolchainFile), toolchain); err != nil {
		return err
	}
	name := strings.TrimPrefix(descriptor.Digest, "sha256:")
	if err := copyExclusive(
		filepath.Join(objectRoot, name),
		source,
		descriptor,
	); err != nil {
		return err
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
	descriptor deployment.ArtifactDescriptor,
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
		return errors.New("standard-toolchain object changed before copying")
	}
	output, err := os.OpenFile(
		destination,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o444,
	)
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
		return errors.New("standard-toolchain object changed while copying")
	}
	return nil
}

func writeExclusive(path string, raw []byte) error {
	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o444,
	)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
