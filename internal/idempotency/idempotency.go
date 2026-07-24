package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const (
	scopeDomain       = "helmr.idempotency-scope.v0"
	keyDomain         = "helmr.idempotency-key.v0"
	lockDomain        = "helmr.idempotency-lock.v0"
	fingerprintDomain = "helmr.idempotency-fingerprint.v0"
)

type operation string

const (
	operationSecretCreate      operation = "secret.create"
	operationSecretRotate      operation = "secret.rotate"
	operationSecretRevoke      operation = "secret.revoke"
	operationActorStart        operation = "actor.start"
	operationActorInputSend    operation = "actor.input.send"
	operationActorOutputAppend operation = "actor.output.append"
	operationActorClose        operation = "actor.close"
	operationTaskStart         operation = "task.start"
	operationTokenCreate       operation = "token.create"
	operationTokenComplete     operation = "token.complete"
	operationTokenCancel       operation = "token.cancel"
	operationWorkspaceCreate   operation = "workspace.create"
	operationWorkspaceExec     operation = "workspace.exec"
	operationWorkspaceDelete   operation = "workspace.delete"
)

type Manager struct {
	hashes    keyedhash.Keyring
	authority keyedhash.Authority
}

type Transaction struct {
	manager Manager
	store   claimStore
	queries *db.Queries
}

type Request interface {
	idempotencyRequest() request
}

type request struct {
	environmentID uuid.UUID
	operation     operation
	scope         []byte
	key           string
	fingerprint   func(hashKeyVersion int32) ([sha256.Size]byte, error)
}

type sealedRequest struct {
	value request
}

func (r sealedRequest) idempotencyRequest() request {
	return r.value
}

type Result struct {
	Claim db.IdempotencyClaim
	New   bool
}

type ActorStartFingerprint struct {
	Key                   *string
	InputPresent          bool
	Input                 json.RawMessage
	WorkspaceAddress      json.RawMessage
	ManagedQueueName      string
	ManagedConcurrencyKey *string
	ManagedPriority       int32
	ManagedQueuedTTLMS    *int64
	ManagedRetryPolicy    json.RawMessage
	ManagedRunMetadata    json.RawMessage
	ManagedRunTags        []string
}

type TokenCreateFingerprint struct {
	TimeoutMS *int64
	Metadata  json.RawMessage
	Tags      []string
}

type TaskStartFingerprint struct {
	PayloadPresent bool
	Payload        json.RawMessage
	Workspace      json.RawMessage
	QueueName      string
	ConcurrencyKey *string
	Priority       int32
	QueuedTTLMS    *int64
	RetryPolicy    json.RawMessage
	Metadata       json.RawMessage
	Tags           []string
}

type WorkspaceCreateFingerprint struct {
	Key     *string
	Secrets json.RawMessage
}

type WorkspaceExecFingerprint struct {
	Command   []string
	Cwd       string
	Env       json.RawMessage
	StdinHash [sha256.Size]byte
	TimeoutMS int64
}

type ConflictError struct {
	ClaimID uuid.UUID
}

func (e ConflictError) Error() string {
	return fmt.Sprintf("idempotency key conflicts with claim %s", e.ClaimID)
}

func New(hashes keyedhash.Keyring) Manager {
	return Manager{hashes: hashes, authority: keyedhash.NewAuthority(hashes)}
}

func NewSecretCreateRequest(environmentID uuid.UUID, name string, key string, authenticator func(int32) ([sha256.Size]byte, error)) (Request, error) {
	if name == "" {
		return nil, errors.New("secret name is required")
	}
	return newSecretValueRequest(environmentID, operationSecretCreate, secretNameScope(name), key, []byte(name), authenticator)
}

func NewSecretRotateRequest(environmentID uuid.UUID, secretID uuid.UUID, key string, authenticator func(int32) ([sha256.Size]byte, error)) (Request, error) {
	if secretID == uuid.Nil {
		return nil, errors.New("secret ID is required")
	}
	return newSecretValueRequest(environmentID, operationSecretRotate, secretID[:], key, nil, authenticator)
}

