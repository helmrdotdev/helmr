package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"google.golang.org/protobuf/proto"
)

func parseAdapter(ctx context.Context, cfg Config, sourceRoot string, taskID string) ([]byte, error) {
	if err := prepareAdapterSource(ctx, cfg, sourceRoot); err != nil {
		return nil, adapterParseError{Kind: "bad_request", Message: err.Error()}
	}
	cmd := exec.CommandContext(ctx, cfg.BunPath, cfg.AdapterPath,
		"parse",
		"--cwd", sourceRoot,
		"--task", taskID,
		"--output", "binary",
	)
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
	if err := prepareAdapterSource(ctx, cfg, sourceRoot); err != nil {
		return nil, adapterParseError{Kind: "bad_request", Message: err.Error()}
	}
	cmd := exec.CommandContext(ctx, cfg.BunPath, cfg.AdapterPath,
		"parse",
		"--cwd", sourceRoot,
		"--output", "json",
	)
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

func prepareAdapterSource(ctx context.Context, cfg Config, sourceRoot string) error {
	return prepareAdapterSourceWithRuntime(ctx, cfg.BunPath, nil, sourceRoot, sourceRoot, mergeEnv(os.Environ(), nil, nil), "", nil, false)
}

func prepareAdapterSourceWithRuntime(ctx context.Context, bunPath string, bunPrefixArgs []string, sourceRoot string, sourceRootForStat string, env []string, imageRoot string, runtimeUser *resolvedRuntimeUser, imageMode bool) error {
	if err := validateAdapterSourcePackageJSON(sourceRootForStat); err != nil {
		return err
	}
	installEnv, err := adapterInstallEnv(sourceRoot, sourceRootForStat, env, runtimeUser)
	if err != nil {
		return err
	}
	args := []string{"install"}
	if lockfileExists(sourceRootForStat) {
		args = append(args, "--frozen-lockfile")
	}
	cmdArgs := append(append([]string{}, bunPrefixArgs...), args...)
	cmd, err := adapterCommand(ctx, bunPath, cmdArgs, sourceRoot, installEnv, imageRoot, runtimeUser, imageMode)
	if err != nil {
		return fmt.Errorf("prepare task project dependency install: %w", err)
	}
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

func adapterInstallEnv(runtimeSourceRoot string, hostSourceRoot string, env []string, runtimeUser *resolvedRuntimeUser) ([]string, error) {
	runtimeStateRoot := pathpkg.Join(runtimeSourceRoot, ".helmr")
	hostStateRoot := filepath.Join(hostSourceRoot, ".helmr")
	paths := []struct {
		envKey      string
		runtimePath string
		hostPath    string
	}{
		{envKey: "HOME", runtimePath: pathpkg.Join(runtimeStateRoot, "home"), hostPath: filepath.Join(hostStateRoot, "home")},
		{envKey: "XDG_CACHE_HOME", runtimePath: pathpkg.Join(runtimeStateRoot, "cache"), hostPath: filepath.Join(hostStateRoot, "cache")},
		{envKey: "npm_config_cache", runtimePath: pathpkg.Join(runtimeStateRoot, "cache", "npm"), hostPath: filepath.Join(hostStateRoot, "cache", "npm")},
		{envKey: "TMPDIR", runtimePath: pathpkg.Join(runtimeStateRoot, "tmp"), hostPath: filepath.Join(hostStateRoot, "tmp")},
	}
	for _, path := range paths {
		if err := os.MkdirAll(path.hostPath, 0o755); err != nil {
			return nil, fmt.Errorf("create adapter install %s: %w", path.envKey, err)
		}
		if runtimeUser != nil {
			if err := chownTree(path.hostPath, runtimeUser.UID, runtimeUser.GID); err != nil {
				return nil, fmt.Errorf("prepare adapter install %s owner: %w", path.envKey, err)
			}
		}
		env = setEnvValue(env, path.envKey, path.runtimePath)
	}
	env = setEnvDefault(env, "PYTHON", "/usr/bin/python3")
	return env, nil
}

func validateAdapterSourcePackageJSON(sourceRoot string) error {
	packagePath := filepath.Join(sourceRoot, "package.json")
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

func lockfileExists(sourceRoot string) bool {
	for _, name := range []string{"bun.lock", "bun.lockb"} {
		metadata, err := os.Stat(filepath.Join(sourceRoot, name))
		if err == nil && !metadata.IsDir() {
			return true
		}
	}
	return false
}

func runtimeSourceHostPath(imageRoot string, runtimePath string, imageMode bool) (string, error) {
	if !imageMode {
		return runtimePath, nil
	}
	return safeJoin(imageRoot, strings.TrimPrefix(runtimePath, "/"))
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

func runAdapter(ctx context.Context, conn io.ReadWriter, cfg Config, imageRoot string, deploymentSourceRoot string, workspaceRoot string, runCwd string, imageConfig ociRuntimeConfig, imageMode bool, request *runv0.RunTaskRequest, registry *waitingRunRegistry) error {
	stdoutWriter := eventWriter{conn: conn}
	bunPath := cfg.BunPath
	var bunPrefixArgs []string
	adapterPath := cfg.AdapterPath
	taskAdapterCwd := deploymentSourceRoot
	if imageMode {
		if err := installRuntimeBundle(cfg.RuntimePath, imageRoot); err != nil {
			return writeRunSetupFailure(conn, err)
		}
		adapterPath = "/opt/helmr/adapter/main.js"
		var err error
		bunPath, bunPrefixArgs, err = bundledRuntimeCommand(imageRoot)
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
	if err := prepareAdapterSourceWithRuntime(ctx, bunPath, bunPrefixArgs, taskAdapterCwd, taskAdapterSourceRoot, cmdEnv, imageRoot, runtimeUser, imageMode); err != nil {
		return writeRunSetupFailure(conn, err)
	}
	cmdArgs := append(append([]string{}, bunPrefixArgs...), adapterPath,
		"run",
		"--cwd", runCwd,
		"--task-cwd", taskAdapterCwd,
		"--task", request.TaskId,
		"--run-id", request.RunId,
		"--payload-json", request.PayloadJson,
	)
	cmd, err := adapterCommand(ctx, bunPath, cmdArgs, launchCwd, cmdEnv, imageRoot, runtimeUser, imageMode)
	if err != nil {
		return writeRunSetupFailure(conn, err)
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	defer controlReader.Close()
	cmd.ExtraFiles = []*os.File{controlWriter}
	if err := cmd.Start(); err != nil {
		_ = controlWriter.Close()
		_ = stdin.Close()
		return writeRunSetupFailure(conn, err)
	}
	_ = controlWriter.Close()

	var wg sync.WaitGroup
	controlErrCh := make(chan error, 1)
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
		_ = forwardChunks(stdout, func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: chunk}}
		}, &stdoutWriter)
	}()
	go func() {
		defer wg.Done()
		_ = forwardChunks(stderr, func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{Event: &runv0.RunEvent_StderrChunk{StderrChunk: chunk}}
		}, &stdoutWriter)
	}()
	go func() {
		defer wg.Done()
		defer stdin.Close()
		for {
			event, err := transport.ReadRunEvent(controlReader)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					recordControlErr(fmt.Errorf("read adapter control event: %w", err))
				}
				return
			}
			if wait := event.GetWaitRequested(); wait != nil {
				if err := stdoutWriter.write(event); err != nil {
					recordControlErr(fmt.Errorf("write wait request event: %w", err))
					return
				}
				var suspend runv0.SuspendForCheckpoint
				if err := transport.ReadProtoFrame(conn, &suspend); err != nil {
					recordControlErr(fmt.Errorf("read checkpoint suspend request: %w", err))
					return
				}
				registration := registry.register(suspend.WaitpointId, suspend.CheckpointId)
				if err := stdoutWriter.writeProto(&runv0.PauseReady{
					WaitpointId:  suspend.WaitpointId,
					CheckpointId: suspend.CheckpointId,
				}); err != nil {
					registration.unregister()
					recordControlErr(fmt.Errorf("write checkpoint pause ready: %w", err))
					return
				}
				attachCtx, cancelAttach := context.WithTimeout(ctx, resumeAttachTimeout)
				attached, err := registration.wait(attachCtx)
				cancelAttach()
				registration.unregister()
				if err != nil {
					recordControlErr(fmt.Errorf("wait for resume attach: %w", err))
					return
				}
				decisionCtx, cancelDecision := context.WithTimeout(ctx, resumeAttachTimeout)
				decision, err := readResumeDecision(decisionCtx, attached)
				cancelDecision()
				if err != nil {
					recordControlErr(fmt.Errorf("read resume decision: %w", err))
					return
				}
				if err := stdoutWriter.resumeOn(attached, stdin, decision); err != nil {
					recordControlErr(fmt.Errorf("resume adapter stream: %w", err))
					return
				}
				continue
			}
			if err := stdoutWriter.write(event); err != nil {
				recordControlErr(fmt.Errorf("write adapter control event: %w", err))
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	_ = stdin.Close()
	wg.Wait()
	var controlErr error
	select {
	case controlErr = <-controlErrCh:
	default:
	}
	exitCode := int32(0)
	var message string
	if controlErr != nil {
		exitCode = 1
		message = controlErr.Error()
	} else if waitErr != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		}
	}
	return stdoutWriter.writeComplete(exitCode, message)
}

