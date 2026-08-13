package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/spf13/cobra"
)

// deploymentBundleBuilderImage is injected by Product release automation only
// after the canonical image has been published by digest.
var deploymentBundleBuilderImage string
var buildArchiveTempDir string

var runDockerBuildx = executeDockerBuildx

func bundleBuildCommand() *cobra.Command {
	var output string
	var installCommand string
	command := &cobra.Command{
		Use:   "build [path]",
		Short: "Build a verified deployment bundle locally.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			source := "."
			if len(arguments) == 1 {
				source = arguments[0]
			}
			return buildDeploymentBundle(
				command.Context(),
				command,
				source,
				output,
				installCommand,
			)
		},
	}
	command.Flags().StringVarP(
		&output,
		"output",
		"o",
		".helmr/deployment-bundle",
		"New deployment bundle directory.",
	)
	command.Flags().StringVar(
		&installCommand,
		"install-command",
		"",
		"Custom dependency installation/preparation command inside BuildKit.",
	)
	return command
}

func buildDeploymentBundle(
	ctx context.Context,
	command *cobra.Command,
	source string,
	output string,
	installCommand string,
) (returnErr error) {
	if !strings.Contains(deploymentBundleBuilderImage, "@sha256:") {
		return errors.New("this Helmr release does not contain a canonical bundle builder image")
	}
	root, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("build path must be a directory: %s", source)
	}
	destination, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("bundle output directory already exists")
		}
		return err
	}

	tree, cleanupTree, err := archive.CreateTarWithOptionsContext(
		ctx,
		root,
		buildArchiveTempDir,
		archive.TarOptions{CanonicalSource: true},
	)
	if err != nil {
		return err
	}
	defer cleanupTree()
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".helmr-bundle-build-")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(stage)) }()
	contextDirectory := filepath.Join(stage, "context")
	archiveFile, err := os.Open(tree.Path)
	if err != nil {
		return err
	}
	extractErr := archive.ExtractTar(archiveFile, contextDirectory)
	closeErr := archiveFile.Close()
	if err := errors.Join(extractErr, closeErr); err != nil {
		return fmt.Errorf("materialize canonical build context: %w", err)
	}
	install, err := builder.SelectInstallPlan(contextDirectory, installCommand)
	if err != nil {
		return err
	}
	dockerfile, err := builder.Dockerfile(deploymentBundleBuilderImage, install)
	if err != nil {
		return err
	}
	dockerfilePath := filepath.Join(stage, "Dockerfile.helmr")
	if err := os.WriteFile(dockerfilePath, dockerfile, 0o600); err != nil {
		return err
	}
	buildOutput := filepath.Join(stage, "bundle")
	if err := runDockerBuildx(
		ctx,
		command,
		dockerfilePath,
		contextDirectory,
		buildOutput,
	); err != nil {
		return err
	}
	if err := builder.PublishBundleDirectory(buildOutput, destination); err != nil {
		return err
	}
	_, err = fmt.Fprintln(command.OutOrStdout(), destination)
	return err
}

func executeDockerBuildx(
	ctx context.Context,
	command *cobra.Command,
	dockerfile string,
	contextDirectory string,
	outputDirectory string,
) error {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("helmr build requires Docker Buildx")
	}
	process := exec.CommandContext(
		ctx,
		docker,
		"buildx",
		"build",
		"--platform", "linux/amd64",
		"--file", dockerfile,
		"--target", "bundle",
		"--output", "type=local,dest="+outputDirectory,
		"--progress", "plain",
		contextDirectory,
	)
	process.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	process.Stdin = command.InOrStdin()
	process.Stdout = command.ErrOrStderr()
	process.Stderr = command.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("build deployment bundle with Docker Buildx: %w", err)
	}
	if _, err := deployment.ReadDeploymentBundleDirectory(outputDirectory); err != nil {
		return fmt.Errorf("verify Docker Buildx output: %w", err)
	}
	return nil
}
