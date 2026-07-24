package control

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

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	errWorkspaceExecInvalid        = errors.New("Workspace exec request is invalid")
	errWorkspaceExecTooLarge       = errors.New("Workspace exec request is too large")
	errWorkspaceExecStdinTooLarge  = errors.New("Workspace exec stdin is too large")
	errWorkspaceExecReceiptInvalid = errors.New("Workspace exec idempotency receipt is invalid")
)

type workspaceExecRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	Workspace      db.Workspace
	Principal      auth.Actor
	Command        []string
	Cwd            string
	Env            map[string]string
	Stdin          []byte
	Timeout        time.Duration
	IdempotencyKey string
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
		stdin:       bytes.Clone(request.Stdin),
		stdinHash:   sha256.Sum256(request.Stdin),
		timeout:     timeout,
		timeoutMS:   timeoutMS,
	}, nil
}

func (s *Server) admitWorkspaceExec(ctx context.Context, request workspaceExecRequest) (workspaceExecAdmission, error) {
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
		claims, err := s.claims.TransactionForQueries(work.q)
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
			return fmt.Errorf("lock Workspace exec Secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errWorkspaceSecretUnavailable
			}
			if binding.PlacementKind == "env" {
				if _, exists := normalized.env[binding.PlacementTarget]; exists {
					return fmt.Errorf("%w: env cannot override Workspace Secret %q", errWorkspaceExecInvalid, binding.PlacementTarget)
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
			return fmt.Errorf("lock Workspace exec authority: %w", err)
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
				return conflict(codedError{code: "workspace_deleting", message: "Workspace is deleting"})
			case db.WorkspaceStateRecoveryRequired:
				return conflict(codedError{code: "workspace_recovery_required", message: "Workspace requires recovery"})
			default:
				return errWorkspaceBusy
			}
		}
		if authority.OwnerActorID.Valid || authority.OwnerRunID.Valid ||
			authority.HasActiveLease || authority.HasActiveProcess {
			return errWorkspaceBusy
		}

		processID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
		process, err := work.q.CreateWorkspaceExec(ctx, db.CreateWorkspaceExecParams{
			ID:                   processID,
			OrgID:                authority.OrgID,
			ProjectID:            authority.ProjectID,
			EnvironmentID:        authority.EnvironmentID,
			WorkspaceID:          authority.ID,
			Request:              normalized.requestJSON,
			ClaimID:              acquired.Claim.ID,
			CreatedBySubjectType: string(request.Principal.Kind),
			CreatedBySubjectID:   workspaceExecSubjectID(request.Principal),
		})
		if err != nil {
			return fmt.Errorf("create Workspace exec: %w", err)
		}
		if len(normalized.stdin) > 0 {
			if _, err := work.q.AppendWorkspaceProcessRecord(ctx, db.AppendWorkspaceProcessRecordParams{
				ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
				EnvironmentID:    authority.EnvironmentID,
				ProcessID:        process.ID,
				ProcessKind:      "exec",
				Stream:           "stdin",
				OffsetStart:      0,
				OffsetEnd:        int64(len(normalized.stdin)),
				ContentDigest:    normalized.stdinHash[:],
				SizeBytes:        int64(len(normalized.stdin)),
				Direction:        "input",
				Data:             normalized.stdin,
				ObservedAt:       acquired.Claim.AcceptedAt,
				PayloadExpiresAt: acquired.Claim.ExpiresAt,
			}); err != nil {
				return fmt.Errorf("record Workspace exec stdin: %w", err)
			}
			process.StdinCursor = pgtype.Int8{Int64: int64(len(normalized.stdin)), Valid: true}
		}
		for _, binding := range bindings {
			if _, err := work.q.CreateSecretResolution(ctx, db.CreateSecretResolutionParams{
				ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
				WorkspaceID:          authority.ID,
				ProcessID:            process.ID,
				PlacementKind:        binding.PlacementKind,
				PlacementTarget:      binding.PlacementTarget,
				SecretID:             binding.SecretID,
				SecretVersionID:      binding.CurrentVersionID,
				RevocationGeneration: binding.RevocationGeneration,
			}); err != nil {
				return fmt.Errorf("record Workspace exec Secret resolution: %w", err)
			}
		}
		admission = workspaceExecAdmission{Process: process}
		return nil
	})
	return admission, err
}

