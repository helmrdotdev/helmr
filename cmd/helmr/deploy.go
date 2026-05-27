package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/spf13/cobra"
)

var (
	deployAdapterRuntimePath = "node"
	deployAdapterPath        = "runtime/typescript/src/main.ts"
	deployArchiveTempDir     string
	deployExecutable         = os.Executable
	deployWaitPollInterval   = 2 * time.Second
)

const deployDefaultWaitTimeout = 20 * time.Minute

func deployCommand() *cobra.Command {
	var environmentID string
	var detach bool
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "deploy [path]",
		Short: "Deploy tasks from a helmr.config.ts project.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sourceRoot := "."
			if len(args) > 0 {
				sourceRoot = args[0]
			}
			absRoot, err := filepath.Abs(sourceRoot)
			if err != nil {
				return err
			}
			info, err := os.Stat(absRoot)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("deploy path must be a directory: %s", sourceRoot)
			}
			if err := prepareLocalDeploySource(cmd.Context(), absRoot); err != nil {
				return err
			}
			config, err := inspectDeployConfig(cmd, absRoot)
			if err != nil {
				return err
			}
			project, err := configProject(config)
			if err != nil {
				return err
			}
			tarArchive, cleanup, err := archive.CreateTarWithOptions(absRoot, deployArchiveTempDir, archive.TarOptions{
				ExcludePatterns: deployArchiveExcludePatterns(config),
			})
			if err != nil {
				return err
			}
			defer cleanup()
			control, err := controlClient()
			if err != nil {
				return err
			}
			response, err := control.CreateDeployment(cmd.Context(), api.CreateDeploymentRequest{
				ProjectID:     project,
				EnvironmentID: strings.TrimSpace(environmentID),
				ContentHash:   tarArchive.Digest,
			}, tarArchive.Path)
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintln(cmd.OutOrStdout(), response.ID)
				return nil
			}
			scope, err := deploymentWaitScope(response)
			if err != nil {
				return err
			}
			deployed, err := waitForDeployment(cmd.Context(), control, response, scope, waitTimeout)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), deployed.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&environmentID, "environment", "", "Environment ID or slug for this deployment.")
	cmd.Flags().BoolVar(&detach, "detach", false, "Queue the deployment and return before it becomes current.")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", deployDefaultWaitTimeout, "Maximum time to wait for deployment completion.")
	return cmd
}

type deploymentStatusClient interface {
	GetDeployment(context.Context, string, api.GetDeploymentRequest) (api.DeploymentResponse, error)
}

func deploymentWaitScope(response api.DeploymentResponse) (api.GetDeploymentRequest, error) {
	projectID := strings.TrimSpace(response.ProjectID)
	environmentID := strings.TrimSpace(response.EnvironmentID)
	if projectID == "" || environmentID == "" {
		return api.GetDeploymentRequest{}, fmt.Errorf("deployment %s response did not include resolved project_id and environment_id", response.ID)
	}
	return api.GetDeploymentRequest{ProjectID: projectID, EnvironmentID: environmentID}, nil
}

