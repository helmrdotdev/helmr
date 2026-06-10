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
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/adapter"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
)

var (
	deployAdapterRuntimePath = "node"
	deployArchiveTempDir     string
)

const deployDefaultWaitTimeout = 20 * time.Minute

var deployEventReconnectDelay = time.Second

func deployCommand() *cobra.Command {
	var projectRef string
	var envRef string
	var envFile string
	var detach bool
	var skipPromotion bool
	var timeout time.Duration
	var jsonOutput bool
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
			reporter := newDeployReporter(cmd, jsonOutput)
			if envFile = strings.TrimSpace(envFile); envFile != "" {
				if err := loadEnvFile(envFile); err != nil {
					return err
				}
			}
			if err := reporter.Step("Preparing project"); err != nil {
				return err
			}
			if err := prepareLocalDeploySource(cmd.Context(), absRoot); err != nil {
				return err
			}
			if err := reporter.Step("Inspecting config"); err != nil {
				return err
			}
			config, err := inspectDeployConfig(cmd, absRoot)
			if err != nil {
				return err
			}
			sdkVersion, err := installedTaskProjectPackageVersion(absRoot, "@helmr/sdk")
			if err != nil {
				return err
			}
			project, err := configProject(config, projectRef)
			if err != nil {
				return err
			}
			if err := reporter.Step("Creating archive"); err != nil {
				return err
			}
			tarArchive, cleanup, err := archive.CreateTarWithOptions(absRoot, deployArchiveTempDir, archive.TarOptions{
				ExcludePatterns: deployArchiveExcludePatterns(config),
			})
			if err != nil {
				return err
			}
			defer cleanup()
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			if err := reporter.Step("Uploading deployment"); err != nil {
				return err
			}
			response, err := control.CreateDeployment(cmd.Context(), api.CreateDeploymentRequest{
				ProjectID:             project,
				EnvironmentID:         strings.TrimSpace(envRef),
				ContentHash:           tarArchive.Digest,
				APIVersion:            api.CurrentAPIVersion,
				SDKVersion:            sdkVersion,
				CLIVersion:            version.Version,
				BundleFormatVersion:   api.CurrentBundleFormatVersion,
				WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
			}, tarArchive.Path)
			if err != nil {
				return err
			}
			if err := reporter.DeploymentCreated(response); err != nil {
				return err
			}
			if detach {
				return reporter.DeploymentResult(response, "queued")
			}
			scope, err := deploymentWaitScope(response)
			if err != nil {
				return err
			}
			deployed, err := waitForDeployment(cmd.Context(), control, response, scope, timeout, reporter)
			if err != nil {
				return err
			}
			if skipPromotion {
				return reporter.DeploymentResult(deployed, "deployed")
			}
			if err := reporter.Step("Promoting deployment"); err != nil {
				return err
			}
			promoted, err := control.PromoteDeployment(cmd.Context(), deployed.ID, api.PromoteDeploymentRequest{
				ProjectID:     scope.ProjectID,
				EnvironmentID: scope.EnvironmentID,
				Reason:        "deploy",
			})
			if err != nil {
				return err
			}
			return reporter.DeploymentResult(promoted, "promoted")
		},
	}
	cmd.Flags().StringVarP(&projectRef, "project", "p", "", "Project slug or ID. Defaults to helmr.config.ts project.")
	cmd.Flags().StringVarP(&envRef, "env", "e", "", "Environment slug or ID for this deployment.")
	cmd.Flags().StringVar(&envFile, "env-file", "", "Load environment variables from a file before reading helmr.config.ts.")
	cmd.Flags().BoolVar(&detach, "detach", false, "Queue the deployment build and return without promotion.")
	cmd.Flags().BoolVar(&skipPromotion, "skip-promotion", false, "Build the deployment without promoting it current.")
	cmd.Flags().DurationVar(&timeout, "timeout", deployDefaultWaitTimeout, "Maximum time to wait for deployment completion.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON lines for deployment progress.")
	return cmd
}

type deploymentStatusClient interface {
	GetDeployment(context.Context, string, api.GetDeploymentRequest) (api.DeploymentResponse, error)
	FollowDeploymentEvents(context.Context, string, api.GetDeploymentRequest, int64, func(api.RunEvent) error) error
}

type deployReporter interface {
	Step(string) error
	DeploymentCreated(api.DeploymentResponse) error
	Event(api.RunEvent) error
	DeploymentResult(api.DeploymentResponse, string) error
}

type cliDeployReporter struct {
	cmd        *cobra.Command
	jsonOutput bool
}

type cliDeployLine struct {
	Type       string                  `json:"type"`
	Step       string                  `json:"step,omitempty"`
	Phase      string                  `json:"phase,omitempty"`
	Deployment *api.DeploymentResponse `json:"deployment,omitempty"`
	Event      *api.RunEvent           `json:"event,omitempty"`
}

func newDeployReporter(cmd *cobra.Command, jsonOutput bool) deployReporter {
	return cliDeployReporter{cmd: cmd, jsonOutput: jsonOutput}
}

