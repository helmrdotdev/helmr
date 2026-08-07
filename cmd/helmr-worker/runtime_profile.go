package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/firecracker"
)

func runRuntimeProfile(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("runtime-profile", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var artifactsDir, artifactsManifest string
	flags.StringVar(&artifactsDir, "runtime-artifacts-dir", "", "directory containing the exact Worker runtime artifacts")
	flags.StringVar(&artifactsManifest, "runtime-artifacts-manifest", "", "canonical runtime artifact manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || (strings.TrimSpace(artifactsDir) == "") == (strings.TrimSpace(artifactsManifest) == "") {
		return errors.New("exactly one of runtime-artifacts-dir or runtime-artifacts-manifest is required")
	}
	var capabilities firecracker.RuntimeCapabilities
	var err error
	if artifactsDir != "" {
		capabilities, err = firecracker.InspectRuntimeArtifacts(artifactsDir)
	} else {
		capabilities, err = firecracker.InspectRuntimeArtifactsManifest(artifactsManifest)
	}
	if err != nil {
		return fmt.Errorf("inspect runtime artifacts: %w", err)
	}
	architecture, err := deployment.RuntimeArchitectureFromGo(capabilities.Arch)
	if err != nil {
		return err
	}
	profile := capacityapi.RuntimeProfile{
		Arch: string(architecture), Contract: capabilities.Contract,
		KernelDigest: capabilities.KernelDigest, InitramfsDigest: capabilities.InitramfsDigest, RootfsDigest: capabilities.RootfsDigest,
	}
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(profile)
}
