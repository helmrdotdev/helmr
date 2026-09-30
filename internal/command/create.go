package command

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

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

const (
	// MaxStdinBytes bounds the stdin a Command carries.
	MaxStdinBytes = 1 << 20
	// DefaultTimeout is the timeout of a Command that names none.
	DefaultTimeout = 5 * time.Minute
	// MaxTimeout is the longest timeout a Command may name.
	MaxTimeout = 15 * time.Minute

	argMaxCount = 128
	argMaxBytes = 64 << 10
	envMaxCount = 128
	envMaxBytes = 256 << 10
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ErrReceiptInvalid reports a stored idempotency receipt this owner did not
// write.
var ErrReceiptInvalid = errors.New("command idempotency receipt is invalid")

// InputKind classifies a rejected Command request.
type InputKind uint8

const (
	// InputInvalid is a malformed request.
	InputInvalid InputKind = iota + 1
	// InputTooLarge is a request whose argv or environment exceeds its bound.
	InputTooLarge
	// InputStdinTooLarge is a request whose stdin exceeds MaxStdinBytes.
	InputStdinTooLarge
)

// InputError reports a Command request the command owner rejects.
type InputError struct {
	Kind    InputKind
	message string
}

func (e InputError) Error() string {
	return e.message
}

func invalid(format string, args ...any) InputError {
	return InputError{Kind: InputInvalid, message: "computer exec request is invalid: " + fmt.Sprintf(format, args...)}
}

func tooLarge(format string, args ...any) InputError {
	return InputError{Kind: InputTooLarge, message: "computer exec request is too large: " + fmt.Sprintf(format, args...)}
}

// Creator is the API key or session that creates a Command.
type Creator struct {
	SubjectType string
	SubjectID   string
}

// CreateRequest asks to run a process on a Computer, once per idempotency
// key. A zero Cwd is /workspace and a zero Timeout is DefaultTimeout.
type CreateRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	ComputerID     uuid.UUID
	Creator        Creator
	Argv           []string
	Cwd            string
	Env            map[string]string
	Stdin          []byte
	Timeout        time.Duration
	IdempotencyKey string
}

type normalizedRequest struct {
	argv      []string
	cwd       string
	env       map[string]string
	envJSON   json.RawMessage
	stdin     []byte
	stdinHash [sha256.Size]byte
	timeoutMS int64
}

func normalize(request CreateRequest) (normalizedRequest, error) {
	if len(request.Argv) == 0 {
		return normalizedRequest{}, invalid("command is required")
	}
	if len(request.Argv) > argMaxCount {
		return normalizedRequest{}, tooLarge("command has more than %d arguments", argMaxCount)
	}
	argv := append([]string{}, request.Argv...)
	argumentBytes := 0
	for index, value := range argv {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return normalizedRequest{}, invalid("command arguments must be valid UTF-8 without NUL")
		}
		if index == 0 && value == "" {
			return normalizedRequest{}, invalid("command executable is required")
		}
		argumentBytes += len(value)
	}
	if argumentBytes > argMaxBytes {
		return normalizedRequest{}, tooLarge("command exceeds %d bytes", argMaxBytes)
	}

	cwd := request.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}
	if !utf8.ValidString(cwd) || len(cwd) > 4096 || strings.IndexByte(cwd, 0) >= 0 ||
		!strings.HasPrefix(cwd, "/") || path.Clean(cwd) != cwd ||
		(cwd != "/workspace" && !strings.HasPrefix(cwd, "/workspace/")) {
		return normalizedRequest{}, invalid("cwd must be a canonical absolute path beneath /workspace")
	}

	if len(request.Env) > envMaxCount {
		return normalizedRequest{}, tooLarge("env has more than %d entries", envMaxCount)
	}
	env := make(map[string]string, len(request.Env))
	envBytes := 0
	for name, value := range request.Env {
		if !envNamePattern.MatchString(name) || strings.HasPrefix(name, "HELMR_") {
			return normalizedRequest{}, invalid("env name %q is invalid or reserved", name)
		}
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return normalizedRequest{}, invalid("env value for %q must be valid UTF-8 without NUL", name)
		}
		env[name] = value
		envBytes += len(name) + len(value)
	}
	if envBytes > envMaxBytes {
		return normalizedRequest{}, tooLarge("env exceeds %d bytes", envMaxBytes)
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return normalizedRequest{}, invalid("encode env: %v", err)
	}
	if len(request.Stdin) > MaxStdinBytes {
		return normalizedRequest{}, InputError{Kind: InputStdinTooLarge, message: "computer exec stdin is too large"}
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < time.Millisecond || timeout > MaxTimeout {
		return normalizedRequest{}, invalid("timeout must be between 1ms and 15m")
	}
	return normalizedRequest{
		argv:      argv,
		cwd:       cwd,
		env:       env,
		envJSON:   envJSON,
		stdin:     nonNilBytes(request.Stdin),
		stdinHash: sha256.Sum256(request.Stdin),
		timeoutMS: timeout.Milliseconds(),
	}, nil
}