func NewSecretRevokeRequest(environmentID uuid.UUID, secretID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if secretID == uuid.Nil {
		return nil, errors.New("secret ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationSecretRevoke,
		scope:         bytes.Clone(secretID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationSecretRevoke, nil, 0, nil), nil
		},
	}}, nil
}

func NewActorInputSendRequest(environmentID uuid.UUID, actorID uuid.UUID, key string, inputJSON []byte) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("actor ID is required")
	}
	canonicalInput, err := jsoncanon.Transform(inputJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Actor input: %w", err)
	}
	input := bytes.Clone(canonicalInput)
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorInputSend,
		scope:         bytes.Clone(actorID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorInputSend, input, 0, nil), nil
		},
	}}, nil
}

func NewActorOutputAppendRequest(
	environmentID uuid.UUID,
	actorID uuid.UUID,
	key string,
	dataJSON []byte,
	contentType string,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("actor ID is required")
	}
	canonicalData, err := jsoncanon.Transform(dataJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Actor output: %w", err)
	}
	fields, err := json.Marshal(struct {
		Data        json.RawMessage `json:"data"`
		ContentType string          `json:"contentType"`
	}{
		Data:        canonicalData,
		ContentType: contentType,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Actor output fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Actor output fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorOutputAppend,
		scope:         bytes.Clone(actorID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorOutputAppend, canonical, 0, nil), nil
		},
	}}, nil
}

func NewActorCloseRequest(environmentID uuid.UUID, actorID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("actor ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorClose,
		scope:         bytes.Clone(actorID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorClose, nil, 0, nil), nil
		},
	}}, nil
}

func NewRuntimeTokenCreateRequest(
	environmentID uuid.UUID,
	runID uuid.UUID,
	key string,
	input TokenCreateFingerprint,
) (Request, error) {
	if runID == uuid.Nil {
		return nil, errors.New("Token creating Run ID is required")
	}
	scope := append([]byte("runtime\x00"), runID[:]...)
	return newTokenCreateRequest(environmentID, scope, key, input)
}

func NewExternalTokenCreateRequest(
	environmentID uuid.UUID,
	key string,
	input TokenCreateFingerprint,
) (Request, error) {
	return newTokenCreateRequest(environmentID, []byte("external"), key, input)
}

func newTokenCreateRequest(
	environmentID uuid.UUID,
	scope []byte,
	key string,
	input TokenCreateFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	metadata, err := canonicalJSONOr(input.Metadata, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Token metadata: %w", err)
	}
	fields, err := json.Marshal(struct {
		TimeoutMS *int64          `json:"timeoutMs"`
		Metadata  json.RawMessage `json:"metadata"`
		Tags      []string        `json:"tags"`
	}{
		TimeoutMS: input.TimeoutMS,
		Metadata:  metadata,
		Tags:      append([]string{}, input.Tags...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode Token create fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Token create fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTokenCreate,
		scope:         bytes.Clone(scope),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationTokenCreate, canonical, 0, nil), nil
		},
	}}, nil
}

func NewTokenCompleteRequest(
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	key string,
	resultJSON []byte,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if tokenID == uuid.Nil {
		return nil, errors.New("Token ID is required")
	}
	canonical, err := jsoncanon.Transform(resultJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Token result: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTokenComplete,
		scope:         bytes.Clone(tokenID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationTokenComplete, canonical, 0, nil), nil
		},
	}}, nil
}

func NewTokenCancelRequest(environmentID uuid.UUID, tokenID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if tokenID == uuid.Nil {
		return nil, errors.New("Token ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTokenCancel,
		scope:         bytes.Clone(tokenID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationTokenCancel, nil, 0, nil), nil
		},
	}}, nil
}

