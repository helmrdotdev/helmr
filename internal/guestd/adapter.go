package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/safepath"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"google.golang.org/protobuf/proto"
)

func parseAdapter(ctx context.Context, cfg Config, sourceRoot string, taskID string) ([]byte, error) {
	if err := prepareAdapterSource(ctx, sourceRoot); err != nil {
		return nil, adapterParseError{Kind: "bad_request", Message: err.Error()}
	}
	args := adapterRuntimeArgs(cfg.AdapterRegisterPath, cfg.AdapterPath,
		"parse",
		"--cwd", sourceRoot,
		"--task", taskID,
		"--output", "binary",
	)
	cmd := exec.CommandContext(ctx, cfg.AdapterRuntimePath, args...)
	cmd.Dir = sourceRoot
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, classifyAdapterParseError(taskID, stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

func indexAdapter(ctx context.Context, cfg Config, sourceRoot string) ([]byte, error) {
	if err := prepareAdapterSource(ctx, sourceRoot); err != nil {
		return nil, adapterParseError{Kind: "bad_request", Message: err.Error()}
	}
	args := adapterRuntimeArgs(cfg.AdapterRegisterPath, cfg.AdapterPath,
		"parse",
		"--cwd", sourceRoot,
		"--output", "json",
	)
	cmd := exec.CommandContext(ctx, cfg.AdapterRuntimePath, args...)
	cmd.Dir = sourceRoot
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, classifyAdapterParseError("", stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

func prepareAdapterSource(ctx context.Context, sourceRoot string) error {
	return prepareAdapterSourceWithRuntime(ctx, sourceRoot, sourceRoot, "", false)
}

func prepareAdapterSourceWithRuntime(ctx context.Context, sourceRoot string, sourceRootForStat string, imageRoot string, imageMode bool) error {
	if err := validateAdapterSourcePackageJSON(sourceRootForStat); err != nil {
		return err
	}
	if imageMode {
		return validateAdapterDependenciesInstalledInImage(sourceRootForStat, imageRoot)
	}
	return installAdapterDependencies(ctx, sourceRoot)
}

func validateAdapterSourcePackageJSON(sourceRoot string) error {
	dependencies, err := adapterPackageDependencies(sourceRoot)
	if err != nil {
		return err
	}
	if _, ok := dependencies["@helmr/sdk"]; !ok {
		return errors.New(`package.json must declare @helmr/sdk in dependencies`)
	}
	return nil
}

func adapterPackageDependencies(sourceRoot string) (map[string]any, error) {
	metadata, err := readAdapterPackageMetadata(sourceRoot)
	if err != nil {
		return nil, err
	}
	return metadata.Dependencies, nil
}

type adapterPackageMetadata struct {
	Dependencies   map[string]any `json:"dependencies"`
	PackageManager string         `json:"packageManager"`
}

func readAdapterPackageMetadata(sourceRoot string) (adapterPackageMetadata, error) {
	packagePath := filepath.Join(sourceRoot, "package.json")
	metadata, err := os.Stat(packagePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return adapterPackageMetadata{}, errors.New("package.json is required for Helmr task projects; run helmr init or add @helmr/sdk to dependencies")
		}
		return adapterPackageMetadata{}, fmt.Errorf("inspect package.json: %w", err)
	}
	if metadata.IsDir() {
		return adapterPackageMetadata{}, errors.New("package.json must be a file")
	}
	body, err := os.ReadFile(packagePath)
	if err != nil {
		return adapterPackageMetadata{}, fmt.Errorf("read package.json: %w", err)
	}
	var packageJSON adapterPackageMetadata
	if err := json.Unmarshal(body, &packageJSON); err != nil {
		return adapterPackageMetadata{}, fmt.Errorf("decode package.json: %w", err)
	}
	return packageJSON, nil
}

func validateAdapterDependenciesInstalledInImage(sourceRoot string, imageRoot string) error {
	imageRoot = filepath.Clean(imageRoot)
	if strings.TrimSpace(imageRoot) == "" || imageRoot == "." {
		return errors.New("sandbox image root is required to validate task project dependencies")
	}
	dependencies, err := adapterPackageDependencies(sourceRoot)
	if err != nil {
		return err
	}
	if len(dependencies) == 0 {
		return nil
	}
	missing := make([]string, 0, len(dependencies))
	for name := range dependencies {
		if !adapterDependencyInstalledInImage(sourceRoot, imageRoot, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("task project dependencies are not installed in the sandbox image: %s; install task dependencies during the sandbox image build", strings.Join(missing, ", "))
}

func adapterDependencyInstalledInImage(sourceRoot string, imageRoot string, name string) bool {
	current := filepath.Clean(sourceRoot)
	for {
		if _, err := os.Stat(filepath.Join(current, "node_modules", filepath.FromSlash(name), "package.json")); err == nil {
			return true
		}
		if current == imageRoot {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current || !isPathWithin(parent, imageRoot) {
			return false
		}
		current = parent
	}
}

func isPathWithin(path string, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func adapterRuntimeArgs(registerPath string, adapterPath string, args ...string) []string {
	if strings.TrimSpace(registerPath) == "" {
		return append([]string{adapterPath}, args...)
	}
	return append([]string{"--import", registerPath, adapterPath}, args...)
}

func installAdapterDependencies(ctx context.Context, sourceRoot string) error {
	if err := validateAdapterDependenciesInstalledInProject(sourceRoot); err == nil {
		return nil
	}
	metadata, err := readAdapterPackageMetadata(sourceRoot)
	if err != nil {
		return err
	}
	packageManager := strings.TrimSpace(metadata.PackageManager)
	if packageManager == "" {
		return errors.New("package.json must declare packageManager for deployment builds")
	}
	command, args, err := adapterPackageInstallCommand(sourceRoot, packageManager)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = sourceRoot
	env, err := adapterDependencyInstallEnv(sourceRoot)
	if err != nil {
		return err
	}
	cmd.Env = env
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

func adapterDependencyInstallEnv(sourceRoot string) ([]string, error) {
	workspace := filepath.Join(sourceRoot, ".helmr-build")
	home := filepath.Join(workspace, "home")
	cache := filepath.Join(workspace, "cache")
	npmCache := filepath.Join(cache, "npm")
	for _, dir := range []string{home, cache, npmCache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create task dependency install directory %s: %w", dir, err)
		}
	}
	env := os.Environ()
	env = setProcessEnv(env, "HOME", home)
	env = setProcessEnv(env, "XDG_CACHE_HOME", cache)
	env = setProcessEnv(env, "npm_config_cache", npmCache)
	env = setProcessEnv(env, "npm_config_update_notifier", "false")
	return env, nil
}

func setProcessEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func validateAdapterDependenciesInstalledInProject(sourceRoot string) error {
	return validateAdapterDependenciesInstalledInImage(sourceRoot, sourceRoot)
}

func adapterPackageInstallCommand(sourceRoot string, packageManager string) (string, []string, error) {
	name, _, _ := strings.Cut(packageManager, "@")
	switch name {
	case "bun":
		args := []string{"install"}
		if fileExists(filepath.Join(sourceRoot, "bun.lock")) || fileExists(filepath.Join(sourceRoot, "bun.lockb")) {
			args = append(args, "--frozen-lockfile")
		}
		return "bun", args, nil
	case "npm":
		if fileExists(filepath.Join(sourceRoot, "package-lock.json")) {
			return "npm", []string{"ci"}, nil
		}
		return "npm", []string{"install"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported packageManager %q; supported package managers: bun, npm", packageManager)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func runtimeSourceHostPath(imageRoot string, runtimePath string, imageMode bool) (string, error) {
	if !imageMode {
		return runtimePath, nil
	}
	return safepath.JoinSlash(imageRoot, strings.TrimPrefix(runtimePath, "/"))
}

type adapterParseError struct {
	Kind    string
	Message string
}

func (e adapterParseError) Error() string {
	if e.Message == "" {
		return "parse task bundle"
	}
	return "parse task bundle: " + e.Message
}

type adapterErrorPayload struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func classifyAdapterParseError(taskID string, stderr string, fallback error) adapterParseError {
	message := strings.TrimSpace(stderr)
	if message == "" && fallback != nil {
		message = fallback.Error()
	}
	for _, line := range reverseLines(stderr) {
		var payload adapterErrorPayload
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		kind := strings.TrimSpace(payload.Kind)
		if kind == "" {
			continue
		}
		payloadMessage := strings.TrimSpace(payload.Message)
		if payloadMessage == "" {
			payloadMessage = defaultAdapterParseMessage(kind, taskID, message)
		}
		return adapterParseError{Kind: kind, Message: payloadMessage}
	}
	return adapterParseError{Kind: "bad_request", Message: message}
}

func reverseLines(value string) []string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return lines
}

func defaultAdapterParseMessage(kind string, taskID string, fallback string) string {
	switch kind {
	case "task_not_found":
		if strings.TrimSpace(taskID) != "" {
			return "task not found: " + taskID
		}
	case "duplicate_task_id":
		if strings.TrimSpace(taskID) != "" {
			return "duplicate task id: " + taskID
		}
	case "missing_config":
		return "no helmr.Config.ts found"
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return kind
}

type adapterTaskResult struct {
	exitCode     int32
	errorMessage string
	outputJSON   string
}

func runAdapter(ctx context.Context, conn io.ReadWriter, cfg Config, imageRoot string, deploymentSourceRoot string, workspaceRoot string, runCwd string, imageConfig ociRuntimeConfig, imageMode bool, request *runv0.RunTaskRequest, registry *waitingRunRegistry) error {
	runStream := adapterRunStream{conn: conn}
	adapterRuntimePath := cfg.AdapterRuntimePath
	var adapterRuntimePrefixArgs []string
	adapterPath := cfg.AdapterPath
	adapterRegisterPath := cfg.AdapterRegisterPath
	taskAdapterCwd := deploymentSourceRoot
	if imageMode {
		if err := installAdapterBundle(cfg.AdapterBundlePath, imageRoot); err != nil {
			return writeRunSetupFailure(conn, err)
		}
		adapterPath = "/opt/helmr/adapter/main.js"
		adapterRegisterPath = "/opt/helmr/adapter/register.mjs"
		var err error
		adapterRuntimePath, err = imageNodeRuntimeCommand(imageRoot, imageConfig)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
	}
	launchCwd := runCwd
	var runtimeUser *resolvedRuntimeUser
	if imageMode {
		var err error
		runtimeUser, err = resolveRuntimeUser(imageRoot, imageConfig.User)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		launchCwd, err = resolveLaunchCwd(runCwd, defaultRuntimeWorkdir)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		taskAdapterCwd, err = materializeDeploymentSourceForRuntime(imageRoot, deploymentSourceRoot, launchCwd, runtimeUser)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		if err := prepareLaunchPath(imageRoot, launchCwd, runtimeUser); err != nil {
			return writeRunSetupFailure(conn, err)
		}
		if err := chownTree(workspaceRoot, runtimeUser.UID, runtimeUser.GID); err != nil {
			return writeRunSetupFailure(conn, fmt.Errorf("prepare workspace owner: %w", err))
		}
	}
	var cmdEnv []string
	if imageMode && runtimeUser != nil {
		cmdEnv = imageRuntimeEnv(imageConfig, runtimeUser, launchCwd)
	} else {
		cmdEnv = mergeEnv(os.Environ(), imageConfig.Env, nil)
	}
	taskAdapterSourceRoot, err := runtimeSourceHostPath(imageRoot, taskAdapterCwd, imageMode)
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	if err := prepareAdapterSourceWithRuntime(ctx, taskAdapterCwd, taskAdapterSourceRoot, imageRoot, imageMode); err != nil {
		return writeRunSetupFailure(conn, err)
	}
	taskContextJSON, err := adapterTaskContextJSON(request)
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	cmdArgs := append(append([]string{}, adapterRuntimePrefixArgs...), adapterRuntimeArgs(adapterRegisterPath, adapterPath,
		"run",
		"--cwd", runCwd,
		"--task-cwd", taskAdapterCwd,
		"--task", request.TaskId,
		"--run-id", request.RunId,
		"--payload-json", request.PayloadJson,
		"--task-context-json", taskContextJSON,
	)...)
	var controlListener net.Listener
	var controlReader io.ReadCloser
	var controlWriteFile *os.File
	if imageMode {
		reader, writer, err := os.Pipe()
		if err != nil {
			return writeRunSetupFailure(conn, fmt.Errorf("create adapter control pipe: %w", err))
		}
		controlReader = reader
		controlWriteFile = writer
		defer controlReader.Close()
		defer controlWriteFile.Close()
		cmdEnv = setEnvValue(cmdEnv, "HELMR_CONTROL_FD", "3")
	} else {
		var controlSocketPath string
		var cleanupControlSocket func()
		var err error
		controlListener, controlSocketPath, cleanupControlSocket, err = listenAdapterControlSocket()
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		defer cleanupControlSocket()
		cmdEnv = setEnvValue(cmdEnv, "HELMR_CONTROL_SOCKET", controlSocketPath)
	}
	cmd, err := adapterCommand(ctx, adapterRuntimePath, cmdArgs, launchCwd, cmdEnv, imageRoot, runtimeUser, imageMode)
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	if controlWriteFile != nil {
		cmd.ExtraFiles = []*os.File{controlWriteFile}
	}
	if imageMode {
		cleanupRuntimeMounts, err := mountImageRuntimeFilesystems(imageRoot)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		defer cleanupRuntimeMounts()
	}
	if err := applySecrets(imageRoot, workspaceRoot, request, runtimeUser, &cmd.Env); err != nil {
		return writeRunSetupFailure(conn, err)
	}
	pipes, err := openAdapterOutputPipes(cmd)
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pipes.close()
		return writeRunSetupFailure(conn, err)
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return signalAdapterProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		pipes.close()
		return writeRunSetupFailure(conn, err)
	}
	pipes.closeWriters()
	defer pipes.closeReaders()
	if controlWriteFile != nil {
		_ = controlWriteFile.Close()
	}

	var wg sync.WaitGroup
	controlErrCh := make(chan error, 1)
	resultCh := make(chan adapterTaskResult, 1)
	waitCh := make(chan error, 1)
	controlDone := make(chan struct{})
	recordControlErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case controlErrCh <- err:
		default:
		}
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		_ = forwardChunks(pipes.stdoutReader, func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: chunk}}
		}, &runStream)
	}()
	go func() {
		defer wg.Done()
		_ = forwardChunks(pipes.stderrReader, func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{Event: &runv0.RunEvent_StderrChunk{StderrChunk: chunk}}
		}, &runStream)
	}()
	go func() {
		defer wg.Done()
		defer close(controlDone)
		defer stdin.Close()
		controlConn := controlReader
		if controlConn == nil {
			var err error
			controlConn, err = acceptAdapterControlConnection(ctx, controlListener)
			if err != nil {
				recordControlErr(fmt.Errorf("accept adapter control connection: %w", err))
				return
			}
		}
		defer controlConn.Close()
		for {
			event, err := transport.ReadRunEvent(controlConn)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					recordControlErr(fmt.Errorf("read adapter control event: %w", err))
				}
				return
			}
			if wait := event.GetWaitRequested(); wait != nil {
				if err := runStream.writeEvent(event); err != nil {
					recordControlErr(fmt.Errorf("write wait request event: %w", err))
					return
				}
				if err := checkpointAndAttachAdapterRun(ctx, &runStream, registry, stdin); err != nil {
					recordControlErr(err)
					return
				}
				continue
			}
			if result := event.GetTaskResult(); result != nil {
				select {
				case resultCh <- adapterTaskResult{
					exitCode:     result.ExitCode,
					errorMessage: result.GetErrorMessage(),
					outputJSON:   result.GetOutputJson(),
				}:
				default:
				}
				return
			}
			if err := runStream.writeEvent(event); err != nil {
				recordControlErr(fmt.Errorf("write adapter control event: %w", err))
				return
			}
		}
	}()

	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case result := <-resultCh:
		if controlListener != nil {
			_ = controlListener.Close()
		}
		_ = stdin.Close()
		terminateAdapterCommand(cmd, waitCh)
		waitForAdapterForwarders(&wg)
		return writeAdapterResult(&runStream, result)
	case controlErr := <-controlErrCh:
		if controlListener != nil {
			_ = controlListener.Close()
		}
		_ = stdin.Close()
		terminateAdapterCommand(cmd, waitCh)
		waitForAdapterForwarders(&wg)
		return runStream.writeComplete(1, controlErr.Error(), "")
	case waitErr := <-waitCh:
		if controlListener != nil {
			_ = controlListener.Close()
		}
		_ = stdin.Close()
		if result, ok := waitForAdapterResultAfterExit(resultCh, controlDone, 250*time.Millisecond); ok {
			waitForAdapterForwarders(&wg)
			return writeAdapterResult(&runStream, result)
		}
		terminateAdapterCommand(cmd, nil)
		waitForAdapterForwarders(&wg)
		select {
		case result := <-resultCh:
			return writeAdapterResult(&runStream, result)
		default:
		}
		var controlErr error
		select {
		case controlErr = <-controlErrCh:
		default:
		}
		exitCode := int32(1)
		message := "adapter exited without reporting task_result"
		if controlErr != nil {
			message = controlErr.Error()
		} else if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = int32(exitErr.ExitCode())
			}
			if strings.TrimSpace(waitErr.Error()) != "" {
				message = waitErr.Error()
			}
		}
		return runStream.writeComplete(exitCode, message, "")
	}
}