func waitForDeployment(ctx context.Context, control deploymentStatusClient, initial api.DeploymentResponse, scope api.GetDeploymentRequest, timeout time.Duration) (api.DeploymentResponse, error) {
	if strings.TrimSpace(initial.ID) == "" {
		return api.DeploymentResponse{}, errors.New("deployment response id is empty")
	}
	if deploymentFinished(initial.Status) {
		return deploymentTerminalResult(initial)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	firstPoll := true
	for {
		if !firstPoll {
			timer := time.NewTimer(deployWaitPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return api.DeploymentResponse{}, fmt.Errorf("wait for deployment %s: %w", initial.ID, ctx.Err())
			case <-timer.C:
			}
		}
		firstPoll = false
		deployment, err := control.GetDeployment(ctx, initial.ID, scope)
		if err != nil {
			return api.DeploymentResponse{}, fmt.Errorf("get deployment %s: %w", initial.ID, err)
		}
		if deploymentFinished(deployment.Status) {
			return deploymentTerminalResult(deployment)
		}
	}
}

func deploymentFinished(status string) bool {
	switch strings.TrimSpace(status) {
	case "deployed", "failed":
		return true
	default:
		return false
	}
}

func deploymentTerminalResult(deployment api.DeploymentResponse) (api.DeploymentResponse, error) {
	switch strings.TrimSpace(deployment.Status) {
	case "deployed":
		return deployment, nil
	case "failed":
		message := strings.TrimSpace(deploymentErrorMessage(deployment))
		if message == "" {
			message = "deployment build failed"
		}
		return api.DeploymentResponse{}, fmt.Errorf("deployment %s failed: %s", deployment.ID, message)
	default:
		return api.DeploymentResponse{}, fmt.Errorf("deployment %s reached unexpected status %q", deployment.ID, deployment.Status)
	}
}

func deploymentErrorMessage(deployment api.DeploymentResponse) string {
	if deployment.Error == nil {
		return ""
	}
	return strings.TrimSpace(deployment.Error.Message)
}

func prepareLocalDeploySource(ctx context.Context, cwd string) error {
	metadata, err := validateTaskProjectPackageJSON(cwd)
	if err != nil {
		return err
	}
	if err := validateTaskProjectDependenciesInstalled(cwd, metadata.Dependencies); err == nil {
		return nil
	}
	if err := installTaskProjectDependencies(ctx, cwd, metadata.PackageManager); err != nil {
		return err
	}
	return validateTaskProjectDependenciesInstalled(cwd, metadata.Dependencies)
}

type taskProjectPackageMetadata struct {
	Dependencies   map[string]any `json:"dependencies"`
	PackageManager string         `json:"packageManager"`
}

func validateTaskProjectPackageJSON(cwd string) (taskProjectPackageMetadata, error) {
	packagePath := filepath.Join(cwd, "package.json")
	metadata, err := os.Stat(packagePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return taskProjectPackageMetadata{}, errors.New("package.json is required for Helmr task projects; run helmr init or add @helmr/sdk to dependencies")
		}
		return taskProjectPackageMetadata{}, fmt.Errorf("inspect package.json: %w", err)
	}
	if metadata.IsDir() {
		return taskProjectPackageMetadata{}, errors.New("package.json must be a file")
	}
	body, err := os.ReadFile(packagePath)
	if err != nil {
		return taskProjectPackageMetadata{}, fmt.Errorf("read package.json: %w", err)
	}
	var packageJSON taskProjectPackageMetadata
	if err := json.Unmarshal(body, &packageJSON); err != nil {
		return taskProjectPackageMetadata{}, fmt.Errorf("decode package.json: %w", err)
	}
	if _, ok := packageJSON.Dependencies["@helmr/sdk"]; !ok {
		return taskProjectPackageMetadata{}, errors.New(`package.json must declare @helmr/sdk in dependencies`)
	}
	if strings.TrimSpace(packageJSON.PackageManager) == "" {
		return taskProjectPackageMetadata{}, errors.New("package.json must declare packageManager for deployment builds")
	}
	return packageJSON, nil
}