func writeRunSetupFailure(conn io.Writer, err error) error {
	message := "guest runtime setup failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	writer := eventWriter{conn: conn}
	if writeErr := writer.write(&runv0.RunEvent{Event: &runv0.RunEvent_StderrChunk{StderrChunk: []byte(message + "\n")}}); writeErr != nil {
		return writeErr
	}
	return writer.writeComplete(1, message)
}

type eventWriter struct {
	mu   sync.Mutex
	conn io.Writer
}

func (w *eventWriter) write(event *runv0.RunEvent) error {
	return w.writeProto(event)
}

func (w *eventWriter) writeProto(message proto.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return transport.WriteProtoFrame(w.conn, message)
}

func (w *eventWriter) resumeOn(conn io.Writer, stdin io.Writer, decision *runv0.ResumeDecision) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conn = conn
	if err := transport.WriteProtoFrame(stdin, decision); err != nil {
		return err
	}
	return transport.WriteProtoFrame(w.conn, &runv0.ResumeAck{WaitpointId: decision.WaitpointId})
}

func (w *eventWriter) writeComplete(exitCode int32, message string) error {
	complete := &runv0.TaskComplete{ExitCode: exitCode}
	if message != "" {
		complete.ErrorMessage = &message
	}
	return w.write(&runv0.RunEvent{Event: &runv0.RunEvent_TaskComplete{TaskComplete: complete}})
}

func forwardChunks(r io.Reader, event func([]byte) *runv0.RunEvent, writer *eventWriter) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if writeErr := writer.write(event(chunk)); writeErr != nil {
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