func NewActorStartRequest(
	environmentID uuid.UUID,
	actorDeclaredID string,
	key string,
	input ActorStartFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if actorDeclaredID == "" {
		return nil, errors.New("actor declared ID is required")
	}
	workspace, err := jsoncanon.Transform(input.WorkspaceAddress)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Actor start Workspace address: %w", err)
	}
	runMetadata, err := canonicalJSONOr(input.ManagedRunMetadata, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize managed Run metadata: %w", err)
	}
	var retryPolicy json.RawMessage
	if len(input.ManagedRetryPolicy) > 0 {
		retryPolicy, err = jsoncanon.Transform(input.ManagedRetryPolicy)
		if err != nil {
			return nil, fmt.Errorf("canonicalize managed Run retry policy: %w", err)
		}
	}
	var initialInput json.RawMessage
	if input.InputPresent {
		initialInput, err = jsoncanon.Transform(input.Input)
		if err != nil {
			return nil, fmt.Errorf("canonicalize initial Actor input: %w", err)
		}
	}
	fields, err := json.Marshal(struct {
		ActorDeclaredID       string          `json:"actorDeclaredId"`
		Key                   *string         `json:"key"`
		InputPresent          bool            `json:"inputPresent"`
		Input                 json.RawMessage `json:"input"`
		WorkspaceAddress      json.RawMessage `json:"workspaceAddress"`
		ManagedQueueName      string          `json:"managedQueueName"`
		ManagedConcurrencyKey *string         `json:"managedConcurrencyKey"`
		ManagedPriority       int32           `json:"managedPriority"`
		ManagedQueuedTTLMS    *int64          `json:"managedQueuedTtlMs"`
		ManagedRetryPolicy    json.RawMessage `json:"managedRetryPolicy"`
		ManagedRunMetadata    json.RawMessage `json:"managedRunMetadata"`
		ManagedRunTags        []string        `json:"managedRunTags"`
	}{
		ActorDeclaredID: actorDeclaredID, Key: input.Key,
		InputPresent: input.InputPresent, Input: initialInput,
		WorkspaceAddress: workspace,
		ManagedQueueName: input.ManagedQueueName, ManagedConcurrencyKey: input.ManagedConcurrencyKey,
		ManagedPriority: input.ManagedPriority, ManagedQueuedTTLMS: input.ManagedQueuedTTLMS,
		ManagedRetryPolicy: retryPolicy, ManagedRunMetadata: runMetadata,
		ManagedRunTags: append([]string{}, input.ManagedRunTags...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode Actor start fingerprint: %w", err)
	}
	canonicalFields, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Actor start fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorStart,
		scope:         []byte(actorDeclaredID),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorStart, canonicalFields, 0, nil), nil
		},
	}}, nil
}

func NewWorkspaceCreateRequest(
	environmentID uuid.UUID,
	workspaceDeclaredID string,
	key string,
	input WorkspaceCreateFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if workspaceDeclaredID == "" {
		return nil, errors.New("Workspace declared ID is required")
	}
	secrets, err := canonicalJSONOr(input.Secrets, `[]`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Workspace Secret placements: %w", err)
	}
	fields, err := json.Marshal(struct {
		DeclaredID string          `json:"declaredId"`
		Key        *string         `json:"key"`
		Secrets    json.RawMessage `json:"secrets"`
	}{
		DeclaredID: workspaceDeclaredID,
		Key:        input.Key,
		Secrets:    secrets,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Workspace create fingerprint: %w", err)
	}
	canonicalFields, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Workspace create fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationWorkspaceCreate,
		scope:         []byte(workspaceDeclaredID),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationWorkspaceCreate, canonicalFields, 0, nil), nil
		},
	}}, nil
}

