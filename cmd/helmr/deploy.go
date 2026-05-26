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

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/spf13/cobra"
)

var (
	deployAdapterRuntimePath = "node"
	deployAdapterPath        = "runtime/typescript/src/main.ts"
	deployArchiveTempDir     string
	deployExecutable         = os.Executable
)

func deployCommand() *cobra.Command {
	var environmentID string
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
			archive, cleanup, err := sourcetar.CreateTarWithOptions(absRoot, deployArchiveTempDir, sourcetar.TarOptions{
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
				ContentHash:   archive.Digest,
			}, archive.Path)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), response.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&environmentID, "environment", "", "Environment ID or slug for this deployment.")
	return cmd
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
