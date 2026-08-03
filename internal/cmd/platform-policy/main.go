package main

import (
	"bytes"
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("platform-policy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runtimePath := flags.String("runtime", "", "Runtime harness descriptor")
	toolchainPath := flags.String("toolchain", "", "toolchain base descriptor")
	keyringPath := flags.String("node-keyring", "", "Node release keyring")
	fingerprintsPath := flags.String("node-fingerprints", "", "Node release key fingerprints")
	outputPath := flags.String("output", "", "canonical build policy output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*runtimePath == "" ||
		*toolchainPath == "" ||
		*keyringPath == "" ||
		*fingerprintsPath == "" ||
		*outputPath == "" {
		return errors.New("all Platform policy inputs and --output are required")
	}
	var runtimeHarness deployment.ArtifactDescriptor
	if err := decodeFile(*runtimePath, &runtimeHarness); err != nil {
		return fmt.Errorf("Runtime harness descriptor: %w", err)
	}
	var toolchain deployment.ToolchainInputs
	if err := decodeFile(*toolchainPath, &toolchain); err != nil {
		return fmt.Errorf("toolchain base descriptor: %w", err)
	}
	keyring, err := os.ReadFile(*keyringPath)
	if err != nil {
		return err
	}
	fingerprintsRaw, err := os.ReadFile(*fingerprintsPath)
	if err != nil {
		return err
	}
	fingerprints := strings.Fields(string(fingerprintsRaw))
	policy, err := deployment.ComposeBuildPolicy(
		deployment.RuntimeInputs{Harness: runtimeHarness},
		toolchain,
		keyring,
		fingerprints,
	)
	if err != nil {
		return err
	}
	parent := filepath.Dir(*outputPath)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(*outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
	if err != nil {
		return err
	}
	if _, err := file.Write(policy); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func decodeFile(path string, destination any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("descriptor has trailing JSON")
		}
		return err
	}
	return nil
}
