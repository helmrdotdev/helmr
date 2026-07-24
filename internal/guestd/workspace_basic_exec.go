package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

const workspaceBasicExecOutputLimit = 4 << 20

type workspaceBasicExec struct {
	fingerprint string
	done        chan struct{}
	result      *workspacev0.WorkspaceOperationResult
}

type workspaceBasicExecSpec struct {
	Command   []string          `json:"command"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	TimeoutMS int64             `json:"timeout_ms"`
}

type workspaceBoundedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	overflow func()
}

func (b *workspaceBoundedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buffer.Len()+len(value) > b.limit {
		if b.overflow != nil {
			b.overflow()
		}
		return 0, errors.New("workspace exec output limit exceeded")
	}
	return b.buffer.Write(value)
}

func (b *workspaceBoundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes())
}

func (entry *workspaceMountEntry) runWorkspaceBasicExec(
	ctx context.Context,
	request *workspacev0.WorkspaceOperationRequest,
) *workspacev0.WorkspaceOperationResult {
	envelope := request.GetEnvelope()
	processID := strings.TrimSpace(envelope.GetOperationId())
	fingerprint := strings.TrimSpace(envelope.GetRequestFingerprint())
	if processID == "" || fingerprint == "" {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", errors.New("Workspace exec identity is required"))
	}

	entry.basicExecMu.Lock()
	if entry.basicExecs == nil {
		entry.basicExecs = make(map[string]*workspaceBasicExec)
	}
	execution := entry.basicExecs[processID]
	if execution != nil {
		if execution.fingerprint != fingerprint {
			entry.basicExecMu.Unlock()
			return workspaceBasicExecFailure(fingerprint, "workspace_exec_fingerprint_conflict", errors.New("Workspace exec fingerprint changed"))
		}
		entry.basicExecMu.Unlock()
		select {
		case <-execution.done:
			return execution.result
		case <-ctx.Done():
			return workspaceBasicExecFailure(fingerprint, "workspace_exec_result_uncertain", ctx.Err())
		}
	}
	execution = &workspaceBasicExec{fingerprint: fingerprint, done: make(chan struct{})}
	entry.basicExecs[processID] = execution
	entry.basicExecMu.Unlock()

	requestCopy := &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:        processID,
			RequestFingerprint: fingerprint,
		},
		RequestJson: request.GetRequestJson(),
		Stdin:       bytes.Clone(request.GetStdin()),
	}
	for _, delivery := range request.GetSecrets() {
		requestCopy.Secrets = append(requestCopy.Secrets, &workspacev0.WorkspaceSecretDelivery{
			PlacementKind:   delivery.GetPlacementKind(),
			PlacementTarget: delivery.GetPlacementTarget(),
			Value:           bytes.Clone(delivery.GetValue()),
		})
	}
	go func() {
		execution.result = entry.executeBasicExec(requestCopy)
		clearWorkspaceBasicExecRequest(requestCopy)
		close(execution.done)
	}()

	select {
	case <-execution.done:
		return execution.result
	case <-ctx.Done():
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_result_uncertain", ctx.Err())
	}
}

func (entry *workspaceMountEntry) executeBasicExec(
	request *workspacev0.WorkspaceOperationRequest,
) *workspacev0.WorkspaceOperationResult {
	if entry.basicExecRun != nil {
		return entry.basicExecRun(request)
	}
	return entry.executeWorkspaceBasicExec(request)
}

func (entry *workspaceMountEntry) executeWorkspaceBasicExec(
	request *workspacev0.WorkspaceOperationRequest,
) *workspacev0.WorkspaceOperationResult {
	fingerprint := request.GetEnvelope().GetRequestFingerprint()
	var spec workspaceBasicExecSpec
	decoder := json.NewDecoder(strings.NewReader(request.GetRequestJson()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", err)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" ||
		spec.TimeoutMS <= 0 || spec.TimeoutMS > int64((15*time.Minute)/time.Millisecond) {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", errors.New("Workspace exec request is invalid"))
	}
	launchCwd, err := entry.workspaceLaunchCwd(spec.Cwd)
	if err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", err)
	}
	env, err := entry.workspaceProcessEnv(launchCwd, spec.Env)
	if err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", err)
	}
	secrets, err := workspaceBasicExecSecrets(request.GetSecrets())
	if err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_secret_delivery_failed", err)
	}
	secretCleanup, err := stageProgramSecrets(entry.imageRoot, secrets, entry.runtimeUser, &env)
	if err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_secret_delivery_failed", err)
	}
	defer secretCleanup()
	runtimePath, err := entry.workspaceRuntimePath(spec.Command[0], launchCwd, env)
	if err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_launch_failed", err)
	}
	if err := prepareLaunchPath(entry.imageRoot, launchCwd, entry.runtimeUser); err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_launch_failed", err)
	}
	if err := entry.prepareWorkspaceOwner(); err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_launch_failed", err)
	}

	execCtx, cancel := context.WithTimeout(context.Background(), time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()
	cmd, err := imageCommand(
		execCtx,
		runtimePath,
		spec.Command[1:],
		launchCwd,
		env,
		entry.imageRoot,
		entry.runtimeUser,
		imageCommandOptions{ManagedProgram: true},
	)
	if err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_launch_failed", err)
	}
	var overflowMu sync.Mutex
	overflowed := false
	markOverflow := func() {
		overflowMu.Lock()
		overflowed = true
		overflowMu.Unlock()
		cancel()
	}
	stdout := &workspaceBoundedBuffer{limit: workspaceBasicExecOutputLimit, overflow: markOverflow}
	stderr := &workspaceBoundedBuffer{limit: workspaceBasicExecOutputLimit, overflow: markOverflow}
	cmd.Stdin = bytes.NewReader(request.GetStdin())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	overflowMu.Lock()
	outputOverflow := overflowed
	overflowMu.Unlock()
	if outputOverflow {
		return workspaceBasicExecFailureWithOutput(
			fingerprint,
			"workspace_exec_output_limit_exceeded",
			errors.New("Workspace exec output limit exceeded"),
			stdout.Bytes(),
			stderr.Bytes(),
		)
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return workspaceBasicExecFailureWithOutput(
			fingerprint,
			"workspace_exec_timed_out",
			execCtx.Err(),
			stdout.Bytes(),
			stderr.Bytes(),
		)
	}
	exitCode := int32(0)
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return workspaceBasicExecFailureWithOutput(
				fingerprint,
				"workspace_exec_launch_failed",
				err,
				stdout.Bytes(),
				stderr.Bytes(),
			)
		}
		exitCode = int32(exitError.ExitCode())
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return workspaceBasicExecFailureWithOutput(
				fingerprint,
				"workspace_exec_signaled",
				fmt.Errorf("Workspace exec terminated by %s", status.Signal()),
				stdout.Bytes(),
				stderr.Bytes(),
			)
		}
	}
	return &workspacev0.WorkspaceOperationResult{
		ExitCode:           exitCode,
		Stdout:             stdout.Bytes(),
		Stderr:             stderr.Bytes(),
		Outcome:            "exited",
		RequestFingerprint: fingerprint,
	}
}

func workspaceBasicExecSecrets(
	deliveries []*workspacev0.WorkspaceSecretDelivery,
) ([]*runv0.ProgramSecret, error) {
	secrets := make([]*runv0.ProgramSecret, 0, len(deliveries))
	for _, delivery := range deliveries {
		secret := &runv0.ProgramSecret{Value: bytes.Clone(delivery.GetValue())}
		switch delivery.GetPlacementKind() {
		case "env":
			secret.Placement = &runv0.ProgramSecret_Env{Env: delivery.GetPlacementTarget()}
		case "file":
			secret.Placement = &runv0.ProgramSecret_File{File: delivery.GetPlacementTarget()}
		default:
			clear(secret.Value)
			clearProgramSecretValues(secrets)
			return nil, fmt.Errorf("unsupported Workspace Secret placement %q", delivery.GetPlacementKind())
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

func workspaceBasicExecFailure(
	fingerprint string,
	code string,
	err error,
) *workspacev0.WorkspaceOperationResult {
	return workspaceBasicExecFailureWithOutput(fingerprint, code, err, nil, nil)
}

func workspaceBasicExecFailureWithOutput(
	fingerprint string,
	code string,
	err error,
	stdout []byte,
	stderr []byte,
) *workspacev0.WorkspaceOperationResult {
	message := code
	if err != nil {
		message = err.Error()
	}
	errorJSON, marshalErr := json.Marshal(map[string]string{"code": code, "message": message})
	if marshalErr != nil {
		errorJSON = []byte(`{"code":"workspace_exec_failed"}`)
	}
	return &workspacev0.WorkspaceOperationResult{
		ErrorJson:          string(errorJSON),
		Stdout:             stdout,
		Stderr:             stderr,
		Outcome:            code,
		RequestFingerprint: fingerprint,
	}
}

func clearWorkspaceBasicExecRequest(request *workspacev0.WorkspaceOperationRequest) {
	clear(request.Stdin)
	request.Stdin = nil
	for _, delivery := range request.Secrets {
		clear(delivery.Value)
		delivery.Value = nil
	}
	request.Secrets = nil
}
