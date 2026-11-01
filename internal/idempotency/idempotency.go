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
	"github.com/helmrdotdev/helmr/internal/pglock"
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
	operationTemplateRefresh    operation = "template.refresh"
	operationSecretCreate       operation = "secret.create"
	operationSecretRotate       operation = "secret.rotate"
	operationSecretRevoke       operation = "secret.revoke"
	operationComputerCreate     operation = "computer.create"
	operationComputerCommand    operation = "computer.exec"
	operationComputerDelete     operation = "computer.delete"
	operationCommandCancel      operation = "command.cancel"
)

type Request struct {
	environmentID uuid.UUID
	operation     operation
	scope         []byte
	key           string
	fingerprint   [sha256.Size]byte
}

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

type Result struct {
	Claim db.IdempotencyReceipt
	New   bool
}
type Target struct{ DeploymentID, SecretID, SecretVersionID, ComputerID, CommandID uuid.UUID }

// Completion is the target and response saved with a successful mutation.
type Completion struct {
	Target  Target
	Receipt []byte
}

// Run owns a pgx savepoint inside the caller's transaction. The scoped advisory
// lock belongs to the caller transaction and survives rollback of this unit.
// The callback uses only its supplied transaction for domain writes. Returning an
// error, panicking, or omitting a new receipt rolls back those writes even when
// an outer caller ignores the error. A nil request performs an unkeyed mutation.
func Run(ctx context.Context, caller pgx.Tx, input *Request, fn func(pgx.Tx, Result) (Completion, error)) error {
	if fn == nil {
		return errors.New("idempotency mutation is required")
	}
	if caller == nil {
		return errors.New("idempotency transaction is required")
	}
	var slot [sha256.Size]byte
	if input != nil {
		if input.environmentID == uuid.Nil() || !supportedOperation(input.operation) || input.key == "" || len(input.key) > 512 || !utf8.ValidString(input.key) || strings.ContainsFunc(input.key, unicode.IsControl) {
			return errors.New("invalid platform retry identity")
		}
		slot = idempotencySlotHash(input)
		// Separate lock/read statements under READ COMMITTED let a waiter see
		// the previous winner. A hash collision only serializes unrelated keys.
		key := pglock.Key("platform-retry:" + hex.EncodeToString(slot[:]))
		if _, err := caller.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return fmt.Errorf("lock platform retry identity: %w", err)
		}
	}
	return db.RunTx(ctx, caller, func(tx pgx.Tx) error {
		acquired := Result{New: true}
		if input != nil {
			var err error
			acquired, err = acquire(ctx, tx, input, slot)
			if err != nil {
				return err
			}
		}
		completion, err := fn(tx, acquired)
		if err != nil {
			return err
		}
		if input == nil || !acquired.New {
			return nil
		}
		if err := validateReceipt(completion.Receipt); err != nil {
			return err
		}
		claim := acquired.Claim
		target := completion.Target
		_, err = db.New(tx).CreateIdempotencyReceipt(ctx, db.CreateIdempotencyReceiptParams{
			ID: claim.ID, EnvironmentID: claim.EnvironmentID, Operation: claim.Operation,
			SlotHash: claim.SlotHash, RequestFingerprint: claim.RequestFingerprint,
			ScopeSecretName: claim.ScopeSecretName, ScopeSecretID: claim.ScopeSecretID,
			ScopeComputerID: claim.ScopeComputerID, ScopeCommandID: claim.ScopeCommandID,
			ScopeComputerKey: claim.ScopeComputerKey,
			DeploymentID:     optionalUUID(target.DeploymentID), SecretID: optionalUUID(target.SecretID),
			SecretVersionID: optionalUUID(target.SecretVersionID), ComputerID: optionalUUID(target.ComputerID),
			CommandID: optionalUUID(target.CommandID), Receipt: bytes.Clone(completion.Receipt),
		})
		return err
	})
}

func acquire(ctx context.Context, tx pgx.Tx, r *Request, slot [sha256.Size]byte) (Result, error) {
	locked, err := db.New(tx).LockIdempotencyReceipt(ctx, db.LockIdempotencyReceiptParams{EnvironmentID: pgvalue.UUID(r.environmentID), Operation: string(r.operation), SlotHash: slot[:]})
	if err == nil {
		claim := locked.IdempotencyReceipt
		if !bytes.Equal(claim.RequestFingerprint, r.fingerprint[:]) {
			return Result{}, ConflictError{ClaimID: pgvalue.MustUUIDValue(claim.ID)}
		}
		if claim.ReceiptPrunedAt.Valid || (locked.Expired.Valid && locked.Expired.Bool) {
			return Result{}, ExpiredError{}
		}
		return Result{Claim: claim}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("read idempotency receipt: %w", err)
	}
	claim, err := newClaim(r, slot)
	return Result{Claim: claim, New: true}, err
}