func (r cliDeployReporter) Step(message string) error {
	if r.jsonOutput {
		return format.JSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "step", Step: message}})
	}
	_, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "%s\n", message)
	return err
}

func (r cliDeployReporter) DeploymentCreated(deployment api.DeploymentResponse) error {
	if r.jsonOutput {
		return format.JSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_created", Deployment: &deployment}})
	}
	_, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "Deployment %s queued\n", deployment.ID)
	return err
}

func (r cliDeployReporter) Event(event api.RunEvent) error {
	if r.jsonOutput {
		return format.JSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_event", Event: &event}})
	}
	message := strings.TrimSpace(event.Message)
	if message == "" {
		message = strings.TrimSpace(event.Kind)
	}
	if message != "" {
		if _, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "%s\n", message); err != nil {
			return err
		}
	}
	return nil
}

func (r cliDeployReporter) DeploymentResult(deployment api.DeploymentResponse, phase string) error {
	if r.jsonOutput {
		return format.JSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_result", Phase: phase, Deployment: &deployment}})
	}
	_, err := fmt.Fprintln(r.cmd.OutOrStdout(), deploymentOutputRef(deployment))
	return err
}

func promoteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var reason string
	cmd := &cobra.Command{
		Use:   "promote DEPLOYMENT",
		Short: "Promote a deployed version to current.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			deployment, err := control.PromoteDeployment(cmd.Context(), args[0], api.PromoteDeploymentRequest{
				ProjectID:     strings.TrimSpace(projectID),
				EnvironmentID: strings.TrimSpace(environmentID),
				Reason:        strings.TrimSpace(reason),
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), deploymentOutputRef(deployment))
			return nil
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project ID or slug for the deployment.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment ID or slug for the deployment.")
	cmd.Flags().StringVar(&reason, "reason", "", "Promotion reason.")
	return cmd
}

func deploymentOutputRef(deployment api.DeploymentResponse) string {
	if strings.TrimSpace(deployment.Version) != "" {
		return strings.TrimSpace(deployment.Version)
	}
	return deployment.ID
}

func deploymentWaitScope(response api.DeploymentResponse) (api.GetDeploymentRequest, error) {
	projectID := strings.TrimSpace(response.ProjectID)
	environmentID := strings.TrimSpace(response.EnvironmentID)
	if projectID == "" || environmentID == "" {
		return api.GetDeploymentRequest{}, fmt.Errorf("deployment %s response did not include resolved project_id and environment_id", response.ID)
	}
	return api.GetDeploymentRequest{ProjectID: projectID, EnvironmentID: environmentID}, nil
}

func waitForDeployment(ctx context.Context, control deploymentStatusClient, initial api.DeploymentResponse, scope api.GetDeploymentRequest, timeout time.Duration, reporter deployReporter) (api.DeploymentResponse, error) {
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
	var cursor int64
	for {
		streamCtx, cancel := context.WithCancel(ctx)
		terminal := false
		err := control.FollowDeploymentEvents(streamCtx, initial.ID, scope, cursor, func(event api.RunEvent) error {
			if parsed, parseErr := strconv.ParseInt(event.ID, 10, 64); parseErr == nil && parsed > cursor {
				cursor = parsed
			}
			if err := reporter.Event(event); err != nil {
				return err
			}
			switch event.Kind {
			case "deployment.deployed", "deployment.failed":
				terminal = true
				cancel()
			}
			return nil
		})
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			return api.DeploymentResponse{}, fmt.Errorf("follow deployment %s events: %w", initial.ID, err)
		}
		if ctx.Err() != nil {
			return api.DeploymentResponse{}, fmt.Errorf("wait for deployment %s: %w", initial.ID, ctx.Err())
		}
		deployment, err := control.GetDeployment(ctx, initial.ID, scope)
		if err != nil {
			return api.DeploymentResponse{}, fmt.Errorf("get deployment %s: %w", initial.ID, err)
		}
		if terminal || deploymentFinished(deployment.Status) {
			return deploymentTerminalResult(deployment)
		}
		timer := time.NewTimer(deployEventReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.DeploymentResponse{}, fmt.Errorf("wait for deployment %s: %w", initial.ID, ctx.Err())
		case <-timer.C:
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

type nodePackageMetadata struct {
	Version string `json:"version"`
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
	_, err := installedTaskProjectPackagePath(cwd, name)
	return err == nil
}

func installedTaskProjectPackageVersion(cwd string, name string) (string, error) {
	path, err := installedTaskProjectPackagePath(cwd, name)
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s package metadata: %w", name, err)
	}
	var metadata nodePackageMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return "", fmt.Errorf("decode %s package metadata: %w", name, err)
	}
	if strings.TrimSpace(metadata.Version) == "" {
		return "", nil
	}
	return strings.TrimSpace(metadata.Version), nil
}

