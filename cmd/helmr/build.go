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

	"github.com/helmrdotdev/helmr/internal/buildcontext"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/hostconfig"
	"github.com/spf13/cobra"
)

// deploymentBundleBuilderImage is injected by Product release automation only
// after the canonical image has been published by digest.
var deploymentBundleBuilderImage string
var buildContextTempDir string

var runDockerBuildx = executeDockerBuildx

type dockerBuildxRequest struct {
	Runner           dockerBuildRunner
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
	command := &cobra.Command{
		Use:   "build [path]",
		Short: "Build a verified deployment bundle locally.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			source := "."
			if len(arguments) == 1 {
				source = arguments[0]
			}
			return buildDeploymentBundleAt(command.Context(), command, source, output, true)
		},
	}
	command.Flags().StringVarP(
		&output,
		"output",
		"o",
		".helmr/deployment-bundle",
		"New deployment bundle directory.",
	)
	return command
}

func buildDeploymentBundleAt(
	ctx context.Context,
	command *cobra.Command,
	source string,
	output string,
	printOutput bool,
) (returnErr error) {
	builderContext, err := builder.BuilderContext(deploymentBundleBuilderImage)
	if err != nil {
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

	runner, err := prepareDockerBuildRunner(ctx)
	if err != nil {
		return fmt.Errorf("prepare local bundle builder: %w", err)
	}

	captured, err := buildcontext.Capture(ctx, root, buildContextTempDir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, captured.Close()) }()
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".helmr-bundle-build-")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(stage)) }()
	contextDirectory := captured.Path
	// The source is captured first; the config then runs once, on this host,
	// against the live project so its ordinary imports resolve. Every target
	// input below comes from the capture and the resolved document.
	resolved, err := evaluateHostConfig(ctx, root, command.ErrOrStderr())
	if err != nil {
		return err
	}
	if err := resolved.Resolve(contextDirectory); err != nil {
		return err
	}
	install, err := builder.SelectInstallPlan(contextDirectory, resolved.Build.InstallCommand)
	if err != nil {
		return err
	}
	install.SecretIDs, err = builder.NormalizeSecretIDs(resolved.Build.Secrets)
	if err != nil {
		return err
	}
	emptyContext := filepath.Join(stage, "empty-context")
	if err := os.Mkdir(emptyContext, 0o755); err != nil {
		return err
	}
	configContext := filepath.Join(stage, "config")
	if err := os.Mkdir(configContext, 0o755); err != nil {
		return err
	}
	discovery, err := deployment.CanonicalBuildConfig(resolved.Discovery)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configContext, "config.json"), discovery, 0o444); err != nil {
		return err
	}
	// One environment for the whole build: the pinned builder, or the project's
	// build.builder steps run once and exported, never repeated per graph.
	environmentContext := builderContext
	if steps := resolved.Build.Builder.Steps; len(steps) != 0 {
		environmentDockerfile, err := builder.EnvironmentDockerfile(steps)
		if err != nil {
			return err
		}
		environmentDockerfilePath, err := writeGraph(stage, "Dockerfile.environment", environmentDockerfile)
		if err != nil {
			return err
		}
		environmentLayout := filepath.Join(stage, "environment-layout")
		if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
			Runner:     runner,
			Dockerfile: environmentDockerfilePath, ContextDirectory: contextDirectory,
			Target: "environment", Output: environmentLayout, OutputType: "oci",
			OutputAttributes: map[string]string{"rewrite-timestamp": "true", "tar": "false"},
			BuildContexts:    map[string]string{builder.BuilderContextName: builderContext},
		}); err != nil {
			return fmt.Errorf("prepare build environment: %w", err)
		}
		environmentContext, err = builder.LayoutContext(environmentLayout)
		if err != nil {
			return fmt.Errorf("validate build environment: %w", err)
		}
	}
	installedDockerfile, err := builder.InstalledDockerfile(install)
	if err != nil {
		return err
	}
	installedDockerfilePath, err := writeGraph(stage, "Dockerfile.installed", installedDockerfile)
	if err != nil {
		return err
	}
	installedLayout := filepath.Join(stage, "installed-layout")
	if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
		Runner:     runner,
		Dockerfile: installedDockerfilePath, ContextDirectory: contextDirectory,
		Target: "installed-tree", Output: installedLayout, OutputType: "oci",
		OutputAttributes: map[string]string{"rewrite-timestamp": "true", "tar": "false"},
		BuildContexts:    map[string]string{builder.EnvironmentContextName: environmentContext},
		SecretIDs:        install.SecretIDs,
	}); err != nil {
		return fmt.Errorf("install project dependencies: %w", err)
	}
	installedContext, err := builder.LayoutContext(installedLayout)
	if err != nil {
		return fmt.Errorf("validate installed project tree: %w", err)
	}
	projectContexts := map[string]string{"helmr_installed": installedContext}
	analysisDockerfile, err := builder.AnalysisDockerfile()
	if err != nil {
		return err
	}
	analysisDockerfilePath, err := writeGraph(stage, "Dockerfile.analysis", analysisDockerfile)
	if err != nil {
		return err
	}
	analysisOutput := filepath.Join(stage, "analysis")
	if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
		Runner:     runner,
		Dockerfile: analysisDockerfilePath, ContextDirectory: emptyContext,
		Target: "analysis", Output: analysisOutput, OutputType: "local",
		BuildContexts: map[string]string{
			builder.BuilderContextName:     builderContext,
			builder.EnvironmentContextName: environmentContext,
			builder.ConfigContextName:      configContext,
			"helmr_installed":              installedContext,
		},
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
		runner,
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
	finalDockerfile, err := builder.Dockerfile()
	if err != nil {
		return err
	}
	finalDockerfilePath, err := writeGraph(stage, "Dockerfile.final", finalDockerfile)
	if err != nil {
		return err
	}
	buildOutput := filepath.Join(stage, "bundle")
	if err := runDockerBuildx(ctx, command, dockerBuildxRequest{
		Runner:     runner,
		Dockerfile: finalDockerfilePath, ContextDirectory: emptyContext,
		Target: "bundle", Output: buildOutput, OutputType: "local",
		BuildContexts: map[string]string{
			builder.BuilderContextName:     builderContext,
			builder.EnvironmentContextName: environmentContext,
			builder.ConfigContextName:      configContext,
			"helmr_images":                 workspaceContext,
			"helmr_installed":              installedContext,
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

// evaluateHostConfig is the single evaluation of helmr.config.ts per build.
var evaluateHostConfig = hostconfig.Evaluate

// writeGraph stores a generated Dockerfile with its own ignore file beside it:
// BuildKit prefers <Dockerfile>.dockerignore over a .dockerignore inside the
// context, so a project's Docker ignore rules cannot drop captured source.
func writeGraph(stage, name string, dockerfile []byte) (string, error) {
	path := filepath.Join(stage, name)
	if err := os.WriteFile(path, dockerfile, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".dockerignore", []byte(builder.CapturedSourceIgnoreFile), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func buildWorkspaceImages(
	ctx context.Context,
	command *cobra.Command,
	stage string,
	workspaceContext string,
	emptyContext string,
	projectContexts map[string]string,
	workspaceBuilds []builder.WorkspaceBuild,
	runner dockerBuildRunner,
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
				Runner:     runner,
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
		"--builder", request.Runner.name,
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
	process := exec.CommandContext(ctx, request.Runner.docker, append(append([]string{}, request.Runner.selector...), arguments...)...)
	process.Env = request.Runner.environment
	process.Stdin = command.InOrStdin()
	process.Stdout = command.ErrOrStderr()
	process.Stderr = command.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("build deployment bundle with Docker Buildx: %w", errors.Join(err, ctx.Err()))
	}
	return nil
}
