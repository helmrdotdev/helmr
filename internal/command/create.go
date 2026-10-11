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
// the Command, and completes the receipt. Secret versions are selected only
// at the first possible delivery to this Command.
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
		replayed, err := q.GetComputerCommandByRetryKey(ctx, db.GetComputerCommandByRetryKeyParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			RetryKeyID:    acquired.Claim.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ComputerCommand{}, ErrReceiptInvalid
		}
		if err == nil {
			var receipt createReceipt
			if json.Unmarshal(acquired.Claim.Receipt, &receipt) != nil || receipt.CommandID != pgvalue.UUIDString(replayed.ID) {
				return db.ComputerCommand{}, ErrReceiptInvalid
			}
		}
		return replayed, err
	}

	// Secret locks precede the Computer lock, matching revocation. Admission
	// checks availability and placements without selecting a runtime version.
	rows, err := tx.Query(ctx, `SELECT b.placement_kind,b.placement_target,s.status
 FROM computer_secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id)
 WHERE b.environment_id=$1 AND b.computer_id=$2 ORDER BY s.id,b.placement_kind,b.placement_target FOR SHARE OF s`, request.EnvironmentID, request.ComputerID)
	if err != nil {
		return db.ComputerCommand{}, err
	}
	type binding struct{ kind, target, status string }
	bindings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (binding, error) {
		var b binding
		err := row.Scan(&b.kind, &b.target, &b.status)
		return b, err
	})
	if err != nil {
		return db.ComputerCommand{}, err
	}
	for _, b := range bindings {
		if b.status != "active" {
			return db.ComputerCommand{}, computer.ErrSecretUnavailable
		}
		if b.kind == "env" {
			if _, ok := normalized.env[b.target]; ok {
				return db.ComputerCommand{}, invalid("env cannot override computer secret %q", b.target)
			}
		}
	}
	var deleted, broken, ready, failed bool
	if err = tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,integrity_fault_at IS NOT NULL,initial_root_id IS NOT NULL,preparation_failed_at IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, request.EnvironmentID, request.ComputerID).Scan(&deleted, &broken, &ready, &failed); err != nil {
		return db.ComputerCommand{}, err
	}
	if broken {
		return db.ComputerCommand{}, computer.ErrRecoveryRequired
	}
	if deleted {
		return db.ComputerCommand{}, computer.ErrDeleting
	}
	if failed {
		return db.ComputerCommand{}, computer.ErrPreparationExhausted
	}
	if !ready {
		return db.ComputerCommand{}, computer.ErrBusy
	}
	var revoked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2)`, request.EnvironmentID, request.ComputerID).Scan(&revoked); err != nil {
		return db.ComputerCommand{}, err
	}
	if revoked {
		return db.ComputerCommand{}, computer.ErrSecretUnavailable
	}

	created, err := q.CreateComputerCommand(ctx, db.CreateComputerCommandParams{
		ID:                   pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:        pgvalue.UUID(request.EnvironmentID),
		ComputerID:           pgvalue.UUID(request.ComputerID),
		Argv:                 normalized.argv,
		Cwd:                  pgvalue.Text(normalized.cwd),
		Env:                  normalized.envJSON,
		TimeoutMs:            normalized.timeoutMS,
		Stdin:                normalized.stdin,
		CreatedBySubjectType: request.Creator.SubjectType,
		CreatedBySubjectID:   request.Creator.SubjectID,
	})
	if err != nil {
		return db.ComputerCommand{}, fmt.Errorf("create computer exec: %w", err)
	}
	receipt, err := json.Marshal(createReceipt{CommandID: pgvalue.UUIDString(created.ID)})
	if err != nil {
		return db.ComputerCommand{}, err
	}
	if _, err := claims.Complete(ctx, acquired.Claim, idempotency.Target{CommandID: uuid.UUID(created.ID.Bytes)}, receipt); err != nil {
		return db.ComputerCommand{}, err
	}
	return created, nil
}