func nonNilBytes(value []byte) []byte {
	if len(value) == 0 {
		return []byte{}
	}
	return bytes.Clone(value)
}

// createReceipt is the idempotency receipt of an admitted Command.
type createReceipt struct {
	CommandID string `json:"command_id"`
}

// Create admits a pending Command on the Computer, or replays the Command
// its idempotency key already admitted. A retained Computer identity
// authorizes a replay after deletion; new admission requires the Computer's
// live authority. In one transaction Create acquires the idempotency claim,
// then locks the Computer's Secrets and the Computer, records the admission,
// the Command and its Secret resolutions, and completes the receipt.
func Create(ctx context.Context, txb db.TxBeginner, request CreateRequest) (db.ComputerCommand, error) {
	switch request.Creator.SubjectType {
	case string(auth.PrincipalKindAPIKey), string(auth.PrincipalKindSession):
	default:
		return db.ComputerCommand{}, invalid("creator type is invalid")
	}
	creatorID, err := uuid.Parse(request.Creator.SubjectID)
	if err != nil || creatorID == uuid.Nil() || creatorID.String() != request.Creator.SubjectID {
		return db.ComputerCommand{}, invalid("creator ID is invalid")
	}
	normalized, err := normalize(request)
	if err != nil {
		return db.ComputerCommand{}, err
	}
	claimRequest, err := idempotency.NewComputerCommandRequest(
		request.EnvironmentID,
		request.ComputerID,
		request.IdempotencyKey,
		idempotency.ComputerCommandFingerprint{
			Command:   normalized.argv,
			Cwd:       normalized.cwd,
			Env:       normalized.envJSON,
			StdinHash: normalized.stdinHash,
			TimeoutMS: normalized.timeoutMS,
		},
	)
	if err != nil {
		return db.ComputerCommand{}, invalid("%v", err)
	}
	var created db.ComputerCommand
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		created, err = create(ctx, tx, request, normalized, claimRequest)
		return err
	})
	if err != nil {
		return db.ComputerCommand{}, err
	}
	return created, nil
}