func workspaceExecSubjectID(principal auth.Actor) string {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		if principal.APIKeyID != uuid.Nil {
			return principal.APIKeyID.String()
		}
	case auth.ActorKindSession:
		if principal.SessionID != uuid.Nil {
			return principal.SessionID.String()
		}
	}
	if principal.UserID != uuid.Nil {
		return principal.UserID.String()
	}
	return ""
}

func workspaceExecTerminal(state db.WorkspaceProcessState) bool {
	switch state {
	case db.WorkspaceProcessStateExited,
		db.WorkspaceProcessStateCancelled,
		db.WorkspaceProcessStateLost,
		db.WorkspaceProcessStateFailed:
		return true
	default:
		return false
	}
}

func (s *Server) waitWorkspaceExec(ctx context.Context, admitted workspaceExecAdmission) (api.ExecuteWorkspaceResult, error) {
	process := admitted.Process
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for !workspaceExecTerminal(process.State) {
		select {
		case <-ctx.Done():
			return api.ExecuteWorkspaceResult{}, ctx.Err()
		case <-ticker.C:
		}
		var err error
		process, err = s.db.GetWorkspaceExec(ctx, db.GetWorkspaceExecParams{
			OrgID:         process.OrgID,
			ProjectID:     process.ProjectID,
			EnvironmentID: process.EnvironmentID,
			WorkspaceID:   process.WorkspaceID,
			ID:            process.ID,
		})
		if err != nil {
			return api.ExecuteWorkspaceResult{}, err
		}
	}
	if process.State != db.WorkspaceProcessStateExited || !process.ExitCode.Valid {
		code := process.TerminalReasonCode.String
		switch code {
		case "workspace_exec_timed_out":
			return api.ExecuteWorkspaceResult{}, apiError{kind: errUnprocessable, err: codedError{code: code, message: "Workspace exec timed out"}}
		case "workspace_exec_output_limit_exceeded":
			return api.ExecuteWorkspaceResult{}, apiError{kind: errUnprocessable, err: codedError{code: code, message: "Workspace exec output limit was exceeded"}}
		default:
			return api.ExecuteWorkspaceResult{}, apiError{kind: errUnprocessable, err: codedError{code: "workspace_exec_failed", message: "Workspace exec failed"}}
		}
	}
	records, err := s.db.ListWorkspaceExecTerminalRecords(ctx, db.ListWorkspaceExecTerminalRecordsParams{
		EnvironmentID: process.EnvironmentID,
		ProcessID:     process.ID,
	})
	if err != nil {
		return api.ExecuteWorkspaceResult{}, err
	}
	var stdout, stderr []byte
	for _, record := range records {
		if record.PayloadCollectedAt.Valid || record.Data == nil {
			return api.ExecuteWorkspaceResult{}, errors.New("Workspace exec terminal output is unavailable")
		}
		switch record.Stream {
		case "stdout":
			stdout = append(stdout, record.Data...)
		case "stderr":
			stderr = append(stderr, record.Data...)
		}
	}
	if len(stdout) > workspaceExecOutputMaxBytes || len(stderr) > workspaceExecOutputMaxBytes {
		return api.ExecuteWorkspaceResult{}, errors.New("Workspace exec terminal output exceeds its persisted limit")
	}
	return api.ExecuteWorkspaceResult{
		ExitCode:     process.ExitCode.Int32,
		StdoutBase64: base64.StdEncoding.EncodeToString(stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(stderr),
	}, nil
}
