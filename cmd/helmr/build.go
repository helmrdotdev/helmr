package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

type dockerBuildxRequest struct {
	Dockerfile       string
	ContextDirectory string
	Target           string
	Output           string
	OutputType       string
	OutputAttributes map[string]string
	BuildContexts    map[string]string
	SecretIDs        []string
}

func bundleBuildCommand() *cobra.Command {
	var output string
	var installCommand string
	var secretIDs []string
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
				secretIDs,
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
	command.Flags().StringSliceVar(&secretIDs, "build-secret", nil, "Environment variable to mount as /run/secrets/NAME during dependency installation (repeatable).")
	return command
}

func buildDeploymentBundle(
	ctx context.Context,
	command *cobra.Command,
	source string,
	output string,
	installCommand string,
	secretIDs []string,
) error {
	return buildDeploymentBundleAt(ctx, command, source, output, installCommand, secretIDs, true)
}

func buildDeploymentBundleAt(
	ctx context.Context,
	command *cobra.Command,
	source string,
	output string,
	installCommand string,
	secretIDs []string,
	printOutput bool,
) (returnErr error) {
	if err := builder.ValidateBuilderImage(deploymentBundleBuilderImage); err != nil {
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
	install.SecretIDs, err = builder.NormalizeSecretIDs(secretIDs)
	if err != nil {
		return err
	}
	emptyContext := filepath.Join(stage, "empty-context")
	if err := os.Mkdir(emptyContext, 0o755); err != nil {
		return err
	}
	installedDockerfile, err := builder.InstalledDockerfile(deploymentBundleBuilderImage, install)
	if err != nil {
		return err
	}
	installedDockerfilePath := filepath.Join(stage, "Dockerfile.installed")
	if err := os.WriteFile(installedDockerfilePath, installedDockerfile, 0o600); err != nil {
		return err
	}
	installedLayout := filepath.Join(stage, "installed-layout")
	if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
		Dockerfile: installedDockerfilePath, ContextDirectory: contextDirectory,
		Target: "installed-tree", Output: installedLayout, OutputType: "oci",
		OutputAttributes: map[string]string{"rewrite-timestamp": "true", "tar": "false"},
		SecretIDs:        install.SecretIDs,
	}); err != nil {
		return fmt.Errorf("install project dependencies: %w", err)
	}
	installedContext, err := builder.InstalledLayoutContext(installedLayout)
	if err != nil {
		return fmt.Errorf("validate installed project tree: %w", err)
	}
	projectContexts := map[string]string{"helmr_installed": installedContext}
	analysisDockerfile, err := builder.AnalysisDockerfile(deploymentBundleBuilderImage)
	if err != nil {
		return err
	}
	analysisDockerfilePath := filepath.Join(stage, "Dockerfile.analysis")
	if err := os.WriteFile(analysisDockerfilePath, analysisDockerfile, 0o600); err != nil {
		return err
	}
	analysisOutput := filepath.Join(stage, "analysis")
	if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
		Dockerfile: analysisDockerfilePath, ContextDirectory: emptyContext,
		Target: "analysis", Output: analysisOutput, OutputType: "local",
		BuildContexts: projectContexts,
	}); err != nil {
		return err
	}
	planRaw, err := os.ReadFile(filepath.Join(analysisOutput, "build-plan.json"))
	if err != nil {
		return fmt.Errorf("read analyzed build plan: %w", err)
	}
	plan, err := deployment.ParseBuildPlan(planRaw)
	if err != nil {
		return fmt.Errorf("verify analyzed build plan: %w", err)
	}
	workspaceBuilds, err := builder.WorkspaceBuilds(plan)
	if err != nil {
		return err
	}
	workspaceContext := filepath.Join(stage, "workspace-images")
	if err := os.Mkdir(workspaceContext, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspaceContext, "build-plan.json"), planRaw, 0o644); err != nil {
		return err
	}
	workspaceInputs, err := buildWorkspaceImages(
		ctx,
		command,
		stage,
		workspaceContext,
		emptyContext,
		projectContexts,
		workspaceBuilds,
	)
	if err != nil {
		return err
	}
	workspaceRaw, err := json.Marshal(workspaceInputs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspaceContext, "images.json"), workspaceRaw, 0o644); err != nil {
		return err
	}
	finalDockerfile, err := builder.Dockerfile(deploymentBundleBuilderImage)
	if err != nil {
		return err
	}
	finalDockerfilePath := filepath.Join(stage, "Dockerfile.final")
	if err := os.WriteFile(finalDockerfilePath, finalDockerfile, 0o600); err != nil {
		return err
	}
	buildOutput := filepath.Join(stage, "bundle")
	if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
		Dockerfile: finalDockerfilePath, ContextDirectory: emptyContext,
		Target: "bundle", Output: buildOutput, OutputType: "local",
		BuildContexts: map[string]string{
			"helmr_images":    workspaceContext,
			"helmr_installed": installedContext,
		},
	}); err != nil {
		return err
	}
	if err := builder.PublishBundleDirectory(buildOutput, destination); err != nil {
		return err
	}
	if printOutput {
		_, err = fmt.Fprintln(command.OutOrStdout(), destination)
		return err
	}
	return nil
}

