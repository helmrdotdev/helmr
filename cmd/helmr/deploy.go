package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/spf13/cobra"
)

var (
	deployBunPath        = "bun"
	deployAdapterPath    = "runtime/typescript/src/main.ts"
	deployArchiveTempDir string
	deployExecutable     = os.Executable
)

func deployCommand() *cobra.Command {
	var projectID string
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
			if err := prepareLocalDeploySource(cmd, absRoot); err != nil {
				return err
			}
			config, err := inspectDeployConfig(cmd, absRoot)
			if err != nil {
				return err
			}
			project := strings.TrimSpace(projectID)
			if project == "" {
				project = strings.TrimSpace(config.Project)
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
			}, archive.Path)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), response.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "Project ID or slug for this deployment.")
	cmd.Flags().StringVar(&environmentID, "environment", "", "Environment ID or slug for this deployment.")
	return cmd
}

func prepareLocalDeploySource(cmd *cobra.Command, cwd string) error {
	if err := validateTaskProjectPackageJSON(cwd); err != nil {
		return err
	}
	bunPath := firstNonEmpty(os.Getenv("HELMR_BUN_PATH"), deployBunPath)
	if bunPath == "" {
		return errors.New("bun path is required")
	}
	args := []string{"install"}
	if deployLockfileExists(cwd) {
		args = append(args, "--frozen-lockfile")
	}
	command := exec.CommandContext(cmd.Context(), bunPath, args...)
	command.Dir = cwd
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
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

func validateTaskProjectPackageJSON(cwd string) error {
	packagePath := filepath.Join(cwd, "package.json")
	metadata, err := os.Stat(packagePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("package.json is required for Helmr task projects; run helmr init or add @helmr/sdk to dependencies")
		}
		return fmt.Errorf("inspect package.json: %w", err)
	}
	if metadata.IsDir() {
		return errors.New("package.json must be a file")
	}
	body, err := os.ReadFile(packagePath)
	if err != nil {
		return fmt.Errorf("read package.json: %w", err)
	}
	var packageJSON struct {
		Dependencies map[string]any `json:"dependencies"`
	}
	if err := json.Unmarshal(body, &packageJSON); err != nil {
		return fmt.Errorf("decode package.json: %w", err)
	}
	if _, ok := packageJSON.Dependencies["@helmr/sdk"]; !ok {
		return errors.New(`package.json must declare @helmr/sdk in dependencies`)
	}
	return nil
}

func deployLockfileExists(cwd string) bool {
	for _, name := range []string{"bun.lock", "bun.lockb"} {
		metadata, err := os.Stat(filepath.Join(cwd, name))
		if err == nil && !metadata.IsDir() {
			return true
		}
	}
	return false
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
	bunPath := firstNonEmpty(os.Getenv("HELMR_BUN_PATH"), deployBunPath)
	if bunPath == "" {
		return nil, errors.New("bun path is required")
	}
	adapterPath, err := resolveDeployAdapterPath()
	if err != nil {
		return nil, err
	}
	args := []string{adapterPath, commandName, "--cwd", cwd}
	command := exec.CommandContext(cmd.Context(), bunPath, args...)
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