func validateTaskProjectDependenciesInstalled(cwd string, dependencies map[string]any) error {
	missing := []string{}
	for name := range dependencies {
		if !taskProjectDependencyInstalled(cwd, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("task project dependencies are not installed: %s; install dependencies before deploying", strings.Join(missing, ", "))
}

func taskProjectDependencyInstalled(cwd string, name string) bool {
	current := filepath.Clean(cwd)
	for {
		if _, err := os.Stat(filepath.Join(current, "node_modules", filepath.FromSlash(name), "package.json")); err == nil {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func installTaskProjectDependencies(ctx context.Context, cwd string, packageManager string) error {
	command, args, err := taskProjectPackageInstallCommand(cwd, packageManager)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("install task project dependencies: %s", message)
	}
	return nil
}

func taskProjectPackageInstallCommand(cwd string, packageManager string) (string, []string, error) {
	name, _, _ := strings.Cut(packageManager, "@")
	switch name {
	case "bun":
		args := []string{"install"}
		if taskProjectFileExists(filepath.Join(cwd, "bun.lock")) || taskProjectFileExists(filepath.Join(cwd, "bun.lockb")) {
			args = append(args, "--frozen-lockfile")
		}
		return "bun", args, nil
	case "npm":
		if taskProjectFileExists(filepath.Join(cwd, "package-lock.json")) {
			return "npm", []string{"ci"}, nil
		}
		return "npm", []string{"install"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported packageManager %q; supported package managers: bun, npm", packageManager)
	}
}

func taskProjectFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

type deployConfig struct {
	Project        string    `json:"project"`
	Dirs           []string  `json:"dirs"`
	IgnorePatterns *[]string `json:"ignorePatterns"`
}

func inspectDeployConfig(cmd *cobra.Command, cwd string) (deployConfig, error) {
	stdout, err := runDeployAdapter(cmd, "inspect-config", cwd)
	if err != nil {
		return deployConfig{}, fmt.Errorf("inspect helmr.config.ts: %w", err)
	}
	var config deployConfig
	if err := json.Unmarshal(stdout, &config); err != nil {
		return deployConfig{}, fmt.Errorf("decode helmr.config.ts: %w", err)
	}
	return config, nil
}

func configProject(config deployConfig) (string, error) {
	project := strings.TrimSpace(config.Project)
	if project == "" {
		return "", errors.New("helmr.config.ts project is required")
	}
	return project, nil
}

func deployArchiveExcludePatterns(config deployConfig) []string {
	patterns := []string{}
	if config.IgnorePatterns != nil {
		patterns = append(patterns, (*config.IgnorePatterns)...)
	} else {
		patterns = append(patterns,
			"**/*.test.*",
			"**/*.spec.*",
			"**/_*.*",
		)
	}
	patterns = append(patterns,
		"**/node_modules/**",
		"**/.git/**",
		"**/.helmr/**",
		"**/.next/**",
		"**/.env",
		"**/.env.*",
	)
	return patterns
}

func runDeployAdapter(cmd *cobra.Command, commandName string, cwd string) ([]byte, error) {
	adapterRuntimePath := firstNonEmpty(os.Getenv("HELMR_ADAPTER_RUNTIME_PATH"), deployAdapterRuntimePath)
	if adapterRuntimePath == "" {
		return nil, errors.New("adapter runtime path is required")
	}
	adapterPath, err := resolveDeployAdapterPath()
	if err != nil {
		return nil, err
	}
	args, err := deployAdapterRuntimeArgs(adapterPath, commandName, "--cwd", cwd)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(cmd.Context(), adapterRuntimePath, args...)
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return stdout.Bytes(), nil
}

func deployAdapterRuntimeArgs(adapterPath string, args ...string) ([]string, error) {
	registerPath, err := resolveDeployAdapterRegisterPath()
	if err != nil {
		return nil, err
	}
	return append([]string{"--import", registerPath, adapterPath}, args...), nil
}

func resolveDeployAdapterRegisterPath() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("HELMR_ADAPTER_REGISTER_PATH")); explicit != "" {
		if isFile(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("adapter register hook not found: %s", explicit)
	}
	for _, candidate := range []string{
		filepath.Join(filepath.Dir(deployAdapterPath), "register.mjs"),
		filepath.Join("runtime", "typescript", "src", "register.mjs"),
	} {
		if isFile(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs, nil
			}
			return candidate, nil
		}
	}
	if exe, err := deployExecutable(); err == nil {
		dir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(dir, "adapter", "register.mjs"),
			filepath.Join(dir, "runtime", "typescript", "src", "register.mjs"),
		} {
			if isFile(candidate) {
				return candidate, nil
			}
		}
	}
	return "", errors.New("adapter register hook is required; set HELMR_ADAPTER_REGISTER_PATH")
}

func resolveDeployAdapterPath() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("HELMR_ADAPTER_PATH")); explicit != "" {
		return explicit, nil
	}
	candidates := []string{}
	if configured := strings.TrimSpace(deployAdapterPath); configured != "" {
		candidates = append(candidates, configured)
	}
	if exe, err := deployExecutable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "adapter", "main.js"),
			filepath.Join(dir, "adapter", "main.ts"),
			filepath.Join(dir, "runtime", "typescript", "src", "main.ts"),
		)
	}
	for _, candidate := range candidates {
		if filepath.IsAbs(candidate) {
			if isFile(candidate) {
				return candidate, nil
			}
			continue
		}
		if isFile(candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("adapter path is required; set HELMR_ADAPTER_PATH")
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