func buildWorkspaceImages(
	ctx context.Context,
	command *cobra.Command,
	stage string,
	workspaceContext string,
	emptyContext string,
	projectContexts map[string]string,
	workspaceBuilds []builder.WorkspaceBuild,
) ([]map[string]string, error) {
	workspaceInputs := make([]map[string]string, len(workspaceBuilds))
	workspaceOutputs := make(map[struct {
		dockerfile string
		target     string
	}]string, len(workspaceBuilds))
	for index, workspace := range workspaceBuilds {
		dockerfile, target, err := builder.WorkspaceImageDockerfile(workspace.Build)
		if err != nil {
			return nil, fmt.Errorf("workspace image %q: %w", workspace.DeclaredID, err)
		}
		key := struct {
			dockerfile string
			target     string
		}{string(dockerfile), target}
		filename, built := workspaceOutputs[key]
		if !built {
			dockerfilePath := filepath.Join(stage, fmt.Sprintf("Dockerfile.workspace-%d", index))
			if err := os.WriteFile(dockerfilePath, dockerfile, 0o600); err != nil {
				return nil, err
			}
			filename = fmt.Sprintf("workspace-%03d.oci.tar", index)
			output := filepath.Join(workspaceContext, filename)
			if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
				Dockerfile: dockerfilePath, ContextDirectory: emptyContext,
				Target: target, Output: output, OutputType: "oci",
				OutputAttributes: map[string]string{"rewrite-timestamp": "true"},
				BuildContexts:    projectContexts,
			}); err != nil {
				return nil, fmt.Errorf("build workspace image %q: %w", workspace.DeclaredID, err)
			}
			workspaceOutputs[key] = filename
		}
		workspaceInputs[index] = map[string]string{
			"declaredId": workspace.DeclaredID,
			"path":       "/workspace/images/" + filename,
		}
	}
	return workspaceInputs, nil
}

func executeDockerBuildx(
	ctx context.Context,
	command *cobra.Command,
	request dockerBuildxRequest,
) error {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("helmr build requires Docker Buildx")
	}
	var output strings.Builder
	output.WriteString("type=" + request.OutputType + ",dest=" + request.Output)
	attributeNames := make([]string, 0, len(request.OutputAttributes))
	for name := range request.OutputAttributes {
		attributeNames = append(attributeNames, name)
	}
	slices.Sort(attributeNames)
	for _, name := range attributeNames {
		output.WriteString("," + name + "=" + request.OutputAttributes[name])
	}
	arguments := []string{
		"buildx",
		"build",
		"--platform", "linux/amd64",
		"--file", request.Dockerfile,
		"--target", request.Target,
		"--output", output.String(),
		"--build-arg", "SOURCE_DATE_EPOCH=0",
		"--provenance=false",
		"--progress", "plain",
	}
	contextNames := make([]string, 0, len(request.BuildContexts))
	for name := range request.BuildContexts {
		contextNames = append(contextNames, name)
	}
	slices.Sort(contextNames)
	for _, name := range contextNames {
		arguments = append(arguments, "--build-context", name+"="+request.BuildContexts[name])
	}
	for _, id := range request.SecretIDs {
		if _, ok := os.LookupEnv(id); !ok {
			return fmt.Errorf("build secret environment variable %s is not set", id)
		}
		arguments = append(arguments, "--secret", "id="+id+",env="+id)
	}
	arguments = append(arguments, request.ContextDirectory)
	process := exec.CommandContext(ctx, docker, arguments...)
	process.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	process.Stdin = command.InOrStdin()
	process.Stdout = command.ErrOrStderr()
	process.Stderr = command.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("build deployment bundle with Docker Buildx: %w", err)
	}
	return nil
}