func newClaim(r *Request, slot [sha256.Size]byte) (db.IdempotencyReceipt, error) {
	params := db.IdempotencyReceipt{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(r.environmentID), Operation: string(r.operation), SlotHash: slot[:], RequestFingerprint: bytes.Clone(r.fingerprint[:])}
	switch r.operation {
	case operationSecretCreate:
		if len(r.scope) < 8 || binary.BigEndian.Uint64(r.scope[:8]) != uint64(len(r.scope)-8) {
			return db.IdempotencyReceipt{}, errors.New("invalid Secret name scope")
		}
		params.ScopeSecretName = pgvalue.Text(string(r.scope[8:]))
	case operationSecretRotate, operationSecretRevoke, operationComputerDelete, operationComputerCommand, operationCommandCancel:
		if len(r.scope) != 16 {
			return db.IdempotencyReceipt{}, errors.New("invalid platform object scope")
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
			return db.IdempotencyReceipt{}, errors.New("invalid external Computer scope")
		}
		params.ScopeComputerKey = pgvalue.Text(name)
	}
	return params, nil
}
func supportedOperation(value operation) bool {
	switch value {
	case operationDeploymentFinalize, operationTemplateRefresh, operationSecretCreate, operationSecretRotate, operationSecretRevoke, operationComputerCreate, operationComputerDelete, operationComputerCommand, operationCommandCancel:
		return true
	}
	return false
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
) (*Request, error) {
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
	return &Request{
		environmentID: environmentID,
		operation:     operationDeploymentFinalize,
		scope:         bytes.Clone(projectID[:]),
		key:           key,
		fingerprint:   operationFingerprint(operationDeploymentFinalize, canonical),
	}, nil
}

func NewSecretCreateRequest(environmentID uuid.UUID, name string, key string) (*Request, error) {
	if name == "" {
		return nil, errors.New("secret name is required")
	}
	return newSecretValueRequest(environmentID, operationSecretCreate, secretNameScope(name), key, []byte(name))
}

func NewSecretRotateRequest(environmentID uuid.UUID, secretID uuid.UUID, key string) (*Request, error) {
	if secretID == uuid.Nil() {
		return nil, errors.New("secret ID is required")
	}
	return newSecretValueRequest(environmentID, operationSecretRotate, secretID[:], key, nil)
}

func NewSecretRevokeRequest(environmentID uuid.UUID, secretID uuid.UUID, key string) (*Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if secretID == uuid.Nil() {
		return nil, errors.New("secret ID is required")
	}
	return &Request{
		environmentID: environmentID,
		operation:     operationSecretRevoke,
		scope:         bytes.Clone(secretID[:]),
		key:           key,
		fingerprint:   operationFingerprint(operationSecretRevoke, nil),
	}, nil
}

func NewExternalComputerCreateRequest(
	environmentID uuid.UUID,
	computerDeclaredID string,
	key string,
	input ComputerCreateFingerprint,
) (*Request, error) {
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
) (*Request, error) {
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
	return &Request{
		environmentID: environmentID,
		operation:     operationComputerCreate,
		scope:         scope,
		key:           key,
		fingerprint:   operationFingerprint(operationComputerCreate, canonicalFields),
	}, nil
}

func NewComputerDeleteRequest(environmentID uuid.UUID, computerID uuid.UUID, key string) (*Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	if computerID == uuid.Nil() {
		return nil, errors.New("computer ID is required")
	}
	return &Request{
		environmentID: environmentID,
		operation:     operationComputerDelete,
		scope:         bytes.Clone(computerID[:]),
		key:           key,
		fingerprint:   operationFingerprint(operationComputerDelete, nil),
	}, nil
}

func NewComputerCommandRequest(
	environmentID uuid.UUID,
	computerID uuid.UUID,
	key string,
	input ComputerCommandFingerprint,
) (*Request, error) {
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
	return &Request{
		environmentID: environmentID,
		operation:     operationComputerCommand,
		scope:         bytes.Clone(computerID[:]),
		key:           key,
		fingerprint:   operationFingerprint(operationComputerCommand, canonical),
	}, nil
}

func canonicalJSONOr(value json.RawMessage, fallback string) ([]byte, error) {
	if len(value) == 0 {
		value = json.RawMessage(fallback)
	}
	return jsoncanon.Transform(value)
}

func newSecretValueRequest(environmentID uuid.UUID, operation operation, scope []byte, key string, fields []byte) (*Request, error) {
	if environmentID == uuid.Nil() {
		return nil, errors.New("idempotency environment is required")
	}
	return &Request{
		environmentID: environmentID,
		operation:     operation,
		scope:         bytes.Clone(scope),
		key:           key,
		fingerprint:   operationFingerprint(operation, fields),
	}, nil
}

func idempotencySlotHash(request *Request) [sha256.Size]byte {
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

func NewCommandCancelRequest(environmentID, commandID uuid.UUID) (*Request, error) {
	if environmentID == uuid.Nil() || commandID == uuid.Nil() {
		return nil, errors.New("command cancellation requires environment and Command IDs")
	}
	return &Request{environmentID: environmentID, operation: operationCommandCancel, scope: bytes.Clone(commandID[:]), key: "cancel", fingerprint: operationFingerprint(operationCommandCancel, nil)}, nil
}

func optionalUUID(value uuid.UUID) pgtype.UUID {
	if value == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(value)
}

// NewTemplateRefreshRequest keeps retries attached to the originally resolved
// deployment and spec through the saved receipt, even after current changes.
func NewTemplateRefreshRequest(environmentID uuid.UUID, template, key string) (*Request, error) {
	if environmentID == uuid.Nil() || template == "" || strings.TrimSpace(key) == "" || len(key) > 512 || !utf8.ValidString(key) || strings.ContainsFunc(key, unicode.IsControl) {
		return nil, errors.New("template refresh requires a valid idempotency key")
	}
	return &Request{environmentID: environmentID, operation: operationTemplateRefresh, scope: []byte(template), key: key, fingerprint: operationFingerprint(operationTemplateRefresh, []byte(template))}, nil
}
