package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	computerCommandBodyMaxBytes   = int64(2 << 20)
	computerCommandArgMaxCount    = 128
	computerCommandArgMaxBytes    = 64 << 10
	computerCommandEnvMaxCount    = 128
	computerCommandEnvMaxBytes    = 256 << 10
	computerCommandStdinMaxBytes  = 1 << 20
	computerCommandDefaultTimeout = 5 * time.Minute
	computerCommandMaxTimeout     = 15 * time.Minute
)

var computerCommandEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	errComputerCommandInvalid        = errors.New("computer exec request is invalid")
	errComputerCommandTooLarge       = errors.New("computer exec request is too large")
	errComputerCommandStdinTooLarge  = errors.New("computer exec stdin is too large")
	errComputerCommandReceiptInvalid = errors.New("computer exec idempotency receipt is invalid")
)

type computerCommandRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	ComputerID     uuid.UUID
	Creator        computerCommandCreator
	Command        []string
	Cwd            string
	Env            map[string]string
	Stdin          []byte
	Timeout        time.Duration
	IdempotencyKey string
	Authorize      func(context.Context, pgx.Tx) error
}

type computerCommandCreator struct {
	SubjectType string
	SubjectID   string
}

type normalizedComputerCommand struct {
	command   []string
	cwd       string
	env       map[string]string
	envJSON   json.RawMessage
	stdin     []byte
	stdinHash [sha256.Size]byte
	timeout   time.Duration
	timeoutMS int64
}

type computerCommandAdmission struct {
	Process  db.ComputerCommand
	Replayed bool
}