func checkpointAndAttachAdapterRun(ctx context.Context, stream *adapterRunStream, registry *waitingRunRegistry, stdin io.Writer) error {
	var suspend runv0.SuspendForCheckpoint
	if err := stream.readProto(&suspend); err != nil {
		fmt.Fprintf(os.Stderr, "helmr checkpoint: read suspend failed: %v\n", err)
		_ = stream.writeCheckpointDiagnostic(fmt.Sprintf("read checkpoint suspend request: %v", err))
		return fmt.Errorf("read checkpoint suspend request: %w", err)
	}
	fmt.Fprintf(os.Stderr, "helmr checkpoint: suspend received waitpoint_id=%s checkpoint_id=%s\n", suspend.WaitpointId, suspend.CheckpointId)
	registration := registry.register(suspend.WaitpointId, suspend.CheckpointId)
	defer registration.unregister()
	syscall.Sync()
	if err := stream.writeCheckpointPauseReady(suspend.WaitpointId, suspend.CheckpointId); err != nil {
		fmt.Fprintf(os.Stderr, "helmr checkpoint: write pause ready failed: %v\n", err)
		_ = stream.writeCheckpointDiagnostic(fmt.Sprintf("write checkpoint pause ready: %v", err))
		return fmt.Errorf("write checkpoint pause ready: %w", err)
	}
	fmt.Fprintf(os.Stderr, "helmr checkpoint: waiting for resume attach waitpoint_id=%s checkpoint_id=%s\n", suspend.WaitpointId, suspend.CheckpointId)
	attachCtx, cancelAttach := context.WithTimeout(ctx, resumeAttachTimeout)
	attached, err := registration.wait(attachCtx)
	cancelAttach()
	if err != nil {
		fmt.Fprintf(os.Stderr, "helmr checkpoint: wait resume attach failed: %v\n", err)
		_ = stream.writeCheckpointDiagnostic(fmt.Sprintf("wait for resume attach: %v", err))
		return fmt.Errorf("wait for resume attach: %w", err)
	}
	fmt.Fprintf(os.Stderr, "helmr checkpoint: resume attached waitpoint_id=%s checkpoint_id=%s\n", suspend.WaitpointId, suspend.CheckpointId)
	decisionCtx, cancelDecision := context.WithTimeout(ctx, resumeAttachTimeout)
	decision, err := readResumeDecision(decisionCtx, attached)
	cancelDecision()
	if err != nil {
		fmt.Fprintf(os.Stderr, "helmr checkpoint: read resume decision failed: %v\n", err)
		_ = stream.writeCheckpointDiagnostic(fmt.Sprintf("read resume decision: %v", err))
		return fmt.Errorf("read resume decision: %w", err)
	}
	if err := stream.attachAndResume(attached, stdin, decision); err != nil {
		fmt.Fprintf(os.Stderr, "helmr checkpoint: attach and resume failed: %v\n", err)
		_ = stream.writeCheckpointDiagnostic(fmt.Sprintf("resume adapter stream: %v", err))
		return fmt.Errorf("resume adapter stream: %w", err)
	}
	fmt.Fprintf(os.Stderr, "helmr checkpoint: resumed waitpoint_id=%s checkpoint_id=%s\n", suspend.WaitpointId, suspend.CheckpointId)
	return nil
}