func NewWorkspaceDeleteRequest(environmentID uuid.UUID, workspaceID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if workspaceID == uuid.Nil {
		return nil, errors.New("Workspace ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationWorkspaceDelete,
		scope:         bytes.Clone(workspaceID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationWorkspaceDelete, nil, 0, nil), nil
		},
	}}, nil
}

func NewWorkspaceExecRequest(
	environmentID uuid.UUID,
	workspaceID uuid.UUID,
	key string,
	input WorkspaceExecFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if workspaceID == uuid.Nil {
		return nil, errors.New("Workspace ID is required")
	}
	env, err := canonicalJSONOr(input.Env, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Workspace exec environment: %w", err)
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
		return nil, fmt.Errorf("encode Workspace exec fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Workspace exec fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationWorkspaceExec,
		scope:         bytes.Clone(workspaceID[:]),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationWorkspaceExec, canonical, 0, nil), nil
		},
	}}, nil
}

func NewTaskStartRequest(
	environmentID uuid.UUID,
	taskDeclaredID string,
	key string,
	input TaskStartFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if taskDeclaredID == "" {
		return nil, errors.New("Task declared ID is required")
	}
	workspace, err := jsoncanon.Transform(input.Workspace)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Task start Workspace: %w", err)
	}
	metadata, err := canonicalJSONOr(input.Metadata, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Task metadata: %w", err)
	}
	var payload json.RawMessage
	if input.PayloadPresent {
		payload, err = jsoncanon.Transform(input.Payload)
		if err != nil {
			return nil, fmt.Errorf("canonicalize Task payload: %w", err)
		}
	}
	var retry json.RawMessage
	if len(input.RetryPolicy) > 0 {
		retry, err = jsoncanon.Transform(input.RetryPolicy)
		if err != nil {
			return nil, fmt.Errorf("canonicalize Task retry policy: %w", err)
		}
	}
	fields, err := json.Marshal(struct {
		TaskDeclaredID string          `json:"taskDeclaredId"`
		PayloadPresent bool            `json:"payloadPresent"`
		Payload        json.RawMessage `json:"payload"`
		Workspace      json.RawMessage `json:"workspace"`
		QueueName      string          `json:"queueName"`
		ConcurrencyKey *string         `json:"concurrencyKey"`
		Priority       int32           `json:"priority"`
		QueuedTTLMS    *int64          `json:"queuedTtlMs"`
		RetryPolicy    json.RawMessage `json:"retryPolicy"`
		Metadata       json.RawMessage `json:"metadata"`
		Tags           []string        `json:"tags"`
	}{
		TaskDeclaredID: taskDeclaredID,
		PayloadPresent: input.PayloadPresent,
		Payload:        payload, Workspace: workspace,
		QueueName: input.QueueName, ConcurrencyKey: input.ConcurrencyKey,
		Priority: input.Priority, QueuedTTLMS: input.QueuedTTLMS,
		RetryPolicy: retry, Metadata: metadata,
		Tags: append([]string{}, input.Tags...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode Task start fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Task start fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTaskStart,
		scope:         []byte(taskDeclaredID),
		key:           key,
		fingerprint: func(int32) ([sha256.Size]byte, error) {
			return operationFingerprint(operationTaskStart, canonical, 0, nil), nil
		},
	}}, nil
}

func canonicalJSONOr(value json.RawMessage, fallback string) ([]byte, error) {
	if len(value) == 0 {
		value = json.RawMessage(fallback)
	}
	return jsoncanon.Transform(value)
}

func newSecretValueRequest(environmentID uuid.UUID, operation operation, scope []byte, key string, fields []byte, authenticator func(int32) ([sha256.Size]byte, error)) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if authenticator == nil {
		return nil, errors.New("secret value authenticator is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operation,
		scope:         bytes.Clone(scope),
		key:           key,
		fingerprint: func(version int32) ([sha256.Size]byte, error) {
			valueAuthenticator, err := authenticator(version)
			if err != nil {
				return [sha256.Size]byte{}, err
			}
			return operationFingerprint(operation, fields, version, valueAuthenticator[:]), nil
		},
	}}, nil
}

