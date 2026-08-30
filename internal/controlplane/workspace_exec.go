package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

const (
	workspaceExecBodyMaxBytes   = int64(2 << 20)
	workspaceExecArgMaxCount    = 128
	workspaceExecArgMaxBytes    = 64 << 10
	workspaceExecEnvMaxCount    = 128
	workspaceExecEnvMaxBytes    = 256 << 10
	workspaceExecStdinMaxBytes  = 1 << 20
	workspaceExecOutputMaxBytes = 4 << 20
	workspaceExecDefaultTimeout = 5 * time.Minute
	workspaceExecMaxTimeout     = 15 * time.Minute
)

var workspaceExecEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	errWorkspaceExecInvalid        = errors.New("workspace exec request is invalid")
	errWorkspaceExecTooLarge       = errors.New("workspace exec request is too large")
	errWorkspaceExecStdinTooLarge  = errors.New("workspace exec stdin is too large")
	errWorkspaceExecReceiptInvalid = errors.New("workspace exec idempotency receipt is invalid")
)

type workspaceExecRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	Workspace      db.Workspace
	Creator        workspaceExecCreator
	Command        []string
	Cwd            string
	Env            map[string]string
	Stdin          []byte
	Timeout        time.Duration
	IdempotencyKey string
	Authorize      func(context.Context, db.Querier) error
}

type workspaceExecCreator struct {
	SubjectType string
	SubjectID   string
}

type normalizedWorkspaceExec struct {
	requestJSON json.RawMessage
	command     []string
	cwd         string
	env         map[string]string
	envJSON     json.RawMessage
	stdin       []byte
	stdinHash   [sha256.Size]byte
	timeout     time.Duration
	timeoutMS   int64
}

type workspaceExecAdmission struct {
	Process  db.WorkspaceProcess
	Replayed bool
}