func writeRunSetupFailure(conn io.ReadWriter, err error) error {
	message := "guest runtime setup failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	runStream := adapterRunStream{conn: conn}
	if writeErr := runStream.writeEvent(&runv0.RunEvent{Event: &runv0.RunEvent_StderrChunk{StderrChunk: []byte(message + "\n")}}); writeErr != nil {
		return writeErr
	}
	return runStream.writeComplete(1, message, "")
}

type adapterRunStream struct {
	mu   sync.Mutex
	conn io.ReadWriter
}

func (s *adapterRunStream) writeEvent(event *runv0.RunEvent) error {
	return s.writeProto(event)
}

func (s *adapterRunStream) writeProto(message proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return transport.WriteProtoFrame(s.conn, message)
}

func (s *adapterRunStream) writeCheckpointPauseReady(waitpointID string, checkpointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return transport.WriteStreamFrameHeader(s.conn, transport.StreamHeader{
		Type:         transport.StreamTypeCheckpointPauseReady,
		WaitpointID:  waitpointID,
		CheckpointID: checkpointID,
	}, 0)
}

func (s *adapterRunStream) writeCheckpointDiagnostic(message string) error {
	return s.writeEvent(&runv0.RunEvent{Event: &runv0.RunEvent_LogEntry{LogEntry: "checkpoint: " + message}})
}