func create(ctx context.Context, tx pgx.Tx, request CreateRequest, normalized normalizedRequest, claimRequest idempotency.Request) (db.ComputerCommand, error) {
	q := db.New(tx)
	var scoped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id
 WHERE c.id=$1 AND c.environment_id=$2 AND e.org_id=$3 AND e.project_id=$4
)`, request.ComputerID, request.EnvironmentID, request.OrgID, request.ProjectID).Scan(&scoped); err != nil {
		return db.ComputerCommand{}, err
	}
	if !scoped {
		return db.ComputerCommand{}, computer.ErrNotFound
	}
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return db.ComputerCommand{}, err
	}
	acquired, err := claims.Acquire(ctx, claimRequest)
	if err != nil {
		return db.ComputerCommand{}, err
	}
	if !acquired.New {
		replayed, err := q.GetComputerCommandByClaim(ctx, db.GetComputerCommandByClaimParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			OrgID:         pgvalue.UUID(request.OrgID),
			ClaimID:       acquired.Claim.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ComputerCommand{}, ErrReceiptInvalid
		}
		return replayed, err
	}

	bindings, err := q.LockComputerSecretsForAdmission(ctx, pgvalue.UUID(request.ComputerID))
	if err != nil {
		return db.ComputerCommand{}, fmt.Errorf("lock computer exec secrets: %w", err)
	}
	for _, binding := range bindings {
		if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
			return db.ComputerCommand{}, computer.ErrSecretUnavailable
		}
		if binding.PlacementKind == "env" {
			if _, exists := normalized.env[binding.PlacementTarget]; exists {
				return db.ComputerCommand{}, invalid("env cannot override computer secret %q", binding.PlacementTarget)
			}
		}
	}
	admission, err := computer.LockForAdmission(ctx, tx, request.EnvironmentID, request.ComputerID)
	if err != nil {
		return db.ComputerCommand{}, err
	}
	if err := admits(admission.Row(), request); err != nil {
		return db.ComputerCommand{}, err
	}
	if err := admission.Touch(ctx); err != nil {
		return db.ComputerCommand{}, fmt.Errorf("record command computer admission: %w", err)
	}

	authority := admission.Row()
	created, err := q.CreateComputerCommand(ctx, db.CreateComputerCommandParams{
		ID:                   pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:        authority.EnvironmentID,
		ComputerID:           authority.ID,
		Argv:                 normalized.argv,
		Cwd:                  pgvalue.Text(normalized.cwd),
		Env:                  normalized.envJSON,
		TimeoutMs:            normalized.timeoutMS,
		Stdin:                normalized.stdin,
		ClaimID:              acquired.Claim.ID,
		CreatedBySubjectType: request.Creator.SubjectType,
		CreatedBySubjectID:   request.Creator.SubjectID,
	})
	if err != nil {
		return db.ComputerCommand{}, fmt.Errorf("create computer exec: %w", err)
	}
	if err := secret.CreateProcessResolutions(ctx, q, authority.ID, created.ID, resolutions(bindings)); err != nil {
		return db.ComputerCommand{}, fmt.Errorf("record computer exec secret resolutions: %w", err)
	}
	receipt, err := json.Marshal(createReceipt{CommandID: pgvalue.UUIDString(created.ID)})
	if err != nil {
		return db.ComputerCommand{}, err
	}
	if _, err := claims.Complete(ctx, acquired.Claim, receipt); err != nil {
		return db.ComputerCommand{}, err
	}
	return created, nil
}

// admits reports why the locked Computer does not admit a new Command in the
// request's scope. Recovery takes precedence over deletion, and any other
// state that may clear is ErrBusy.
func admits(authority db.LockComputerAdmissionAuthorityRow, request CreateRequest) error {
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
			return computer.ErrRecoveryRequired
		}
		switch authority.Status {
		case db.ComputerStatusDeleting:
			return computer.ErrDeleting
		case db.ComputerStatusRecoveryRequired:
			return computer.ErrRecoveryRequired
		default:
			return computer.ErrBusy
		}
	}
	if len(authority.PreparationFailure) > 0 {
		return computer.ErrPreparationExhausted
	}
	return nil
}

func resolutions(bindings []db.LockComputerSecretsForAdmissionRow) []secret.Resolution {
	values := make([]secret.Resolution, len(bindings))
	for index, binding := range bindings {
		values[index] = secret.Resolution{
			PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
			SecretID: binding.SecretID, SecretVersionID: binding.CurrentVersionID,
			RevocationGeneration: binding.RevocationGeneration,
		}
	}
	return values
}
