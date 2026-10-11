package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

type computerBasicExec struct {
	cancel       context.CancelFunc
	acknowledged bool
	envelope     *computerv0.ComputerCommandAuthority
	done         chan struct{}
	result       *computerv0.ComputerBasicExecResult
	output       *commandOutputSpool
}

type computerBasicExecSpec struct {
	Command   []string          `json:"command"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	TimeoutMS int64             `json:"timeout_ms"`
}

func (r *computerOperationRegistry) runComputerBasicExec(ctx context.Context, entry *computerMountEntry, request *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
	execution, code, err := r.startComputerBasicExec(ctx, entry, request)
	if err != nil {
		return computerBasicCommandFailure(request.GetEnvelope().GetRequestFingerprint(), code, err)
	}
	select {
	case <-execution.done:
		return execution.result
	case <-ctx.Done():
		return computerBasicCommandFailure(request.GetEnvelope().GetRequestFingerprint(), "computer_command_result_uncertain", ctx.Err())
	}
}

// The lifecycle/finalization locks serialize transfer, reservation, Program admission
// and stop. The registry mutex is never held over journal IO or process execution.
func (r *computerOperationRegistry) startComputerBasicExec(ctx context.Context, entry *computerMountEntry, request *computerv0.ComputerBasicExecRequest) (*computerBasicExec, string, error) {
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if r.captureSealed() {
		return nil, "computer_command_unavailable", errors.New("computer capture has sealed command admission")
	}
	envelope := request.GetEnvelope()
	if err := validateComputerBasicExecClaim(ctx, request, entry.authorityNow()); err != nil {
		return nil, "computer_command_fenced", err
	}
	if !r.currentMountLocked(entry, entry.computerInstanceID, envelope.GetComputerId(), envelope.GetChannelCredential()) || entry.computerInstanceID != envelope.GetComputerInstanceId() || entry.writerGeneration != envelope.GetWriterGeneration() {
		return nil, "computer_command_fenced", errors.New("command does not match the live Computer instance")
	}
	entry.processesMu.Lock()
	unavailable := entry.recoveryRequired
	entry.processesMu.Unlock()
	if unavailable || entry.stopping {
		return nil, "computer_command_unavailable", errors.New("computer is not accepting Commands")
	}
	if execution := entry.commands[envelope.GetOperationId()]; execution != nil {
		if execution.acknowledged {
			return nil, "computer_command_unavailable", errors.New("command result has been released")
		}
		if execution.envelope.GetRequestFingerprint() != envelope.GetRequestFingerprint() {
			return nil, "computer_command_fingerprint_conflict", errors.New("command fingerprint changed")
		}
		fixed := proto.Clone(envelope).(*computerv0.ComputerCommandAuthority)
		fixed.OperationExpiresAtUnixNano = execution.envelope.GetOperationExpiresAtUnixNano()
		if !proto.Equal(fixed, execution.envelope) {
			return nil, "computer_command_fenced", errors.New("command replay authority changed")
		}
		return execution, "", nil
	}
	if request.GetTailOnly() {
		return nil, "computer_command_unavailable", errors.New("retained command output is unavailable")
	}
	output, err := newCommandOutputSpool(diagnosticLimits{ChunkBytes: int(request.GetLogLimits().GetChunkBytes()), BufferBytes: request.GetLogLimits().GetBufferBytes(), BufferRecords: int(request.GetLogLimits().GetBufferRecords())})
	if err != nil {
		return nil, "computer_command_invalid", err
	}
	if err := validateComputerBasicExecClaim(ctx, request, entry.authorityNow()); err != nil {
		output.close()
		return nil, "computer_command_fenced", err
	}
	entry.processesMu.Lock()
	entry.processAdmissions++
	entry.processesMu.Unlock()
	executionCtx, cancel := context.WithCancel(context.Background())
	execution := &computerBasicExec{cancel: cancel, envelope: proto.Clone(envelope).(*computerv0.ComputerCommandAuthority), done: make(chan struct{}), output: output}
	if entry.commands == nil {
		entry.commands = make(map[string]*computerBasicExec)
	}
	entry.commands[envelope.GetOperationId()] = execution
	// Keep the image alive even if the transport disconnects during execution.
	r.mu.Lock()
	entry.active++
	cleanup := entry.cleanup
	entry.cleanup = func() {
		output.close()
		if cleanup != nil {
			cleanup()
		}
	}
	r.mu.Unlock()
	requestCopy := proto.Clone(request).(*computerv0.ComputerBasicExecRequest)
	go func() {
		defer r.release(entry)
		defer cancel()
		execution.result = entry.executeBasicExec(executionCtx, requestCopy, output)
		output.finish(execution.result.GetOutcome() != "computer_command_output_capture_failed" && execution.result.GetOutcome() != "computer_command_scope_termination_failed")
		execution.result.Stdout = output.boundaries["stdout"]
		execution.result.Stderr = output.boundaries["stderr"]
		clearComputerBasicExecRequest(requestCopy)
		entry.processesMu.Lock()
		entry.processAdmissions--
		entry.processesMu.Unlock()
		close(execution.done)
	}()
	return execution, "", nil
}

func validateComputerBasicExecClaim(ctx context.Context, request *computerv0.ComputerBasicExecRequest, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := request.GetEnvelope()
	if strings.TrimSpace(e.GetOperationId()) == "" || strings.TrimSpace(e.GetRequestFingerprint()) == "" || strings.TrimSpace(e.GetComputerInstanceId()) == "" || strings.TrimSpace(e.GetComputerId()) == "" || strings.TrimSpace(e.GetChannelCredential()) == "" || e.GetWriterGeneration() <= 0 {
		return errors.New("command authority is incomplete")
	}
	if now.UnixNano() >= e.GetOperationExpiresAtUnixNano() {
		return errors.New("computer exec claim expired")
	}
	return nil
}

func (entry *computerMountEntry) executeBasicExec(
	ctx context.Context,
	request *computerv0.ComputerBasicExecRequest,
	output *commandOutputSpool,
) *computerv0.ComputerBasicExecResult {
	if entry.basicExecRun != nil {
		return entry.basicExecRun(ctx, request)
	}
	return entry.executeComputerBasicExec(ctx, request, output)
}

func (entry *computerMountEntry) executeComputerBasicExec(
	ctx context.Context,
	request *computerv0.ComputerBasicExecRequest,
	output *commandOutputSpool,
) *computerv0.ComputerBasicExecResult {
	fingerprint := request.GetEnvelope().GetRequestFingerprint()
	var spec computerBasicExecSpec
	decoder := json.NewDecoder(strings.NewReader(request.GetRequestJson()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_invalid", err)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" ||
		spec.TimeoutMS <= 0 || spec.TimeoutMS > int64((15*time.Minute)/time.Millisecond) {
		return computerBasicCommandFailure(fingerprint, "computer_command_invalid", errors.New("computer exec request is invalid"))
	}
	launchCwd, err := entry.computerLaunchCwd(spec.Cwd)
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_invalid", err)
	}
	env, err := entry.computerProcessEnv(launchCwd, spec.Env)
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_invalid", err)
	}
	secrets, err := computerBasicExecSecrets(request.GetSecrets())
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_secret_delivery_failed", err)
	}
	secretRoot, secretCleanup, err := stageProgramSecrets(entry.imageRoot, secrets, entry.runtimeUser, &env)
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_secret_delivery_failed", err)
	}
	defer secretCleanup()
	if err := stageProtectedEnv(entry.imageRoot, request.GetProtectedEnv(), request.GetProxyCa(), &env); err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_secret_delivery_failed", err)
	}
	runtimePath, err := entry.computerRuntimePath(spec.Command[0], launchCwd, env)
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_launch_failed", err)
	}
	if err := prepareLaunchPath(entry.imageRoot, launchCwd, entry.runtimeUser); err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_launch_failed", err)
	}
	if err := entry.prepareComputerOwner(); err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_launch_failed", err)
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()
	leaf, err := execCgroupLeafName(request.GetEnvelope().GetOperationId())
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_invalid", err)
	}
	options := computerBasicExecImageCommandOptions(leaf)
	options.SecretRoot = secretRoot
	cmd, err := imageCommand(
		execCtx,
		runtimePath,
		spec.Command[1:],
		launchCwd,
		env,
		entry.imageRoot,
		entry.runtimeUser,
		options,
	)
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_launch_failed", err)
	}
	var outputMu sync.Mutex
	var outputErr error
	onOutputError := func(err error) {
		outputMu.Lock()
		outputErr = errors.Join(outputErr, err)
		outputMu.Unlock()
		cancel()
	}
	stdout := &commandOutputWriter{spool: output, stream: "stdout", onError: onOutputError}
	stderr := &commandOutputWriter{spool: output, stream: "stderr", onError: onOutputError}
	cmd.Stdin = bytes.NewReader(request.GetStdin())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	scope, err := createProcessCgroup(leaf)
	if err != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_launch_failed", err)
	}
	var cleanupErr error
	err, cleanupErr = runScopedCommand(cmd, scope, nil)
	if cleanupErr != nil {
		entry.processesMu.Lock()
		entry.recoveryRequired = true
		entry.processesMu.Unlock()
		return computerBasicCommandFailure(fingerprint, "computer_command_scope_termination_failed", cleanupErr)
	}
	outputMu.Lock()
	captureErr := outputErr
	outputMu.Unlock()
	if captureErr != nil {
		return computerBasicCommandFailure(fingerprint, "computer_command_output_capture_failed", captureErr)
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return computerBasicCommandFailure(
			fingerprint,
			"computer_command_timed_out",
			execCtx.Err(),
		)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return computerBasicCommandFailure(fingerprint, "computer_command_cancelled", ctx.Err())
	}
	exitCode := int32(0)
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return computerBasicCommandFailure(
				fingerprint,
				"computer_command_launch_failed",
				err,
			)
		}
		exitCode = int32(exitError.ExitCode())
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return computerBasicCommandFailure(
				fingerprint,
				"computer_command_signaled",
				fmt.Errorf("computer exec terminated by %s", status.Signal()),
			)
		}
	}
	return &computerv0.ComputerBasicExecResult{
		ExitCode:           exitCode,
		Outcome:            "exited",
		RequestFingerprint: fingerprint,
	}
}

func computerBasicExecImageCommandOptions(leaf string) imageCommandOptions {
	// Direct computer exec is computer-tool authority. It must not mount the
	// program artifact or managed runtime drives into the image namespace.
	return imageCommandOptions{CgroupNamespace: true, CgroupLeaf: leaf}
}

func computerBasicExecSecrets(
	deliveries []*computerv0.ComputerSecretDelivery,
) ([]*agentv1.SessionSecret, error) {
	secrets := make([]*agentv1.SessionSecret, 0, len(deliveries))
	for _, delivery := range deliveries {
		secret := &agentv1.SessionSecret{Value: bytes.Clone(delivery.GetValue())}
		switch delivery.GetPlacementKind() {
		case "env":
			secret.Placement = &agentv1.SessionSecret_Env{Env: delivery.GetPlacementTarget()}
		case "file":
			secret.Placement = &agentv1.SessionSecret_File{File: delivery.GetPlacementTarget()}
		default:
			clear(secret.Value)
			clearProgramSecretValues(secrets)
			return nil, fmt.Errorf("unsupported computer secret placement %q", delivery.GetPlacementKind())
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

func computerBasicCommandFailure(
	fingerprint string,
	code string,
	err error,
) *computerv0.ComputerBasicExecResult {
	message := code
	if err != nil {
		message = err.Error()
	}
	errorJSON, marshalErr := json.Marshal(map[string]string{"code": code, "message": message})
	if marshalErr != nil {
		errorJSON = []byte(`{"code":"computer_command_failed"}`)
	}
	return &computerv0.ComputerBasicExecResult{
		ErrorJson:          string(errorJSON),
		Outcome:            code,
		RequestFingerprint: fingerprint,
	}
}

func clearComputerBasicExecRequest(request *computerv0.ComputerBasicExecRequest) {
	clear(request.Stdin)
	request.Stdin = nil
	for _, delivery := range request.Secrets {
		clear(delivery.Value)
		delivery.Value = nil
	}
	request.Secrets = nil
}

func (entry *computerMountEntry) computerLaunchCwd(raw string) (string, error) {
	return resolveLaunchCwd(raw, entry.computerMount)
}

func (entry *computerMountEntry) computerProcessEnv(
	launchCwd string,
	userEnv map[string]string,
) ([]string, error) {
	if entry.runtimeUser == nil {
		return nil, errors.New("computer runtime user is not resolved")
	}
	env := imageRuntimeEnv(entry.imageConfig, entry.runtimeUser, launchCwd)
	for key, value := range userEnv {
		if strings.Contains(key, "\x00") || strings.Contains(value, "\x00") {
			return nil, fmt.Errorf("env %q contains NUL", key)
		}
		env = setEnvValue(env, key, value)
	}
	return env, nil
}

func (entry *computerMountEntry) prepareComputerOwner() error {
	if entry.runtimeUser == nil || os.Geteuid() != 0 {
		return nil
	}
	if err := chownTree(
		entry.computerRoot,
		entry.runtimeUser.UID,
		entry.runtimeUser.GID,
	); err != nil {
		return fmt.Errorf("prepare computer owner: %w", err)
	}
	return nil
}

func (entry *computerMountEntry) computerRuntimePath(
	command string,
	launchCwd string,
	env []string,
) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("command is required")
	}
	if strings.Contains(command, "\x00") {
		return "", errors.New("command contains NUL")
	}
	if strings.Contains(command, "/") {
		if strings.HasPrefix(command, "/") {
			return path.Clean(command), nil
		}
		return path.Clean(path.Join(launchCwd, command)), nil
	}
	searchPath := computerEnvValue(env, "PATH")
	if strings.TrimSpace(searchPath) == "" {
		searchPath = defaultRuntimePath
	}
	for dir := range strings.SplitSeq(searchPath, ":") {
		if dir == "" {
			dir = "."
		}
		candidate := path.Clean(path.Join(dir, command))
		if !strings.HasPrefix(candidate, "/") {
			candidate = path.Clean(path.Join(launchCwd, candidate))
		}
		hostPath, err := confinedLayerPath(
			entry.imageRoot,
			strings.TrimPrefix(candidate, "/"),
		)
		if err != nil {
			continue
		}
		if isExecutableFile(hostPath) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("command %q not found in image PATH", command)
}

func computerEnvValue(env []string, key string) string {
	for _, entry := range env {
		entryKey, value, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			return value
		}
	}
	return ""
}

func isExecutableFile(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}
