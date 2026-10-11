package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	slotDomain        = "helmr.idempotency-slot.v0"
	fingerprintDomain = "helmr.idempotency-fingerprint.v0"
)

type operation string

const (
	operationDeploymentFinalize operation = "deployment.finalize"
	operationSecretCreate       operation = "secret.create"
	operationSecretRotate       operation = "secret.rotate"
	operationSecretRevoke       operation = "secret.revoke"
	operationComputerCreate     operation = "computer.create"
	operationComputerCommand    operation = "computer.exec"
	operationComputerDelete     operation = "computer.delete"
	operationCommandCancel      operation = "command.cancel"
)

type Request interface{ idempotencyRequest() request }
type request struct {
	environmentID uuid.UUID
	operation     operation
	scope         []byte
	key           string
	fingerprint   func() ([sha256.Size]byte, error)
}
type sealedRequest struct{ value request }

func (r sealedRequest) idempotencyRequest() request { return r.value }

type DeploymentFinalizeFingerprint struct {
	BundleDigest string `json:"bundleDigest"`
}
type ComputerCreateFingerprint struct {
	Key     *string
	Secrets json.RawMessage
}
type ComputerCommandFingerprint struct {
	Command   []string
	Cwd       string
	Env       json.RawMessage
	StdinHash [sha256.Size]byte
	TimeoutMS int64
}
type ExpiredError struct{}

func (ExpiredError) Error() string        { return "operation receipt has expired" }
func (ExpiredError) ErrorCode() string    { return "operation_expired" }
func (ExpiredError) ErrorRetryable() bool { return false }

type ConflictError struct{ ClaimID uuid.UUID }

func (e ConflictError) Error() string { return "idempotency key conflicts with its original request" }

var ErrIncomplete = errors.New("platform mutation receipt is incomplete")

type Result struct {
	Claim db.PlatformRetryKey
	New   bool
}
type Target struct{ DeploymentID, SecretID, SecretVersionID, ComputerID, CommandID uuid.UUID }
type Transaction struct{ queries *db.Queries }

func TransactionFor(tx pgx.Tx) (*Transaction, error) {
	if tx == nil {
		return nil, errors.New("idempotency transaction is required")
	}
	return &Transaction{queries: db.New(tx)}, nil
}

