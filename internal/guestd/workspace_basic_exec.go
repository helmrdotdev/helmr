package guestd

import (
	"bytes"
	"context"
	"crypto/subtle"
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

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"google.golang.org/protobuf/proto"
)

const workspaceBasicExecOutputLimit = 4 << 20

type workspaceBasicExec struct {
	envelope            *workspacev0.WorkspaceOperationEnvelope
	baseVersionID       string
	ownershipGeneration int64
	writerGeneration    int64
	done                chan struct{}
	result              *workspacev0.WorkspaceBasicExecResult
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

func (r *workspaceOperationRegistry) runWorkspaceBasicExec(ctx context.Context, entry *workspaceMountEntry, request *workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
	execution, code, err := r.startWorkspaceBasicExec(ctx, entry, request)
	if err != nil {
		return workspaceBasicExecFailure(request.GetEnvelope().GetRequestFingerprint(), code, err)
	}
	select {
	case <-execution.done:
		return execution.result
	case <-ctx.Done():
		return workspaceBasicExecFailure(request.GetEnvelope().GetRequestFingerprint(), "workspace_exec_result_uncertain", ctx.Err())
	}
}

// The turn/finalization locks serialize transfer, reservation, Program admission
// and stop. The registry mutex is never held over journal IO or process execution.
func (r *workspaceOperationRegistry) startWorkspaceBasicExec(ctx context.Context, entry *workspaceMountEntry, request *workspacev0.WorkspaceBasicExecRequest) (*workspaceBasicExec, string, error) {
	entry.turnCommitMu.Lock()
	defer entry.turnCommitMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	envelope := request.GetEnvelope()
	if err := validateWorkspaceBasicExecClaim(ctx, request); err != nil {
		return nil, "workspace_exec_fenced", err
	}
	if !r.currentMountLocked(entry, envelope.GetWorkspaceMountId(), envelope.GetWorkspaceId(), envelope.GetChannelToken()) {
		return nil, "workspace_exec_fenced", errors.New("workspace exec mount is not current")
	}
	entry.processesMu.Lock()
	unavailable := entry.recoveryRequired || entry.turnCommitBlocked
	entry.processesMu.Unlock()
	if unavailable {
		return nil, "workspace_exec_unavailable", errors.New("workspace requires recovery or has a blocked commit")
	}
	if execution := entry.basicExec; execution != nil {
		if execution.envelope.GetOperationId() != envelope.GetOperationId() {
			return nil, "workspace_exec_unavailable", errors.New("workspace mount already served another exec operation")
		}
		if execution.envelope.GetRequestFingerprint() != envelope.GetRequestFingerprint() {
			return nil, "workspace_exec_fingerprint_conflict", errors.New("workspace exec fingerprint changed")
		}
		// Lease heartbeat may renew expiry; it cannot change any fixed owner coordinate.
		fixed := proto.Clone(envelope).(*workspacev0.WorkspaceOperationEnvelope)
		fixed.OperationExpiresAtUnixNano = execution.envelope.GetOperationExpiresAtUnixNano()
		if subtle.ConstantTimeCompare([]byte(envelope.GetFencingToken()), []byte(execution.envelope.GetFencingToken())) != 1 ||
			!proto.Equal(fixed, execution.envelope) ||
			request.GetBaseWorkspaceVersionId() != execution.baseVersionID ||
			request.GetOwnershipGeneration() != execution.ownershipGeneration ||
			request.GetWriterGeneration() != execution.writerGeneration ||
			entry.currentFencingGeneration() != envelope.GetFencingGeneration() {
			return nil, "workspace_exec_fenced", errors.New("workspace exec replay authority changed")
		}
		return execution, "", nil
	}
	if entry.stopping {
		return nil, "workspace_exec_unavailable", errors.New("workspace mount is stopping")
	}
	r.mu.Lock()
	hasProgram := r.hasProgramClaimLocked(entry)
	r.mu.Unlock()
	entry.processesMu.Lock()
	active := entry.processAdmissions != 0
	finalizing := entry.authorityState == workspaceAuthorityFinalizing
	entry.processesMu.Unlock()
	if hasProgram || active {
		return nil, "workspace_exec_unavailable", errors.New("workspace has active work")
	}
	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	if entry.authority == nil {
		if finalizing || request.GetBaseWorkspaceVersionId() != entry.baseVersionID || envelope.GetFencingGeneration() < entry.currentFencingGeneration() {
			return nil, "workspace_exec_fenced", errors.New("workspace exec does not match the mounted frontier")
		}
	} else {
		prior := entry.authority.GetFence()
		if !finalizing || request.GetOwnershipGeneration() <= prior.GetOwnershipGeneration() || request.GetWriterGeneration() <= prior.GetWriterGeneration() || envelope.GetFencingGeneration() <= entry.currentFencingGeneration() {
			return nil, "workspace_exec_fenced", errors.New("workspace exec does not advance committed Run authority")
		}
		journal, found, err := entry.readWorkspaceFinalizationJournal()
		if err != nil {
			return nil, "workspace_exec_fenced", err
		}
		if !found || journal.Phase != "committed" || validateWorkspaceFinalizationBeginJournal(journal, entry.finalizationID, entry.finalizationKind, workspaceFinalizationFence(prior)) != nil {
			return nil, "workspace_exec_fenced", errors.New("workspace finalization is not committed")
		}
		if err := validateWorkspaceBasicExecClaim(ctx, request); err != nil {
			return nil, "workspace_exec_fenced", err
		}
		if err := entry.pruneWorkspaceFinalizationState(); err != nil {
			entry.processesMu.Lock()
			entry.recoveryRequired = true
			entry.processesMu.Unlock()
			return nil, "workspace_exec_fenced", err
		}
	}
	// Pruning can block or partially mutate disk. A failed post-IO check must
	// fence the consumed predecessor rather than make it available for admission.
	if err := validateWorkspaceBasicExecClaim(ctx, request); err != nil {
		if entry.authority != nil {
			entry.processesMu.Lock()
			entry.recoveryRequired = true
			entry.processesMu.Unlock()
		}
		return nil, "workspace_exec_fenced", err
	}

	entry.baseVersionID = request.GetBaseWorkspaceVersionId()
	entry.authority = nil
	entry.previousExpiry = 0
	entry.setFencingGeneration(envelope.GetFencingGeneration())
	entry.processesMu.Lock()
	entry.authorityState = workspaceAuthorityLive
	entry.finalizationID = ""
	entry.finalizationKind = ""
	entry.processAdmissions++
	entry.processesMu.Unlock()
	execution := &workspaceBasicExec{
		envelope:      proto.Clone(envelope).(*workspacev0.WorkspaceOperationEnvelope),
		baseVersionID: request.GetBaseWorkspaceVersionId(), ownershipGeneration: request.GetOwnershipGeneration(), writerGeneration: request.GetWriterGeneration(), done: make(chan struct{}),
	}
	entry.basicExec = execution
	// Keep the image alive even if the transport disconnects during execution.
	r.mu.Lock()
	entry.active++
	r.mu.Unlock()
	requestCopy := proto.Clone(request).(*workspacev0.WorkspaceBasicExecRequest)
	go func() {
		defer r.release(entry)
		execution.result = entry.executeBasicExec(requestCopy)
		clearWorkspaceBasicExecRequest(requestCopy)
		entry.processesMu.Lock()
		entry.processAdmissions--
		entry.processesMu.Unlock()
		close(execution.done)
	}()
	return execution, "", nil
}

func validateWorkspaceBasicExecClaim(ctx context.Context, request *workspacev0.WorkspaceBasicExecRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := request.GetEnvelope()
	if strings.TrimSpace(e.GetOperationId()) == "" ||
		strings.TrimSpace(e.GetRequestFingerprint()) == "" ||
		strings.TrimSpace(e.GetWorkspaceMountId()) == "" ||
		strings.TrimSpace(e.GetWorkspaceId()) == "" ||
		strings.TrimSpace(e.GetChannelToken()) == "" ||
		strings.TrimSpace(e.GetInstanceLeaseId()) == "" ||
		strings.TrimSpace(e.GetWriteLeaseId()) == "" ||
		strings.TrimSpace(e.GetFencingToken()) == "" ||
		strings.TrimSpace(request.GetBaseWorkspaceVersionId()) == "" ||
		e.GetFencingGeneration() == 0 ||
		request.GetOwnershipGeneration() <= 0 ||
		request.GetWriterGeneration() <= 0 {
		return errors.New("workspace exec authority is incomplete")
	}
	if subtle.ConstantTimeCompare([]byte(e.GetInstanceLeaseId()), []byte(e.GetWriteLeaseId())) != 1 {
		return errors.New("workspace exec lease identity differs")
	}
	if time.Now().UnixNano() >= e.GetOperationExpiresAtUnixNano() {
		return errors.New("workspace exec claim expired")
	}
	return nil
}

func (entry *workspaceMountEntry) executeBasicExec(
	request *workspacev0.WorkspaceBasicExecRequest,
) *workspacev0.WorkspaceBasicExecResult {
	if entry.basicExecRun != nil {
		return entry.basicExecRun(request)
	}
	return entry.executeWorkspaceBasicExec(request)
}

func (entry *workspaceMountEntry) executeWorkspaceBasicExec(
	request *workspacev0.WorkspaceBasicExecRequest,
) *workspacev0.WorkspaceBasicExecResult {
	fingerprint := request.GetEnvelope().GetRequestFingerprint()
	var spec workspaceBasicExecSpec
	decoder := json.NewDecoder(strings.NewReader(request.GetRequestJson()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", err)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" ||
		spec.TimeoutMS <= 0 || spec.TimeoutMS > int64((15*time.Minute)/time.Millisecond) {
		return workspaceBasicExecFailure(fingerprint, "workspace_exec_invalid", errors.New("workspace exec request is invalid"))
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
		workspaceBasicExecImageCommandOptions(),
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
			errors.New("workspace exec output limit exceeded"),
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
				fmt.Errorf("workspace exec terminated by %s", status.Signal()),
				stdout.Bytes(),
				stderr.Bytes(),
			)
		}
	}
	return &workspacev0.WorkspaceBasicExecResult{
		ExitCode:           exitCode,
		Stdout:             stdout.Bytes(),
		Stderr:             stderr.Bytes(),
		Outcome:            "exited",
		RequestFingerprint: fingerprint,
	}
}

func workspaceBasicExecImageCommandOptions() imageCommandOptions {
	// Direct workspace exec is workspace-tool authority. It must not mount the
	// program artifact or managed runtime drives into the image namespace.
	return imageCommandOptions{}
}

func workspaceBasicExecSecrets(
	deliveries []*workspacev0.WorkspaceSecretDelivery,
) ([]*programv0.ProgramSecret, error) {
	secrets := make([]*programv0.ProgramSecret, 0, len(deliveries))
	for _, delivery := range deliveries {
		secret := &programv0.ProgramSecret{Value: bytes.Clone(delivery.GetValue())}
		switch delivery.GetPlacementKind() {
		case "env":
			secret.Placement = &programv0.ProgramSecret_Env{Env: delivery.GetPlacementTarget()}
		case "file":
			secret.Placement = &programv0.ProgramSecret_File{File: delivery.GetPlacementTarget()}
		default:
			clear(secret.Value)
			clearProgramSecretValues(secrets)
			return nil, fmt.Errorf("unsupported workspace secret placement %q", delivery.GetPlacementKind())
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

func workspaceBasicExecFailure(
	fingerprint string,
	code string,
	err error,
) *workspacev0.WorkspaceBasicExecResult {
	return workspaceBasicExecFailureWithOutput(fingerprint, code, err, nil, nil)
}

func workspaceBasicExecFailureWithOutput(
	fingerprint string,
	code string,
	err error,
	stdout []byte,
	stderr []byte,
) *workspacev0.WorkspaceBasicExecResult {
	message := code
	if err != nil {
		message = err.Error()
	}
	errorJSON, marshalErr := json.Marshal(map[string]string{"code": code, "message": message})
	if marshalErr != nil {
		errorJSON = []byte(`{"code":"workspace_exec_failed"}`)
	}
	return &workspacev0.WorkspaceBasicExecResult{
		ErrorJson:          string(errorJSON),
		Stdout:             stdout,
		Stderr:             stderr,
		Outcome:            code,
		RequestFingerprint: fingerprint,
	}
}

func clearWorkspaceBasicExecRequest(request *workspacev0.WorkspaceBasicExecRequest) {
	clear(request.Stdin)
	request.Stdin = nil
	for _, delivery := range request.Secrets {
		clear(delivery.Value)
		delivery.Value = nil
	}
	request.Secrets = nil
}

func (entry *workspaceMountEntry) workspaceLaunchCwd(raw string) (string, error) {
	return resolveLaunchCwd(raw, entry.workspaceMount)
}

func (entry *workspaceMountEntry) workspaceProcessEnv(
	launchCwd string,
	userEnv map[string]string,
) ([]string, error) {
	if entry.runtimeUser == nil {
		return nil, errors.New("workspace runtime user is not resolved")
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

func (entry *workspaceMountEntry) prepareWorkspaceOwner() error {
	if entry.runtimeUser == nil || os.Geteuid() != 0 {
		return nil
	}
	if err := chownTree(
		entry.workspaceRoot,
		entry.runtimeUser.UID,
		entry.runtimeUser.GID,
	); err != nil {
		return fmt.Errorf("prepare workspace owner: %w", err)
	}
	return nil
}

func (entry *workspaceMountEntry) workspaceRuntimePath(
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
	searchPath := workspaceEnvValue(env, "PATH")
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

func workspaceEnvValue(env []string, key string) string {
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