func (m Manager) Transaction(tx pgx.Tx) (*Transaction, error) {
	if tx == nil {
		return nil, errors.New("idempotency transaction is required")
	}
	queries := db.New(tx)
	return &Transaction{manager: m, store: queries, queries: queries}, nil
}

func (m Manager) TransactionForQueries(queries db.Querier) (*Transaction, error) {
	if queries == nil {
		return nil, errors.New("idempotency query transaction is required")
	}
	return &Transaction{manager: m, store: queries}, nil
}

func (t *Transaction) Queries() *db.Queries {
	return t.queries
}

func (t *Transaction) Acquire(ctx context.Context, input Request) (Result, error) {
	if input == nil {
		return Result{}, errors.New("idempotency request is required")
	}
	request := input.idempotencyRequest()
	if request.environmentID == uuid.Nil {
		return Result{}, errors.New("idempotency environment is required")
	}
	if !supportedOperation(request.operation) {
		return Result{}, fmt.Errorf("unsupported idempotency operation %q", request.operation)
	}
	if request.key == "" {
		return Result{}, errors.New("idempotency key is required")
	}
	if request.fingerprint == nil {
		return Result{}, errors.New("idempotency fingerprint function is required")
	}

	if err := t.store.LockIdempotencySlot(ctx, lockKey(request)); err != nil {
		return Result{}, fmt.Errorf("lock idempotency slot: %w", err)
	}
	selection, err := t.manager.authority.Lock(ctx, t.store)
	if err != nil {
		return Result{}, err
	}
	candidates, err := t.candidates(request, selection.Versions)
	if err != nil {
		return Result{}, err
	}
	claims, err := t.store.FindLiveIdempotencyClaims(ctx, db.FindLiveIdempotencyClaimsParams{
		HashKeyVersions: candidates.versions,
		ScopeHashes:     candidates.scopeHashes,
		KeyHashes:       candidates.keyHashes,
		EnvironmentID:   pgvalue.UUID(request.environmentID),
		Operation:       string(request.operation),
	})
	if err != nil {
		return Result{}, fmt.Errorf("find idempotency claim: %w", err)
	}
	if len(claims) > 1 {
		return Result{}, errors.New("multiple live idempotency claims matched one raw key")
	}
	if len(claims) == 0 {
		generation, err := t.store.GetLatestIdempotencyClaimGeneration(ctx, db.GetLatestIdempotencyClaimGenerationParams{
			HashKeyVersions: candidates.versions,
			ScopeHashes:     candidates.scopeHashes,
			KeyHashes:       candidates.keyHashes,
			EnvironmentID:   pgvalue.UUID(request.environmentID),
			Operation:       string(request.operation),
		})
		if err != nil {
			return Result{}, fmt.Errorf("read idempotency generation: %w", err)
		}
		return t.create(ctx, request, generation+1, selection.Current)
	}

	matched := claims[0]
	claim := claimFromRow(matched)
	if matched.Expired {
		if _, err := t.store.RetireExpiredIdempotencyClaim(ctx, db.RetireExpiredIdempotencyClaimParams{
			EnvironmentID: pgvalue.UUID(request.environmentID),
			ID:            claim.ID,
		}); err != nil {
			return Result{}, fmt.Errorf("retire idempotency claim: %w", err)
		}
		return t.create(ctx, request, claim.Generation+1, selection.Current)
	}
	fingerprint, err := request.fingerprint(claim.HashKeyVersion)
	if err != nil {
		return Result{}, fmt.Errorf("fingerprint idempotency request: %w", err)
	}
	if !bytes.Equal(claim.RequestFingerprint, fingerprint[:]) {
		claimID, _ := pgvalue.UUIDValue(claim.ID)
		return Result{}, ConflictError{ClaimID: claimID}
	}
	return Result{Claim: claim}, nil
}