func (s *adapterRunStream) readProto(message proto.Message) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	return transport.ReadProtoFrame(conn, message)
}

func (s *adapterRunStream) attachAndResume(conn io.ReadWriter, stdin io.Writer, decision *runv0.ResumeDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	if err := transport.WriteProtoFrame(stdin, decision); err != nil {
		return err
	}
	return transport.WriteProtoFrame(s.conn, &runv0.ResumeAck{WaitpointId: decision.WaitpointId})
}

func (s *adapterRunStream) writeComplete(exitCode int32, message string, outputJSON string) error {
	complete := &runv0.TaskResult{ExitCode: exitCode}
	if message != "" {
		complete.ErrorMessage = &message
	}
	if outputJSON != "" {
		complete.OutputJson = &outputJSON
	}
	return s.writeEvent(&runv0.RunEvent{Event: &runv0.RunEvent_TaskResult{TaskResult: complete}})
}

func writeAdapterResult(stream *adapterRunStream, result adapterTaskResult) error {
	outputJSON := ""
	if result.exitCode == 0 {
		outputJSON = result.outputJSON
	}
	return stream.writeComplete(result.exitCode, result.errorMessage, outputJSON)
}

func waitForAdapterResultAfterExit(
	resultCh <-chan adapterTaskResult,
	controlDone <-chan struct{},
	timeout time.Duration,
) (adapterTaskResult, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case result := <-resultCh:
			return result, true
		case <-controlDone:
			select {
			case result := <-resultCh:
				return result, true
			default:
				return adapterTaskResult{}, false
			}
		case <-timer.C:
			return adapterTaskResult{}, false
		}
	}
}