// Acquire serializes one scoped request. The enclosing transaction must bind its
// typed result and receipt before commit; the deferred constraint enforces that.
func (t *Transaction) Acquire(ctx context.Context, input Request) (Result, error) {
	if input == nil {
		return Result{}, errors.New("idempotency request is required")
	}
	r := input.idempotencyRequest()
	if r.environmentID == uuid.Nil() || !supportedOperation(r.operation) || r.key == "" || len(r.key) > 512 || !utf8.ValidString(r.key) || strings.ContainsFunc(r.key, unicode.IsControl) || r.fingerprint == nil {
		return Result{}, errors.New("invalid platform retry identity")
	}
	slot := idempotencySlotHash(r)
	fingerprint, err := r.fingerprint()
	if err != nil {
		return Result{}, err
	}
	for {
		locked, err := t.queries.LockPlatformRetryKey(ctx, db.LockPlatformRetryKeyParams{EnvironmentID: pgvalue.UUID(r.environmentID), Operation: string(r.operation), SlotHash: slot[:]})
		if err == nil {
			claim := locked.PlatformRetryKey
			if !bytes.Equal(claim.RequestFingerprint, fingerprint[:]) {
				return Result{}, ConflictError{ClaimID: pgvalue.MustUUIDValue(claim.ID)}
			}
			if claim.ReceiptPrunedAt.Valid || (locked.Expired.Valid && locked.Expired.Bool) {
				return Result{}, ExpiredError{}
			}
			if len(claim.Receipt) == 0 {
				return Result{}, ErrIncomplete
			}
			return Result{Claim: claim}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, fmt.Errorf("lock platform retry key: %w", err)
		}
		params := db.CreatePlatformRetryKeyParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(r.environmentID), Operation: string(r.operation), SlotHash: slot[:], RequestFingerprint: fingerprint[:]}
		switch r.operation {
		case operationSecretCreate:
			if len(r.scope) < 8 || binary.BigEndian.Uint64(r.scope[:8]) != uint64(len(r.scope)-8) {
				return Result{}, errors.New("invalid Secret name scope")
			}
			params.ScopeSecretName = pgvalue.Text(string(r.scope[8:]))
		case operationSecretRotate, operationSecretRevoke, operationComputerDelete, operationComputerCommand, operationCommandCancel:
			if len(r.scope) != 16 {
				return Result{}, errors.New("invalid platform object scope")
			}
			id := pgvalue.UUID(uuid.UUID(r.scope))
			switch r.operation {
			case operationSecretRotate, operationSecretRevoke:
				params.ScopeSecretID = id
			case operationComputerDelete, operationComputerCommand:
				params.ScopeComputerID = id
			case operationCommandCancel:
				params.ScopeCommandID = id
			}
		case operationComputerCreate:
			name, ok := strings.CutPrefix(string(r.scope), "external\x00")
			if !ok || name == "" {
				return Result{}, errors.New("invalid external Computer scope")
			}
			params.ScopeCallerKind = pgvalue.Text("external")
			params.ScopeComputerKey = pgvalue.Text(name)
		}
		claim, err := t.queries.CreatePlatformRetryKey(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("bind platform retry key: %w", err)
		}
		return Result{Claim: claim, New: true}, nil
	}
}
func supportedOperation(value operation) bool {
	switch value {
	case operationDeploymentFinalize, operationSecretCreate, operationSecretRotate, operationSecretRevoke, operationComputerCreate, operationComputerDelete, operationComputerCommand, operationCommandCancel:
		return true
	}
	return false
}
func (t *Transaction) Complete(ctx context.Context, claim db.PlatformRetryKey, target Target, receipt []byte) (db.PlatformRetryKey, error) {
	if err := validateReceipt(receipt); err != nil {
		return db.PlatformRetryKey{}, err
	}
	return t.queries.CompletePlatformRetryKey(ctx, db.CompletePlatformRetryKeyParams{EnvironmentID: claim.EnvironmentID, ID: claim.ID, RequestFingerprint: bytes.Clone(claim.RequestFingerprint), DeploymentID: optionalUUID(target.DeploymentID), SecretID: optionalUUID(target.SecretID), SecretVersionID: optionalUUID(target.SecretVersionID), ComputerID: optionalUUID(target.ComputerID), CommandID: optionalUUID(target.CommandID), Receipt: bytes.Clone(receipt)})
}
func deploymentDigestBytes(value string) ([]byte, error) {
	hexValue, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(hexValue) != sha256.Size*2 || strings.ToLower(hexValue) != hexValue {
		return nil, errors.New("deployment bundle digest must be a lowercase SHA-256 digest")
	}
	decoded, err := hex.DecodeString(hexValue)
	if err != nil {
		return nil, errors.New("deployment bundle digest must be a lowercase SHA-256 digest")
	}
	return decoded, nil
}

func NewDeploymentFinalizeRequest(
	environmentID uuid.UUID,
	projectID uuid.UUID,
	key string,
	fingerprint DeploymentFinalizeFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if projectID == uuid.Nil() {
		return nil, errors.New("project ID is required")
	}
	if _, err := deploymentDigestBytes(fingerprint.BundleDigest); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return nil, fmt.Errorf("encode deployment finalization fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize deployment finalization fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationDeploymentFinalize,
		scope:         bytes.Clone(projectID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationDeploymentFinalize, canonical), nil
		},
	}}, nil
}

func NewSecretCreateRequest(environmentID uuid.UUID, name string, key string) (Request, error) {
	if name == "" {
		return nil, errors.New("secret name is required")
	}
	return newSecretValueRequest(environmentID, operationSecretCreate, secretNameScope(name), key, []byte(name))
}

func NewSecretRotateRequest(environmentID uuid.UUID, secretID uuid.UUID, key string) (Request, error) {
	if secretID == uuid.Nil() {
		return nil, errors.New("secret ID is required")
	}
	return newSecretValueRequest(environmentID, operationSecretRotate, secretID[:], key, nil)
}

func NewSecretRevokeRequest(environmentID uuid.UUID, secretID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if secretID == uuid.Nil() {
		return nil, errors.New("secret ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationSecretRevoke,
		scope:         bytes.Clone(secretID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationSecretRevoke, nil), nil
		},
	}}, nil
}

func NewExternalComputerCreateRequest(
	environmentID uuid.UUID,
	computerDeclaredID string,
	key string,
	input ComputerCreateFingerprint,
) (Request, error) {
	return newComputerCreateRequest(
		environmentID,
		[]byte("external\x00"+computerDeclaredID),
		computerDeclaredID,
		key,
		input,
	)
}