func supportedOperation(value operation) bool {
	switch value {
	case operationSecretCreate, operationSecretRotate, operationSecretRevoke,
		operationActorStart, operationActorInputSend, operationActorOutputAppend, operationActorClose,
		operationTaskStart, operationTokenCreate, operationTokenComplete, operationTokenCancel,
		operationWorkspaceCreate, operationWorkspaceExec, operationWorkspaceDelete:
		return true
	default:
		return false
	}
}

func (t *Transaction) Complete(ctx context.Context, claim db.IdempotencyClaim, receipt []byte) (db.IdempotencyClaim, error) {
	if err := validateReceipt(receipt); err != nil {
		return db.IdempotencyClaim{}, err
	}
	completed, err := t.store.CompleteIdempotencyClaim(ctx, db.CompleteIdempotencyClaimParams{
		Receipt:            bytes.Clone(receipt),
		EnvironmentID:      claim.EnvironmentID,
		ID:                 claim.ID,
		RequestFingerprint: bytes.Clone(claim.RequestFingerprint),
	})
	if err != nil {
		return db.IdempotencyClaim{}, fmt.Errorf("complete idempotency claim: %w", err)
	}
	return completed, nil
}

func (t *Transaction) Fail(ctx context.Context, claim db.IdempotencyClaim, receipt []byte) (db.IdempotencyClaim, error) {
	if err := validateReceipt(receipt); err != nil {
		return db.IdempotencyClaim{}, err
	}
	failed, err := t.store.FailIdempotencyClaim(ctx, db.FailIdempotencyClaimParams{
		Receipt:            bytes.Clone(receipt),
		EnvironmentID:      claim.EnvironmentID,
		ID:                 claim.ID,
		RequestFingerprint: bytes.Clone(claim.RequestFingerprint),
	})
	if err != nil {
		return db.IdempotencyClaim{}, fmt.Errorf("fail idempotency claim: %w", err)
	}
	return failed, nil
}

func (t *Transaction) create(ctx context.Context, request request, generation int64, version int32) (Result, error) {
	fingerprint, err := request.fingerprint(version)
	if err != nil {
		return Result{}, fmt.Errorf("fingerprint idempotency request: %w", err)
	}
	scopeHash, keyHash, err := t.hashes(request, version)
	if err != nil {
		return Result{}, err
	}
	claim, err := t.store.CreateIdempotencyClaim(ctx, db.CreateIdempotencyClaimParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:      pgvalue.UUID(request.environmentID),
		Operation:          string(request.operation),
		ScopeHash:          scopeHash,
		KeyHash:            keyHash,
		HashKeyVersion:     version,
		Generation:         generation,
		RequestFingerprint: fingerprint[:],
	})
	if err != nil {
		return Result{}, fmt.Errorf("create idempotency claim: %w", err)
	}
	return Result{Claim: claim, New: true}, nil
}

type hashCandidates struct {
	versions    []int32
	scopeHashes [][]byte
	keyHashes   [][]byte
}

func (t *Transaction) candidates(request request, versions []int32) (hashCandidates, error) {
	result := hashCandidates{
		versions:    make([]int32, 0, len(versions)),
		scopeHashes: make([][]byte, 0, len(versions)),
		keyHashes:   make([][]byte, 0, len(versions)),
	}
	for _, version := range versions {
		scopeHash, keyHash, err := t.hashes(request, version)
		if err != nil {
			return hashCandidates{}, err
		}
		result.versions = append(result.versions, version)
		result.scopeHashes = append(result.scopeHashes, scopeHash)
		result.keyHashes = append(result.keyHashes, keyHash)
	}
	return result, nil
}