func waitForAdapterForwarders(wg *sync.WaitGroup) {
	wg.Wait()
}

type adapterOutputPipes struct {
	stdoutReader *os.File
	stdoutWriter *os.File
	stderrReader *os.File
	stderrWriter *os.File
}

func openAdapterOutputPipes(cmd *exec.Cmd) (*adapterOutputPipes, error) {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, err
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	return &adapterOutputPipes{
		stdoutReader: stdoutReader,
		stdoutWriter: stdoutWriter,
		stderrReader: stderrReader,
		stderrWriter: stderrWriter,
	}, nil
}

func (p *adapterOutputPipes) closeWriters() {
	if p == nil {
		return
	}
	if p.stdoutWriter != nil {
		_ = p.stdoutWriter.Close()
		p.stdoutWriter = nil
	}
	if p.stderrWriter != nil {
		_ = p.stderrWriter.Close()
		p.stderrWriter = nil
	}
}

func (p *adapterOutputPipes) closeReaders() {
	if p == nil {
		return
	}
	if p.stdoutReader != nil {
		_ = p.stdoutReader.Close()
		p.stdoutReader = nil
	}
	if p.stderrReader != nil {
		_ = p.stderrReader.Close()
		p.stderrReader = nil
	}
}

func (p *adapterOutputPipes) close() {
	p.closeWriters()
	p.closeReaders()
}

