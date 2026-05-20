package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/spf13/cobra"
)

var (
	deployBunPath        = "bun"
	deployAdapterPath    = "runtime/typescript/src/main.ts"
	deployAdapterSDKPath string
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
			config, err := inspectDeployConfig(cmd, absRoot)
			if err != nil {
				return err
			}
			tasks, err := indexDeployTasks(cmd, absRoot)
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
			response, err := control.CreateTaskDeployment(cmd.Context(), api.CreateTaskDeploymentRequest{
				ProjectID:     project,
				EnvironmentID: strings.TrimSpace(environmentID),
				Tasks:         tasks,
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

type deployRegistryOutput struct {
	Tasks map[string]deployTaskOutput `json:"tasks"`
}

type deployTaskOutput struct {
	ModulePath string `json:"modulePath"`
	ExportName string `json:"exportName"`
	Bundle     struct {
		Sandbox struct {
			Resources *deployTaskResources `json:"resources"`
		} `json:"sandbox"`
	} `json:"bundle"`
}

type deployTaskResources struct {
	CPU    uint32 `json:"cpu"`
	Memory string `json:"memory"`
}

func indexDeployTasks(cmd *cobra.Command, cwd string) ([]api.DeployedTaskCreate, error) {
	stdout, err := runDeployAdapter(cmd, "parse", cwd)
	if err != nil {
		return nil, fmt.Errorf("index tasks: %w", err)
	}
	var registry deployRegistryOutput
	if err := json.Unmarshal(stdout, &registry); err != nil {
		return nil, fmt.Errorf("decode task registry: %w", err)
	}
	taskIDs := make([]string, 0, len(registry.Tasks))
	for taskID := range registry.Tasks {
		taskIDs = append(taskIDs, taskID)
	}
	sort.Strings(taskIDs)
	tasks := make([]api.DeployedTaskCreate, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		task := registry.Tasks[taskID]
		resources, err := deployRunResources(task.Bundle.Sandbox.Resources)
		if err != nil {
			return nil, fmt.Errorf("task %q resources: %w", taskID, err)
		}
		tasks = append(tasks, api.DeployedTaskCreate{
			TaskID:             taskID,
			ModulePath:         filepath.ToSlash(strings.TrimSpace(task.ModulePath)),
			ExportName:         strings.TrimSpace(task.ExportName),
			RequestedMilliCPU:  resources.MilliCPU,
			RequestedMemoryMiB: resources.MemoryMiB,
		})
	}
	return tasks, nil
}

func deployRunResources(input *deployTaskResources) (compute.ResourceVector, error) {
	resources := compute.DefaultRunResources()
	if input != nil {
		if input.CPU != 0 {
			resources.MilliCPU = int64(input.CPU) * 1000
		}
		if strings.TrimSpace(input.Memory) != "" {
			memoryMiB, err := parseMemoryMiB(input.Memory)
			if err != nil {
				return compute.ResourceVector{}, err
			}
			resources.MemoryMiB = memoryMiB
		}
	}
	if err := resources.Validate(true); err != nil {
		return compute.ResourceVector{}, err
	}
	return resources, nil
}

func parseMemoryMiB(input string) (int64, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return 0, errors.New("memory is required")
	}
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "kib", multiplier: 1},
		{suffix: "ki", multiplier: 1},
		{suffix: "mib", multiplier: 1024},
		{suffix: "mi", multiplier: 1024},
		{suffix: "gib", multiplier: 1024 * 1024},
		{suffix: "gi", multiplier: 1024 * 1024},
	}
	lower := strings.ToLower(value)
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			amountText := strings.TrimSpace(value[:len(value)-len(unit.suffix)])
			amount, err := strconv.ParseInt(amountText, 10, 64)
			if err != nil || amount <= 0 {
				return 0, fmt.Errorf("memory %q must be a positive integer quantity", input)
			}
			if unit.multiplier == 1 {
				if amount%1024 != 0 {
					return 0, fmt.Errorf("memory %q must resolve to whole MiB", input)
				}
				return amount / 1024, nil
			}
			return amount * unit.multiplier / 1024, nil
		}
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("memory %q must use MiB or GiB units", input)
	}
	return amount, nil
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
	if commandName == "parse" {
		args = append(args, "--output", "json")
	}
	command := exec.CommandContext(cmd.Context(), bunPath, args...)
	command.Env = os.Environ()
	if sdkPath := resolveDeployAdapterSDKPath(adapterPath); sdkPath != "" {
		command.Env = append(command.Env, "HELMR_ADAPTER_SDK_PATH="+sdkPath)
	}
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

func resolveDeployAdapterSDKPath(adapterPath string) string {
	if explicit := firstNonEmpty(os.Getenv("HELMR_ADAPTER_SDK_PATH"), deployAdapterSDKPath); explicit != "" {
		return explicit
	}
	candidates := []string{}
	if adapterPath != "" {
		dir := filepath.Dir(adapterPath)
		candidates = append(candidates,
			filepath.Join(dir, "sdk.js"),
			filepath.Join(dir, "adapter", "sdk.js"),
			filepath.Join(dir, "..", "..", "..", "sdk", "typescript", "src", "index.ts"),
		)
	}
	if exe, err := deployExecutable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "adapter", "sdk.js"),
			filepath.Join(dir, "sdk.js"),
			filepath.Join(dir, "sdk", "typescript", "src", "index.ts"),
		)
	}
	for _, candidate := range candidates {
		if isFile(candidate) {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute
			}
			return candidate
		}
	}
	return ""
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