func (t *Transaction) hashes(request request, version int32) ([]byte, []byte, error) {
	scope, err := t.manager.hashes.Sum(version, lookupFrame(scopeDomain, request, request.scope))
	if err != nil {
		return nil, nil, fmt.Errorf("hash idempotency scope: %w", err)
	}
	key, err := t.manager.hashes.Sum(version, lookupFrame(keyDomain, request, []byte(request.key)))
	if err != nil {
		return nil, nil, fmt.Errorf("hash idempotency key: %w", err)
	}
	return bytes.Clone(scope.Value[:]), bytes.Clone(key.Value[:]), nil
}

func lookupFrame(domain string, request request, value []byte) []byte {
	frame := make([]byte, 0, len(domain)+1+16+8+len(request.operation)+8+len(value))
	frame = append(frame, domain...)
	frame = append(frame, 0)
	frame = append(frame, request.environmentID[:]...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.operation)))
	frame = append(frame, request.operation...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(value)))
	frame = append(frame, value...)
	return frame
}

func lockKey(request request) int64 {
	frame := make([]byte, 0, len(lockDomain)+1+16+8+len(request.operation)+8+len(request.scope)+8+len(request.key))
	frame = append(frame, lockDomain...)
	frame = append(frame, 0)
	frame = append(frame, request.environmentID[:]...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.operation)))
	frame = append(frame, request.operation...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.scope)))
	frame = append(frame, request.scope...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(request.key)))
	frame = append(frame, request.key...)
	sum := sha256.Sum256(frame)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func secretNameScope(name string) []byte {
	scope := make([]byte, 0, 8+len(name))
	scope = binary.BigEndian.AppendUint64(scope, uint64(len(name)))
	return append(scope, name...)
}

func operationFingerprint(operation operation, fields []byte, hashKeyVersion int32, authenticator []byte) [sha256.Size]byte {
	frame := make([]byte, 0, len(fingerprintDomain)+1+8+len(operation)+8+len(fields)+4+8+len(authenticator))
	frame = append(frame, fingerprintDomain...)
	frame = append(frame, 0)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(operation)))
	frame = append(frame, operation...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(fields)))
	frame = append(frame, fields...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(hashKeyVersion))
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(authenticator)))
	frame = append(frame, authenticator...)
	return sha256.Sum256(frame)
}

func claimFromRow(row db.FindLiveIdempotencyClaimsRow) db.IdempotencyClaim {
	return db.IdempotencyClaim{
		ID:                 row.ID,
		EnvironmentID:      row.EnvironmentID,
		Operation:          row.Operation,
		ScopeHash:          row.ScopeHash,
		KeyHash:            row.KeyHash,
		HashKeyVersion:     row.HashKeyVersion,
		Generation:         row.Generation,
		RequestFingerprint: row.RequestFingerprint,
		State:              row.State,
		Receipt:            row.Receipt,
		AcceptedAt:         row.AcceptedAt,
		ExpiresAt:          row.ExpiresAt,
		RetiredAt:          row.RetiredAt,
		CompletedAt:        row.CompletedAt,
	}
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

type claimStore interface {
	LockIdempotencySlot(context.Context, int64) error
	LockActiveLookupHMACVersions(context.Context) ([]db.LookupHmacVersion, error)
	FindLiveIdempotencyClaims(context.Context, db.FindLiveIdempotencyClaimsParams) ([]db.FindLiveIdempotencyClaimsRow, error)
	GetLatestIdempotencyClaimGeneration(context.Context, db.GetLatestIdempotencyClaimGenerationParams) (int64, error)
	CreateIdempotencyClaim(context.Context, db.CreateIdempotencyClaimParams) (db.IdempotencyClaim, error)
	RetireExpiredIdempotencyClaim(context.Context, db.RetireExpiredIdempotencyClaimParams) (db.IdempotencyClaim, error)
	CompleteIdempotencyClaim(context.Context, db.CompleteIdempotencyClaimParams) (db.IdempotencyClaim, error)
	FailIdempotencyClaim(context.Context, db.FailIdempotencyClaimParams) (db.IdempotencyClaim, error)
}