type computerCommandSpec struct {
	Command   []string          `json:"command"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	TimeoutMS int64             `json:"timeout_ms"`
}

func normalizeComputerCommand(request computerCommandRequest) (normalizedComputerCommand, error) {
	if len(request.Command) == 0 {
		return normalizedComputerCommand{}, fmt.Errorf("%w: command is required", errComputerCommandInvalid)
	}
	if len(request.Command) > computerCommandArgMaxCount {
		return normalizedComputerCommand{}, fmt.Errorf("%w: command has more than %d arguments", errComputerCommandTooLarge, computerCommandArgMaxCount)
	}
	command := append([]string{}, request.Command...)
	argumentBytes := 0
	for index, value := range command {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return normalizedComputerCommand{}, fmt.Errorf("%w: command arguments must be valid UTF-8 without NUL", errComputerCommandInvalid)
		}
		if index == 0 && value == "" {
			return normalizedComputerCommand{}, fmt.Errorf("%w: command executable is required", errComputerCommandInvalid)
		}
		argumentBytes += len(value)
	}
	if argumentBytes > computerCommandArgMaxBytes {
		return normalizedComputerCommand{}, fmt.Errorf("%w: command exceeds %d bytes", errComputerCommandTooLarge, computerCommandArgMaxBytes)
	}

	cwd := request.Cwd
	if cwd == "" {
		cwd = "/computer"
	}
	if !utf8.ValidString(cwd) || len(cwd) > 4096 || strings.IndexByte(cwd, 0) >= 0 ||
		!strings.HasPrefix(cwd, "/") || path.Clean(cwd) != cwd ||
		(cwd != "/computer" && !strings.HasPrefix(cwd, "/computer/")) {
		return normalizedComputerCommand{}, fmt.Errorf("%w: cwd must be a canonical absolute path beneath /computer", errComputerCommandInvalid)
	}

	if len(request.Env) > computerCommandEnvMaxCount {
		return normalizedComputerCommand{}, fmt.Errorf("%w: env has more than %d entries", errComputerCommandTooLarge, computerCommandEnvMaxCount)
	}
	env := make(map[string]string, len(request.Env))
	envBytes := 0
	for name, value := range request.Env {
		if !computerCommandEnvNamePattern.MatchString(name) || strings.HasPrefix(name, "HELMR_") {
			return normalizedComputerCommand{}, fmt.Errorf("%w: env name %q is invalid or reserved", errComputerCommandInvalid, name)
		}
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return normalizedComputerCommand{}, fmt.Errorf("%w: env value for %q must be valid UTF-8 without NUL", errComputerCommandInvalid, name)
		}
		env[name] = value
		envBytes += len(name) + len(value)
	}
	if envBytes > computerCommandEnvMaxBytes {
		return normalizedComputerCommand{}, fmt.Errorf("%w: env exceeds %d bytes", errComputerCommandTooLarge, computerCommandEnvMaxBytes)
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return normalizedComputerCommand{}, fmt.Errorf("%w: encode env: %v", errComputerCommandInvalid, err)
	}
	if len(request.Stdin) > computerCommandStdinMaxBytes {
		return normalizedComputerCommand{}, errComputerCommandStdinTooLarge
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = computerCommandDefaultTimeout
	}
	if timeout < time.Millisecond || timeout > computerCommandMaxTimeout {
		return normalizedComputerCommand{}, fmt.Errorf("%w: timeout must be between 1ms and 15m", errComputerCommandInvalid)
	}
	timeoutMS := timeout.Milliseconds()
	return normalizedComputerCommand{
		command:   command,
		cwd:       cwd,
		env:       env,
		envJSON:   envJSON,
		stdin:     nonNilComputerCommandBytes(request.Stdin),
		stdinHash: sha256.Sum256(request.Stdin),
		timeout:   timeout,
		timeoutMS: timeoutMS,
	}, nil
}

func nonNilComputerCommandBytes(value []byte) []byte {
	if len(value) == 0 {
		return []byte{}
	}
	return bytes.Clone(value)
}

func (s *Server) admitComputerCommand(ctx context.Context, request computerCommandRequest) (computerCommandAdmission, error) {
	switch request.Creator.SubjectType {
	case string(auth.ActorKindAPIKey), string(auth.ActorKindSession):
	default:
		return computerCommandAdmission{}, fmt.Errorf("%w: creator type is invalid", errComputerCommandInvalid)
	}
	creatorID, err := uuid.Parse(request.Creator.SubjectID)
	if err != nil || creatorID == uuid.Nil() || creatorID.String() != request.Creator.SubjectID {
		return computerCommandAdmission{}, fmt.Errorf("%w: creator ID is invalid", errComputerCommandInvalid)
	}
	normalized, err := normalizeComputerCommand(request)
	if err != nil {
		return computerCommandAdmission{}, err
	}
	computerID := request.ComputerID
	claimRequest, err := idempotency.NewComputerCommandRequest(
		request.EnvironmentID,
		computerID,
		request.IdempotencyKey,
		idempotency.ComputerCommandFingerprint{
			Command:   normalized.command,
			Cwd:       normalized.cwd,
			Env:       normalized.envJSON,
			StdinHash: normalized.stdinHash,
			TimeoutMS: normalized.timeoutMS,
		},
	)
	if err != nil {
		return computerCommandAdmission{}, fmt.Errorf("%w: %v", errComputerCommandInvalid, err)
	}

	var admission computerCommandAdmission
	err = s.inTx(ctx, func(work *txWork) error {
		// Retained Computer identity authorizes receipt replay after deletion;
		// new admissions still require live authority below.
		var scoped bool
		if err := work.tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id
 WHERE c.id=$1 AND c.environment_id=$2 AND e.org_id=$3 AND e.project_id=$4
)`, request.ComputerID, request.EnvironmentID, request.OrgID, request.ProjectID).Scan(&scoped); err != nil {
			return err
		}
		if !scoped {
			return errComputerNotFound
		}
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, claimRequest)
		if err != nil {
			return err
		}
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.tx); err != nil {
				return err
			}
		}
		if !acquired.New {
			process, err := work.q.GetComputerCommandByClaim(ctx, db.GetComputerCommandByClaimParams{
				EnvironmentID: pgvalue.UUID(request.EnvironmentID),
				OrgID:         pgvalue.UUID(request.OrgID),
				ClaimID:       acquired.Claim.ID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errComputerCommandReceiptInvalid
			}
			if err != nil {
				return err
			}
			admission = computerCommandAdmission{Process: process, Replayed: true}
			return nil
		}

		bindings, err := work.q.LockComputerSecretsForAdmission(ctx, pgvalue.UUID(request.ComputerID))
		if err != nil {
			return fmt.Errorf("lock computer exec secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
				return errComputerSecretUnavailable
			}
			if binding.PlacementKind == "env" {
				if _, exists := normalized.env[binding.PlacementTarget]; exists {
					return fmt.Errorf("%w: env cannot override computer secret %q", errComputerCommandInvalid, binding.PlacementTarget)
				}
			}
		}
		authority, err := work.q.LockComputerAdmissionAuthority(ctx, db.LockComputerAdmissionAuthorityParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			ID:            pgvalue.UUID(request.ComputerID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errComputerNotFound
		}
		if err != nil {
			return fmt.Errorf("lock computer exec authority: %w", err)
		}
		if authority.OrgID != pgvalue.UUID(request.OrgID) ||
			authority.ProjectID != pgvalue.UUID(request.ProjectID) ||
			authority.Status != db.ComputerStatusActive ||
			(authority.DesiredState != db.ComputerDesiredStateActive &&
				authority.DesiredState != db.ComputerDesiredStateStopped) ||
			authority.DirtyState == db.ComputerDirtyStateCaptureFailed ||
			authority.DirtyState == db.ComputerDirtyStateDirtyStateLost ||
			len(authority.RecoveryFailure) > 0 ||
			!authority.HeadDiskVersionID.Valid {
			if authority.DirtyState == db.ComputerDirtyStateCaptureFailed || len(authority.RecoveryFailure) > 0 {
				return conflict(codedError{code: "computer_recovery_required", message: "computer requires recovery"})
			}
			switch authority.Status {
			case db.ComputerStatusDeleting:
				return conflict(codedError{code: "computer_deleting", message: "computer is deleting"})
			case db.ComputerStatusRecoveryRequired:
				return conflict(codedError{code: "computer_recovery_required", message: "computer requires recovery"})
			default:
				return errComputerBusy
			}
		}
		if len(authority.PreparationFailure) > 0 {
			return conflict(codedError{code: "computer_preparation_exhausted", message: "Computer preparation limit reached"})
		}
		if _, err := work.q.TouchComputerForAdmission(ctx, db.TouchComputerForAdmissionParams{
			EnvironmentID: authority.EnvironmentID, ID: authority.ID, ExpectedRevision: authority.Revision,
		}); err != nil {
			return fmt.Errorf("record command computer admission: %w", err)
		}

		commandID := pgvalue.UUID(uuid.NewV7())
		process, err := work.q.CreateComputerCommand(ctx, db.CreateComputerCommandParams{
			ID:                   commandID,
			EnvironmentID:        authority.EnvironmentID,
			ComputerID:           authority.ID,
			Argv:                 normalized.command,
			Cwd:                  pgvalue.Text(normalized.cwd),
			Env:                  normalized.envJSON,
			TimeoutMs:            normalized.timeoutMS,
			Stdin:                normalized.stdin,
			ClaimID:              acquired.Claim.ID,
			CreatedBySubjectType: request.Creator.SubjectType,
			CreatedBySubjectID:   request.Creator.SubjectID,
		})
		if err != nil {
			return fmt.Errorf("create computer exec: %w", err)
		}
		if err := secret.CreateProcessResolutions(
			ctx, work.q, authority.ID, process.ID, computerSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record computer exec secret resolutions: %w", err)
		}
		receipt, err := json.Marshal(map[string]string{"command_id": pgvalue.UUIDString(process.ID)})
		if err != nil {
			return err
		}
		if _, err := claims.Complete(ctx, acquired.Claim, receipt); err != nil {
			return err
		}
		admission = computerCommandAdmission{Process: process}
		return nil
	})
	return admission, err
}