func terminateAdapterCommand(cmd *exec.Cmd, waitCh <-chan error) {
	if cmd.Process == nil {
		return
	}
	select {
	case <-waitCh:
		return
	default:
	}
	_ = signalAdapterProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-waitCh:
		return
	case <-time.After(250 * time.Millisecond):
	}
	_ = signalAdapterProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-waitCh:
	case <-time.After(250 * time.Millisecond):
	}
}

func signalAdapterProcessGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pgid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func forwardChunks(r io.Reader, event func([]byte) *runv0.RunEvent, stream *adapterRunStream) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if writeErr := stream.writeEvent(event(chunk)); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type resumeDecisionResult struct {
	decision *runv0.ResumeDecision
	err      error
}

func readResumeDecision(ctx context.Context, reader io.Reader) (*runv0.ResumeDecision, error) {
	result := make(chan resumeDecisionResult, 1)
	go func() {
		var decision runv0.ResumeDecision
		err := transport.ReadProtoFrame(reader, &decision)
		result <- resumeDecisionResult{decision: &decision, err: err}
	}()
	select {
	case value := <-result:
		return value.decision, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func listenAdapterControlSocket() (net.Listener, string, func(), error) {
	dir, err := mkdirGuestdTemp("helmr-control-*")
	if err != nil {
		return nil, "", func() {}, fmt.Errorf("create adapter control socket dir: %w", err)
	}
	hostSocketPath := filepath.Join(dir, "control.sock")
	cleanup := func() {
		_ = os.RemoveAll(dir)
	}
	_ = os.Remove(hostSocketPath)
	listener, err := net.Listen("unix", hostSocketPath)
	if err != nil {
		cleanup()
		return nil, "", func() {}, fmt.Errorf("listen adapter control socket: %w", err)
	}
	return listener, hostSocketPath, func() {
		_ = listener.Close()
		_ = os.Remove(hostSocketPath)
		cleanup()
	}, nil
}

type adapterControlAcceptResult struct {
	conn net.Conn
	err  error
}

func acceptAdapterControlConnection(ctx context.Context, listener net.Listener) (net.Conn, error) {
	result := make(chan adapterControlAcceptResult, 1)
	go func() {
		conn, err := listener.Accept()
		result <- adapterControlAcceptResult{conn: conn, err: err}
	}()
	select {
	case value := <-result:
		_ = listener.Close()
		return value.conn, value.err
	case <-ctx.Done():
		_ = listener.Close()
		return nil, ctx.Err()
	}
}