func installedTaskProjectPackagePath(cwd string, name string) (string, error) {
	current := filepath.Clean(cwd)
	for {
		path := filepath.Join(current, "node_modules", filepath.FromSlash(name), "package.json")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("task project dependency is not installed: %s; install dependencies before deploying", name)
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

func configProject(config deployConfig, override string) (string, error) {
	project := strings.TrimSpace(override)
	if project != "" {
		if err := validateProjectFlag(project); err != nil {
			return "", err
		}
	}
	if project == "" {
		project = strings.TrimSpace(config.Project)
	}
	if project == "" {
		return "", errors.New("project is required; set helmr.config.ts project or pass --project")
	}
	return project, nil
}

func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read --env-file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	for lineNumber, line := range lines {
		line = cleanEnvFileLine(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = trimEnvFileExportPrefix(line)
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("parse --env-file %s:%d: expected KEY=VALUE", path, lineNumber+1)
		}
		if strings.HasPrefix(key, "HELMR_") {
			return fmt.Errorf("parse --env-file %s:%d: %s uses the reserved HELMR_ namespace; set Helmr CLI/runtime configuration in the shell or CLI flags, not --env-file", path, lineNumber+1, key)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		parsedValue, err := parseEnvFileValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse --env-file %s:%d: %w", path, lineNumber+1, err)
		}
		if err := os.Setenv(key, parsedValue); err != nil {
			return fmt.Errorf("set --env-file %s:%d: %w", path, lineNumber+1, err)
		}
	}
	return nil
}

func cleanEnvFileLine(line string) string {
	line = strings.TrimSpace(line)
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inDoubleQuote && ch == '\\' {
			escaped = true
			continue
		}
		switch ch {
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '#':
			if !inSingleQuote && !inDoubleQuote && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return strings.TrimSpace(line[:i])
			}
		}
	}
	return line
}

func trimEnvFileExportPrefix(line string) string {
	if !strings.HasPrefix(line, "export") {
		return line
	}
	if len(line) == len("export") {
		return line
	}
	next := line[len("export")]
	if next != ' ' && next != '\t' {
		return line
	}
	return strings.TrimSpace(line[len("export"):])
}

func parseEnvFileValue(value string) (string, error) {
	if len(value) < 2 {
		return value, nil
	}
	quote := value[0]
	if quote != '"' && quote != '\'' {
		return value, nil
	}
	if value[len(value)-1] != quote || (quote == '"' && quoteIsEscaped(value, len(value)-1)) {
		return "", errors.New("quoted value is not terminated")
	}
	value = value[1 : len(value)-1]
	if quote == '\'' {
		return value, nil
	}
	replacer := strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`)
	return replacer.Replace(value), nil
}

func quoteIsEscaped(value string, index int) bool {
	backslashes := 0
	for i := index - 1; i >= 0 && value[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
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
	if err := validateDeployAdapterRuntime(cmd.Context(), adapterRuntimePath); err != nil {
		return nil, err
	}
	adapter, err := resolveDeployAdapter()
	if err != nil {
		return nil, err
	}
	args := deployAdapterRuntimeArgs(adapter, commandName, "--cwd", cwd)
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

func validateDeployAdapterRuntime(ctx context.Context, runtimePath string) error {
	const check = `const [major = 0, minor = 0] = process.versions.node.split(".").map(Number);
if (major < 22 || (major === 22 && minor < 18)) {
  console.error(` + "`" + `node >=22.18 is required for helmr deploy; found ${process.versions.node}` + "`" + `);
  process.exit(1);
}`
	command := exec.CommandContext(ctx, runtimePath, "-e", check)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return errors.New(message)
		}
		return fmt.Errorf("adapter runtime %q is not available; install node >=22.18 or set HELMR_ADAPTER_RUNTIME_PATH", runtimePath)
	}
	return nil
}

type deployAdapterFiles struct {
	MainPath     string
	RegisterPath string
}

func deployAdapterRuntimeArgs(adapter deployAdapterFiles, args ...string) []string {
	return append([]string{"--import", adapter.RegisterPath, adapter.MainPath}, args...)
}

func resolveDeployAdapter() (deployAdapterFiles, error) {
	explicitMain := strings.TrimSpace(os.Getenv("HELMR_ADAPTER_PATH"))
	explicitRegister := strings.TrimSpace(os.Getenv("HELMR_ADAPTER_REGISTER_PATH"))
	if explicitMain != "" || explicitRegister != "" {
		if explicitMain == "" || explicitRegister == "" {
			return deployAdapterFiles{}, errors.New("HELMR_ADAPTER_PATH and HELMR_ADAPTER_REGISTER_PATH must be set together")
		}
		if !isFile(explicitMain) {
			return deployAdapterFiles{}, fmt.Errorf("adapter path not found: %s", explicitMain)
		}
		if !isFile(explicitRegister) {
			return deployAdapterFiles{}, fmt.Errorf("adapter register hook not found: %s", explicitRegister)
		}
		return deployAdapterFiles{MainPath: explicitMain, RegisterPath: explicitRegister}, nil
	}
	resolved, err := adapter.Ensure()
	if err != nil {
		return deployAdapterFiles{}, err
	}
	return deployAdapterFiles{MainPath: resolved.MainPath, RegisterPath: resolved.RegisterPath}, nil
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