func computerCommandCreatorFromActor(principal auth.Actor) computerCommandCreator {
	creator := computerCommandCreator{SubjectType: string(principal.Kind)}
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

func computerCommandTerminal(state db.ComputerCommandStatus) bool {
	switch state {
	case db.ComputerCommandStatusExited, db.ComputerCommandStatusFailed, db.ComputerCommandStatusCancelled, db.ComputerCommandStatusTimedOut, db.ComputerCommandStatusLost:
		return true
	default:
		return false
	}
}

func publicCommandInfo(process db.ComputerCommand) (api.CommandInfo, error) {
	if process.ResultPrunedAt.Valid {
		return api.CommandInfo{}, gone(codedError{code: "command_result_expired", message: "exec result has expired"})
	}
	resource := api.CommandInfo{ID: pgvalue.MustUUIDValue(process.ID).String(), ComputerID: pgvalue.MustUUIDValue(process.ComputerID).String(), Status: string(process.Status)}
	switch process.Status {
	case db.ComputerCommandStatusPending, db.ComputerCommandStatusStarting, db.ComputerCommandStatusRunning:
		return resource, nil
	case db.ComputerCommandStatusStopping:
		return resource, nil
	case db.ComputerCommandStatusExited, db.ComputerCommandStatusFailed, db.ComputerCommandStatusCancelled, db.ComputerCommandStatusTimedOut, db.ComputerCommandStatusLost:
	default:
		return api.CommandInfo{}, errors.New("exec state is invalid")
	}
	if !process.TerminalAt.Valid {
		return api.CommandInfo{}, errors.New("exec terminal timestamp is missing")
	}
	outcome := &api.CommandOutcome{CommandID: resource.ID, TerminalAt: process.TerminalAt.Time, Kind: "system_failed"}
	if process.ExitCode.Valid {
		code := process.ExitCode.Int32
		outcome.ExitCode = &code
	}
	if process.Status == db.ComputerCommandStatusExited {
		if outcome.ExitCode == nil {
			return api.CommandInfo{}, errors.New("exec exit code is missing")
		}
		outcome.Kind = "exited"
	} else if process.Status == db.ComputerCommandStatusCancelled || process.Status == db.ComputerCommandStatusTimedOut {
		outcome.Kind = process.Status
	} else {
		switch process.FailureReason.String {
		case "guest_failure", "placement_failed", "scope_termination_failed":
			outcome.Failure = &api.CommandFailure{Reason: process.FailureReason.String}
		default:
			return api.CommandInfo{}, errors.New("Command failure reason is invalid")
		}
	}
	resource.Outcome = outcome
	resource.ProcessReconciled = !process.ComputerInstanceID.Valid || process.ProcessReconciledAt.Valid
	return resource, nil
}