type workspaceExecSpec struct {
	Command   []string          `json:"command"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	TimeoutMS int64             `json:"timeout_ms"`
}

func normalizeWorkspaceExec(request workspaceExecRequest) (normalizedWorkspaceExec, error) {
	if len(request.Command) == 0 {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: command is required", errWorkspaceExecInvalid)
	}
	if len(request.Command) > workspaceExecArgMaxCount {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: command has more than %d arguments", errWorkspaceExecTooLarge, workspaceExecArgMaxCount)
	}
	command := append([]string{}, request.Command...)
	argumentBytes := 0
	for index, value := range command {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return normalizedWorkspaceExec{}, fmt.Errorf("%w: command arguments must be valid UTF-8 without NUL", errWorkspaceExecInvalid)
		}
		if index == 0 && value == "" {
			return normalizedWorkspaceExec{}, fmt.Errorf("%w: command executable is required", errWorkspaceExecInvalid)
		}
		argumentBytes += len(value)
	}
	if argumentBytes > workspaceExecArgMaxBytes {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: command exceeds %d bytes", errWorkspaceExecTooLarge, workspaceExecArgMaxBytes)
	}

	cwd := request.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}
	if !utf8.ValidString(cwd) || len(cwd) > 4096 || strings.IndexByte(cwd, 0) >= 0 ||
		!strings.HasPrefix(cwd, "/") || path.Clean(cwd) != cwd ||
		(cwd != "/workspace" && !strings.HasPrefix(cwd, "/workspace/")) {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: cwd must be a canonical absolute path beneath /workspace", errWorkspaceExecInvalid)
	}

	if len(request.Env) > workspaceExecEnvMaxCount {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: env has more than %d entries", errWorkspaceExecTooLarge, workspaceExecEnvMaxCount)
	}
	env := make(map[string]string, len(request.Env))
	envBytes := 0
	for name, value := range request.Env {
		if !workspaceExecEnvNamePattern.MatchString(name) || strings.HasPrefix(name, "HELMR_") {
			return normalizedWorkspaceExec{}, fmt.Errorf("%w: env name %q is invalid or reserved", errWorkspaceExecInvalid, name)
		}
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return normalizedWorkspaceExec{}, fmt.Errorf("%w: env value for %q must be valid UTF-8 without NUL", errWorkspaceExecInvalid, name)
		}
		env[name] = value
		envBytes += len(name) + len(value)
	}
	if envBytes > workspaceExecEnvMaxBytes {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: env exceeds %d bytes", errWorkspaceExecTooLarge, workspaceExecEnvMaxBytes)
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: encode env: %v", errWorkspaceExecInvalid, err)
	}
	if len(request.Stdin) > workspaceExecStdinMaxBytes {
		return normalizedWorkspaceExec{}, errWorkspaceExecStdinTooLarge
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = workspaceExecDefaultTimeout
	}
	if timeout < time.Millisecond || timeout > workspaceExecMaxTimeout {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: timeout must be between 1ms and 15m", errWorkspaceExecInvalid)
	}
	timeoutMS := timeout.Milliseconds()
	spec := workspaceExecSpec{
		Command:   command,
		Cwd:       cwd,
		Env:       env,
		TimeoutMS: timeoutMS,
	}
	requestJSON, err := json.Marshal(spec)
	if err != nil {
		return normalizedWorkspaceExec{}, fmt.Errorf("%w: encode request: %v", errWorkspaceExecInvalid, err)
	}
	return normalizedWorkspaceExec{
		requestJSON: requestJSON,
		command:     command,
		cwd:         cwd,
		env:         env,
		envJSON:     envJSON,
		stdin:       nonNilWorkspaceExecBytes(request.Stdin),
		stdinHash:   sha256.Sum256(request.Stdin),
		timeout:     timeout,
		timeoutMS:   timeoutMS,
	}, nil
}

func nonNilWorkspaceExecBytes(value []byte) []byte {
	if len(value) == 0 {
		return []byte{}
	}
	return bytes.Clone(value)
}

func (s *Server) admitWorkspaceExec(ctx context.Context, request workspaceExecRequest) (workspaceExecAdmission, error) {
	switch request.Creator.SubjectType {
	case string(auth.ActorKindAPIKey), string(auth.ActorKindSession), "run":
	default:
		return workspaceExecAdmission{}, fmt.Errorf("%w: creator type is invalid", errWorkspaceExecInvalid)
	}
	creatorID, err := uuid.Parse(request.Creator.SubjectID)
	if err != nil || creatorID == uuid.Nil() || creatorID.String() != request.Creator.SubjectID {
		return workspaceExecAdmission{}, fmt.Errorf("%w: creator ID is invalid", errWorkspaceExecInvalid)
	}
	normalized, err := normalizeWorkspaceExec(request)
	if err != nil {
		return workspaceExecAdmission{}, err
	}
	workspaceID := pgvalue.MustUUIDValue(request.Workspace.ID)
	claimRequest, err := idempotency.NewWorkspaceExecRequest(
		request.EnvironmentID,
		workspaceID,
		request.IdempotencyKey,
		idempotency.WorkspaceExecFingerprint{
			Command:   normalized.command,
			Cwd:       normalized.cwd,
			Env:       normalized.envJSON,
			StdinHash: normalized.stdinHash,
			TimeoutMS: normalized.timeoutMS,
		},
	)
	if err != nil {
		return workspaceExecAdmission{}, fmt.Errorf("%w: %v", errWorkspaceExecInvalid, err)
	}

	var admission workspaceExecAdmission
	err = s.inTx(ctx, func(work *txWork) error {
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, claimRequest)
		if err != nil {
			return err
		}
		if !acquired.New {
			process, err := work.q.GetWorkspaceExecByClaim(ctx, db.GetWorkspaceExecByClaimParams{
				EnvironmentID: request.Workspace.EnvironmentID,
				WorkspaceID:   request.Workspace.ID,
				ClaimID:       acquired.Claim.ID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errWorkspaceExecReceiptInvalid
			}
			if err != nil {
				return err
			}
			admission = workspaceExecAdmission{Process: process, Replayed: true}
			return nil
		}

		bindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, request.Workspace.ID)
		if err != nil {
			return fmt.Errorf("lock workspace exec secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errWorkspaceSecretUnavailable
			}
			if binding.PlacementKind == "env" {
				if _, exists := normalized.env[binding.PlacementTarget]; exists {
					return fmt.Errorf("%w: env cannot override workspace secret %q", errWorkspaceExecInvalid, binding.PlacementTarget)
				}
			}
		}
		authority, err := work.q.LockWorkspaceAdmissionAuthority(ctx, db.LockWorkspaceAdmissionAuthorityParams{
			EnvironmentID: request.Workspace.EnvironmentID,
			ID:            request.Workspace.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWorkspaceNotFound
		}
		if err != nil {
			return fmt.Errorf("lock workspace exec authority: %w", err)
		}
		if authority.OrgID != pgvalue.UUID(request.OrgID) ||
			authority.ProjectID != pgvalue.UUID(request.ProjectID) ||
			authority.State != db.WorkspaceStateActive ||
			(authority.DesiredState != db.WorkspaceDesiredStateActive &&
				authority.DesiredState != db.WorkspaceDesiredStateStopped) ||
			authority.DirtyState != db.WorkspaceDirtyStateClean ||
			!authority.HeadVersionID.Valid {
			switch authority.State {
			case db.WorkspaceStateDeleting:
				return conflict(codedError{code: "workspace_deleting", message: "workspace is deleting"})
			case db.WorkspaceStateRecoveryRequired:
				return conflict(codedError{code: "workspace_recovery_required", message: "workspace requires recovery"})
			default:
				return errWorkspaceBusy
			}
		}
		if authority.OwnerSessionID.Valid || authority.OwnerRunID.Valid ||
			authority.HasActiveLease || authority.HasActiveProcess {
			return errWorkspaceBusy
		}

		processID := pgvalue.UUID(uuid.NewV7())
		process, err := work.q.CreateWorkspaceExec(ctx, db.CreateWorkspaceExecParams{
			ID:                   processID,
			OrgID:                authority.OrgID,
			ProjectID:            authority.ProjectID,
			EnvironmentID:        authority.EnvironmentID,
			WorkspaceID:          authority.ID,
			BaseVersionID:        authority.HeadVersionID,
			RestoreDesiredState:  authority.DesiredState,
			Request:              normalized.requestJSON,
			Stdin:                normalized.stdin,
			ClaimID:              acquired.Claim.ID,
			CreatedBySubjectType: request.Creator.SubjectType,
			CreatedBySubjectID:   request.Creator.SubjectID,
		})
		if err != nil {
			return fmt.Errorf("create workspace exec: %w", err)
		}
		if err := secret.CreateProcessResolutions(
			ctx, work.q, authority.ID, process.ID, workspaceSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record workspace exec secret resolutions: %w", err)
		}
		admission = workspaceExecAdmission{Process: process}
		return nil
	})
	return admission, err
}

func workspaceExecCreatorFromActor(principal auth.Actor) workspaceExecCreator {
	creator := workspaceExecCreator{SubjectType: string(principal.Kind)}
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		if principal.APIKeyID != uuid.Nil() {
			creator.SubjectID = principal.APIKeyID.String()
			return creator
		}
	case auth.ActorKindSession:
		if principal.SessionID != uuid.Nil() {
			creator.SubjectID = principal.SessionID.String()
			return creator
		}
	}
	return creator
}

func workspaceExecTerminal(state db.WorkspaceProcessState) bool {
	switch state {
	case db.WorkspaceProcessStateExited,
		db.WorkspaceProcessStateFailed:
		return true
	default:
		return false
	}
}

func publicWorkspaceExecProcess(process db.WorkspaceProcess) (api.WorkspaceExecProcess, error) {
	resource := api.WorkspaceExecProcess{ProcessID: pgvalue.MustUUIDValue(process.ID).String()}
	switch process.State {
	case db.WorkspaceProcessStatePending, db.WorkspaceProcessStateStarting:
		resource.Status = api.WorkspaceExecProcessStatusPending
	case db.WorkspaceProcessStateRunning, db.WorkspaceProcessStateExitRequested:
		resource.Status = api.WorkspaceExecProcessStatusRunning
	case db.WorkspaceProcessStateExited:
		if !process.ExitCode.Valid || process.Stdout == nil || process.Stderr == nil {
			return api.WorkspaceExecProcess{}, errors.New("workspace exec terminal output is unavailable")
		}
		if len(process.Stdout) > workspaceExecOutputMaxBytes || len(process.Stderr) > workspaceExecOutputMaxBytes {
			return api.WorkspaceExecProcess{}, errors.New("workspace exec terminal output exceeds its persisted limit")
		}
		stdout := base64.StdEncoding.EncodeToString(process.Stdout)
		stderr := base64.StdEncoding.EncodeToString(process.Stderr)
		exitCode := process.ExitCode.Int32
		resource.Status = api.WorkspaceExecProcessStatusExited
		resource.ExitCode = &exitCode
		resource.StdoutBase64 = &stdout
		resource.StderrBase64 = &stderr
	case db.WorkspaceProcessStateFailed:
		resource.Status = api.WorkspaceExecProcessStatusFailed
		resource.Error = &api.WorkspaceExecProcessError{
			TerminalReasonCode: publicWorkspaceExecTerminalReason(process.TerminalReasonCode.String),
		}
	default:
		return api.WorkspaceExecProcess{}, errors.New("workspace exec state is invalid")
	}
	return resource, nil
}

func publicWorkspaceExecTerminalReason(code string) string {
	switch code {
	case "workspace_exec_timed_out",
		"workspace_exec_output_limit_exceeded",
		"workspace_exec_placement_timed_out":
		return code
	default:
		return "workspace_exec_failed"
	}
}

func workspaceExecResult(process db.WorkspaceProcess) (api.ExecuteWorkspaceResult, error) {
	if !workspaceExecTerminal(process.State) {
		return api.ExecuteWorkspaceResult{}, errors.New("workspace exec is not terminal")
	}
	if process.State != db.WorkspaceProcessStateExited || !process.ExitCode.Valid {
		code := process.TerminalReasonCode.String
		switch code {
		case "workspace_exec_timed_out":
			return api.ExecuteWorkspaceResult{}, apiError{kind: errUnprocessable, err: codedError{code: code, message: "workspace exec timed out"}}
		case "workspace_exec_output_limit_exceeded":
			return api.ExecuteWorkspaceResult{}, apiError{kind: errUnprocessable, err: codedError{code: code, message: "workspace exec output limit was exceeded"}}
		default:
			return api.ExecuteWorkspaceResult{}, apiError{kind: errUnprocessable, err: codedError{code: "workspace_exec_failed", message: "workspace exec failed"}}
		}
	}
	if process.Stdout == nil || process.Stderr == nil {
		return api.ExecuteWorkspaceResult{}, errors.New("workspace exec terminal output is unavailable")
	}
	if len(process.Stdout) > workspaceExecOutputMaxBytes ||
		len(process.Stderr) > workspaceExecOutputMaxBytes {
		return api.ExecuteWorkspaceResult{}, errors.New("workspace exec terminal output exceeds its persisted limit")
	}
	return api.ExecuteWorkspaceResult{
		ExitCode:     process.ExitCode.Int32,
		StdoutBase64: base64.StdEncoding.EncodeToString(process.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(process.Stderr),
	}, nil
}