func newComputerCreateRequest(
	environmentID uuid.UUID,
	scope []byte,
	computerDeclaredID string,
	key string,
	input ComputerCreateFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if computerDeclaredID == "" {
		return nil, errors.New("computer declared ID is required")
	}
	secrets, err := canonicalJSONOr(input.Secrets, `[]`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize computer secret placements: %w", err)
	}
	fields, err := json.Marshal(struct {
		DeclaredID string          `json:"declaredId"`
		Key        *string         `json:"key"`
		Secrets    json.RawMessage `json:"secrets"`
	}{
		DeclaredID: computerDeclaredID,
		Key:        input.Key,
		Secrets:    secrets,
	})
	if err != nil {
		return nil, fmt.Errorf("encode computer create fingerprint: %w", err)
	}
	canonicalFields, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize computer create fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationComputerCreate,
		scope:         scope,
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationComputerCreate, canonicalFields), nil
		},
	}}, nil
}

func NewComputerDeleteRequest(environmentID uuid.UUID, computerID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if computerID == uuid.Nil() {
		return nil, errors.New("computer ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationComputerDelete,
		scope:         bytes.Clone(computerID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationComputerDelete, nil), nil
		},
	}}, nil
}

func NewComputerCommandRequest(
	environmentID uuid.UUID,
	computerID uuid.UUID,
	key string,
	input ComputerCommandFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if computerID == uuid.Nil() {
		return nil, errors.New("computer ID is required")
	}
	env, err := canonicalJSONOr(input.Env, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize computer exec environment: %w", err)
	}
	fields, err := json.Marshal(struct {
		Command   []string        `json:"command"`
		Cwd       string          `json:"cwd"`
		Env       json.RawMessage `json:"env"`
		StdinHash string          `json:"stdinHash"`
		TimeoutMS int64           `json:"timeoutMs"`
	}{
		Command:   append([]string{}, input.Command...),
		Cwd:       input.Cwd,
		Env:       env,
		StdinHash: fmt.Sprintf("%x", input.StdinHash),
		TimeoutMS: input.TimeoutMS,
	})
	if err != nil {
		return nil, fmt.Errorf("encode computer exec fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize computer exec fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationComputerCommand,
		scope:         bytes.Clone(computerID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationComputerCommand, canonical), nil
		},
	}}, nil
}

func canonicalJSONOr(value json.RawMessage, fallback string) ([]byte, error) {
	if len(value) == 0 {
		value = json.RawMessage(fallback)
	}
	return jsoncanon.Transform(value)
}

func newSecretValueRequest(environmentID uuid.UUID, operation operation, scope []byte, key string, fields []byte) (Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operation,
		scope:         bytes.Clone(scope),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operation, fields), nil
		},
	}}, nil
}

func idempotencySlotHash(request request) [sha256.Size]byte {
	frame := make([]byte, 0, len(slotDomain)+1+16+8+len(request.operation)+8+len(request.scope)+8+len(request.key))
	frame = append(frame, slotDomain...)
	frame = append(frame, 0)
	frame = append(frame, request.environmentID[:]...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.operation)))
	frame = append(frame, request.operation...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.scope)))
	frame = append(frame, request.scope...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.key)))
	frame = append(frame, request.key...)
	return sha256.Sum256(frame)
}

func secretNameScope(name string) []byte {
	scope := make([]byte, 0, 8+len(name))
	scope = binary.BigEndian.AppendUint64(scope, uint64(len(name)))
	return append(scope, name...)
}

func operationFingerprint(operation operation, fields []byte) [sha256.Size]byte {
	frame := make([]byte, 0, len(fingerprintDomain)+1+8+len(operation)+8+len(fields))
	frame = append(frame, fingerprintDomain...)
	frame = append(frame, 0)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(operation)))
	frame = append(frame, operation...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(fields)))
	frame = append(frame, fields...)
	return sha256.Sum256(frame)
}

func validateReceipt(receipt []byte) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(receipt, &value); err != nil {
		return fmt.Errorf("idempotency receipt must be a JSON object: %w", err)
	}
	if value == nil {
		return errors.New("idempotency receipt must be a JSON object")
	}
	return nil
}

func NewCommandCancelRequest(environmentID, commandID uuid.UUID) (Request, error) {
	if environmentID == uuid.Nil() || commandID == uuid.Nil() {
		return nil, errors.New("command cancellation requires environment and Command IDs")
	}
	return sealedRequest{value: request{environmentID: environmentID, operation: operationCommandCancel, scope: bytes.Clone(commandID[:]), key: "cancel", fingerprint: func() ([sha256.Size]byte, error) { return operationFingerprint(operationCommandCancel, nil), nil }}}, nil
}

func optionalUUID(value uuid.UUID) pgtype.UUID {
	if value == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(value)
}
