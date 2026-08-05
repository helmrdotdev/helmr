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
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const (
	slotDomain        = "helmr.idempotency-slot.v0"
	fingerprintDomain = "helmr.idempotency-fingerprint.v0"
)

type operation string

const (
	operationDeploymentCreate    operation = "deployment.create"
	operationSecretCreate        operation = "secret.create"
	operationSecretRotate        operation = "secret.rotate"
	operationSecretRevoke        operation = "secret.revoke"
	operationRunMetadata         operation = "run.metadata"
	operationActorStart          operation = "actor.start"
	operationActorInputSend      operation = "session.input.send"
	operationActorOutputAppend   operation = "session.output.append"
	operationActorClose          operation = "session.close"
	operationTaskStart           operation = "task.start"
	operationTaskChildInvoke     operation = "task.child.invoke"
	operationTokenCreate         operation = "token.create"
	operationTokenComplete       operation = "token.complete"
	operationTokenCancel         operation = "token.cancel"
	operationWorkspaceCreate     operation = "workspace.create"
	operationWorkspaceExec       operation = "workspace.exec"
	operationWorkspaceDelete     operation = "workspace.delete"
	operationWorkspaceImageBuild operation = "workspace.image.build"
)

type Transaction struct {
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
	fingerprint   func() ([sha256.Size]byte, error)
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

type DeploymentCreateFingerprint struct {
	SourceDigest         string `json:"sourceDigest"`
	LockfileDigest       string `json:"lockfileDigest"`
	LockfileName         string `json:"lockfileName"`
	NodeVersion          string `json:"nodeVersion"`
	ManagerName          string `json:"managerName"`
	ManagerVersion       string `json:"managerVersion"`
	ManagerIntegrity     string `json:"managerIntegrity,omitempty"`
	BuildContractVersion string `json:"buildContractVersion"`
	ImageCacheMode       string `json:"imageCacheMode"`
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

type TaskChildInvokeFingerprint struct {
	Method         string
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

type WorkspaceImageBuildFingerprint struct {
	Architecture           string                            `json:"architecture"`
	PlanDigest             string                            `json:"planDigest"`
	SubmittedSourceDigest  string                            `json:"submittedSourceDigest"`
	BuildTreeDigest        string                            `json:"buildTreeDigest"`
	BuildTreeSizeBytes     int64                             `json:"buildTreeSizeBytes"`
	AdmittedPathSetDigest  string                            `json:"admittedPathSetDigest"`
	SourceArchiveDigest    string                            `json:"sourceArchiveDigest"`
	SourceArchiveSizeBytes int64                             `json:"sourceArchiveSizeBytes"`
	SourceArchiveEntries   int                               `json:"sourceArchiveEntries"`
	ImageCacheMode         string                            `json:"imageCacheMode"`
	CacheScope             string                            `json:"cacheScope"`
	ExecutionABI           string                            `json:"executionAbi"`
	LLBABI                 string                            `json:"llbAbi"`
	CacheABI               string                            `json:"cacheAbi"`
	Quotas                 WorkspaceImageBuildQuotas         `json:"quotas"`
	Output                 WorkspaceImageBuildOutputContract `json:"output"`
}

type WorkspaceImageBuildQuotas struct {
	CPUMillis               int64 `json:"cpuMillis"`
	MemoryBytes             int64 `json:"memoryBytes"`
	ScratchBytes            int64 `json:"scratchBytes"`
	PIDs                    int64 `json:"pids"`
	MaxSourceArchiveBytes   int64 `json:"maxSourceArchiveBytes"`
	MaxSourceArchiveEntries int   `json:"maxSourceArchiveEntries"`
	MaxOCIArchiveBytes      int64 `json:"maxOciArchiveBytes"`
}

type WorkspaceImageBuildOutputContract struct {
	Architecture string `json:"architecture"`
	MediaType    string `json:"mediaType"`
	MaxSizeBytes int64  `json:"maxSizeBytes"`
}

type ConflictError struct {
	ClaimID uuid.UUID
}

func (e ConflictError) Error() string {
	return fmt.Sprintf("idempotency key conflicts with claim %s", e.ClaimID)
}

func NewDeploymentCreateRequest(
	environmentID uuid.UUID,
	projectID uuid.UUID,
	key string,
	fingerprint DeploymentCreateFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if projectID == uuid.Nil {
		return nil, errors.New("project ID is required")
	}
	if fingerprint.ImageCacheMode != "prefer" && fingerprint.ImageCacheMode != "bypass" {
		return nil, errors.New("deployment image cache mode is invalid")
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return nil, fmt.Errorf("encode deployment creation fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize deployment creation fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationDeploymentCreate,
		scope:         bytes.Clone(projectID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationDeploymentCreate, canonical), nil
		},
	}}, nil
}

func NewWorkspaceImageBuildRequest(
	environmentID uuid.UUID,
	buildLeaseID uuid.UUID,
	buildLeaseGeneration int64,
	declarationSlot string,
	fingerprint WorkspaceImageBuildFingerprint,
) (Request, error) {
	authority, err := workspaceImageBuildAuthority(
		environmentID,
		buildLeaseID,
		buildLeaseGeneration,
		declarationSlot,
	)
	if err != nil {
		return nil, err
	}
	if err := validateWorkspaceImageBuildFingerprint(fingerprint); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return nil, fmt.Errorf("encode workspace image build fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace image build fingerprint: %w", err)
	}
	authority.fingerprint = func() ([sha256.Size]byte, error) {
		return operationFingerprint(operationWorkspaceImageBuild, canonical), nil
	}
	return sealedRequest{value: authority}, nil
}

// WorkspaceImageBuildSlotHash returns the exact authority hash used by the
// generic claim. Completion paths use it to prove that a terminal receipt
// belongs to the current Build Lease generation and declaration slot without
// decoding or duplicating the claim's opaque framing.
func WorkspaceImageBuildSlotHash(
	environmentID uuid.UUID,
	buildLeaseID uuid.UUID,
	buildLeaseGeneration int64,
	declarationSlot string,
) ([sha256.Size]byte, error) {
	authority, err := workspaceImageBuildAuthority(
		environmentID,
		buildLeaseID,
		buildLeaseGeneration,
		declarationSlot,
	)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return idempotencySlotHash(authority), nil
}

func workspaceImageBuildAuthority(
	environmentID uuid.UUID,
	buildLeaseID uuid.UUID,
	buildLeaseGeneration int64,
	declarationSlot string,
) (request, error) {
	if environmentID == uuid.Nil {
		return request{}, errors.New("idempotency environment is required")
	}
	if buildLeaseID == uuid.Nil {
		return request{}, errors.New("build lease ID is required")
	}
	if buildLeaseGeneration <= 0 {
		return request{}, errors.New("build lease generation must be positive")
	}
	if declarationSlot == "" {
		return request{}, errors.New("workspace declaration slot is required")
	}
	scope := make([]byte, 0, len(buildLeaseID)+8)
	scope = append(scope, buildLeaseID[:]...)
	scope = binary.BigEndian.AppendUint64(scope, uint64(buildLeaseGeneration))
	return request{
		environmentID: environmentID,
		operation:     operationWorkspaceImageBuild,
		scope:         scope,
		key:           declarationSlot,
	}, nil
}

func validateWorkspaceImageBuildFingerprint(
	fingerprint WorkspaceImageBuildFingerprint,
) error {
	for label, value := range map[string]string{
		"architecture":             fingerprint.Architecture,
		"plan digest":              fingerprint.PlanDigest,
		"submitted source digest":  fingerprint.SubmittedSourceDigest,
		"build tree digest":        fingerprint.BuildTreeDigest,
		"admitted path-set digest": fingerprint.AdmittedPathSetDigest,
		"source archive digest":    fingerprint.SourceArchiveDigest,
		"cache scope":              fingerprint.CacheScope,
		"execution ABI":            fingerprint.ExecutionABI,
		"LLB ABI":                  fingerprint.LLBABI,
		"cache ABI":                fingerprint.CacheABI,
		"output architecture":      fingerprint.Output.Architecture,
		"output media type":        fingerprint.Output.MediaType,
	} {
		if value == "" {
			return fmt.Errorf("workspace image build %s is required", label)
		}
	}
	if fingerprint.Architecture != fingerprint.Output.Architecture {
		return errors.New("workspace image build output architecture does not match the request")
	}
	if fingerprint.ImageCacheMode != "prefer" && fingerprint.ImageCacheMode != "bypass" {
		return errors.New("workspace image build cache mode is invalid")
	}
	if fingerprint.BuildTreeSizeBytes < 1 ||
		fingerprint.SourceArchiveSizeBytes < 1 ||
		fingerprint.SourceArchiveEntries < 0 ||
		fingerprint.Quotas.CPUMillis < 1 ||
		fingerprint.Quotas.MemoryBytes < 1 ||
		fingerprint.Quotas.ScratchBytes < 1 ||
		fingerprint.Quotas.PIDs < 1 ||
		fingerprint.Quotas.MaxSourceArchiveBytes < 1 ||
		fingerprint.Quotas.MaxSourceArchiveEntries < 1 ||
		fingerprint.Quotas.MaxOCIArchiveBytes < 1 ||
		fingerprint.Output.MaxSizeBytes < 1 {
		return errors.New("workspace image build fingerprint contains an invalid bound")
	}
	return nil
}

func NewSecretCreateRequest(environmentID uuid.UUID, name string, key string) (Request, error) {
	if name == "" {
		return nil, errors.New("secret name is required")
	}
	return newSecretValueRequest(environmentID, operationSecretCreate, secretNameScope(name), key, []byte(name))
}

func NewSecretRotateRequest(environmentID uuid.UUID, secretID uuid.UUID, key string) (Request, error) {
	if secretID == uuid.Nil {
		return nil, errors.New("secret ID is required")
	}
	return newSecretValueRequest(environmentID, operationSecretRotate, secretID[:], key, nil)
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
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationSecretRevoke, nil), nil
		},
	}}, nil
}

func NewRunMetadataRequest(
	environmentID uuid.UUID,
	runID uuid.UUID,
	attemptNumber int32,
	operationID string,
	mutationJSON []byte,
	leaseFenceFingerprint string,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if runID == uuid.Nil {
		return nil, errors.New("run ID is required")
	}
	if attemptNumber <= 0 {
		return nil, errors.New("run attempt number must be positive")
	}
	if leaseFenceFingerprint == "" {
		return nil, errors.New("run lease fence fingerprint is required")
	}
	mutation, err := jsoncanon.Transform(mutationJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize run metadata mutation: %w", err)
	}
	fingerprintInput, err := json.Marshal(struct {
		Mutation              json.RawMessage `json:"mutation"`
		LeaseFenceFingerprint string          `json:"leaseFenceFingerprint"`
	}{
		Mutation: mutation, LeaseFenceFingerprint: leaseFenceFingerprint,
	})
	if err != nil {
		return nil, fmt.Errorf("encode run metadata mutation fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fingerprintInput)
	if err != nil {
		return nil, fmt.Errorf("canonicalize run metadata mutation fingerprint: %w", err)
	}
	scope := make([]byte, 0, len("attempt\x00")+len(runID)+4)
	scope = append(scope, "attempt\x00"...)
	scope = append(scope, runID[:]...)
	var attempt [4]byte
	binary.BigEndian.PutUint32(attempt[:], uint32(attemptNumber))
	scope = append(scope, attempt[:]...)
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationRunMetadata,
		scope:         scope,
		key:           operationID,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationRunMetadata, canonical), nil
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
		return nil, fmt.Errorf("canonicalize actor input: %w", err)
	}
	input := bytes.Clone(canonicalInput)
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorInputSend,
		scope:         bytes.Clone(actorID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorInputSend, input), nil
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
		return nil, fmt.Errorf("canonicalize actor output: %w", err)
	}
	fields, err := json.Marshal(struct {
		Data        json.RawMessage `json:"data"`
		ContentType string          `json:"contentType"`
	}{
		Data:        canonicalData,
		ContentType: contentType,
	})
	if err != nil {
		return nil, fmt.Errorf("encode actor output fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize actor output fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorOutputAppend,
		scope:         bytes.Clone(actorID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorOutputAppend, canonical), nil
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
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorClose, nil), nil
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
		return nil, errors.New("token creating run ID is required")
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
		return nil, fmt.Errorf("canonicalize token metadata: %w", err)
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
		return nil, fmt.Errorf("encode token create fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize token create fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTokenCreate,
		scope:         bytes.Clone(scope),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationTokenCreate, canonical), nil
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
		return nil, errors.New("token ID is required")
	}
	canonical, err := jsoncanon.Transform(resultJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize token result: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTokenComplete,
		scope:         bytes.Clone(tokenID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationTokenComplete, canonical), nil
		},
	}}, nil
}

func NewTokenCancelRequest(environmentID uuid.UUID, tokenID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if tokenID == uuid.Nil {
		return nil, errors.New("token ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTokenCancel,
		scope:         bytes.Clone(tokenID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationTokenCancel, nil), nil
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
		return nil, fmt.Errorf("canonicalize actor start workspace address: %w", err)
	}
	runMetadata, err := canonicalJSONOr(input.ManagedRunMetadata, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize managed run metadata: %w", err)
	}
	var retryPolicy json.RawMessage
	if len(input.ManagedRetryPolicy) > 0 {
		retryPolicy, err = jsoncanon.Transform(input.ManagedRetryPolicy)
		if err != nil {
			return nil, fmt.Errorf("canonicalize managed run retry policy: %w", err)
		}
	}
	var initialInput json.RawMessage
	if input.InputPresent {
		initialInput, err = jsoncanon.Transform(input.Input)
		if err != nil {
			return nil, fmt.Errorf("canonicalize initial actor input: %w", err)
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
		return nil, fmt.Errorf("encode actor start fingerprint: %w", err)
	}
	canonicalFields, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize actor start fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationActorStart,
		scope:         []byte(actorDeclaredID),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationActorStart, canonicalFields), nil
		},
	}}, nil
}

func NewExternalWorkspaceCreateRequest(
	environmentID uuid.UUID,
	workspaceDeclaredID string,
	key string,
	input WorkspaceCreateFingerprint,
) (Request, error) {
	return newWorkspaceCreateRequest(
		environmentID,
		workspaceCreateScope("external", uuid.Nil, workspaceDeclaredID),
		workspaceDeclaredID,
		key,
		input,
	)
}

func NewRuntimeWorkspaceCreateRequest(
	environmentID uuid.UUID,
	runID uuid.UUID,
	workspaceDeclaredID string,
	key string,
	input WorkspaceCreateFingerprint,
) (Request, error) {
	if runID == uuid.Nil {
		return nil, errors.New("workspace creating run ID is required")
	}
	return newWorkspaceCreateRequest(
		environmentID,
		workspaceCreateScope("runtime", runID, workspaceDeclaredID),
		workspaceDeclaredID,
		key,
		input,
	)
}

func newWorkspaceCreateRequest(
	environmentID uuid.UUID,
	scope []byte,
	workspaceDeclaredID string,
	key string,
	input WorkspaceCreateFingerprint,
) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if workspaceDeclaredID == "" {
		return nil, errors.New("workspace declared ID is required")
	}
	secrets, err := canonicalJSONOr(input.Secrets, `[]`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace secret placements: %w", err)
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
		return nil, fmt.Errorf("encode workspace create fingerprint: %w", err)
	}
	canonicalFields, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace create fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationWorkspaceCreate,
		scope:         scope,
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationWorkspaceCreate, canonicalFields), nil
		},
	}}, nil
}

func workspaceCreateScope(kind string, runID uuid.UUID, declaredID string) []byte {
	scope := make([]byte, 0, len(kind)+1+len(runID)+1+len(declaredID))
	scope = append(scope, kind...)
	scope = append(scope, 0)
	if runID != uuid.Nil {
		scope = append(scope, runID[:]...)
		scope = append(scope, 0)
	}
	return append(scope, declaredID...)
}

func NewWorkspaceDeleteRequest(environmentID uuid.UUID, workspaceID uuid.UUID, key string) (Request, error) {
	if environmentID == uuid.Nil {
		return nil, errors.New("idempotency environment is required")
	}
	if workspaceID == uuid.Nil {
		return nil, errors.New("workspace ID is required")
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationWorkspaceDelete,
		scope:         bytes.Clone(workspaceID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationWorkspaceDelete, nil), nil
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
		return nil, errors.New("workspace ID is required")
	}
	env, err := canonicalJSONOr(input.Env, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace exec environment: %w", err)
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
		return nil, fmt.Errorf("encode workspace exec fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace exec fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationWorkspaceExec,
		scope:         bytes.Clone(workspaceID[:]),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationWorkspaceExec, canonical), nil
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
		return nil, errors.New("task declared ID is required")
	}
	workspace, err := jsoncanon.Transform(input.Workspace)
	if err != nil {
		return nil, fmt.Errorf("canonicalize task start workspace: %w", err)
	}
	metadata, err := canonicalJSONOr(input.Metadata, `{}`)
	if err != nil {
		return nil, fmt.Errorf("canonicalize task metadata: %w", err)
	}
	var payload json.RawMessage
	if input.PayloadPresent {
		payload, err = jsoncanon.Transform(input.Payload)
		if err != nil {
			return nil, fmt.Errorf("canonicalize task payload: %w", err)
		}
	}
	var retry json.RawMessage
	if len(input.RetryPolicy) > 0 {
		retry, err = jsoncanon.Transform(input.RetryPolicy)
		if err != nil {
			return nil, fmt.Errorf("canonicalize task retry policy: %w", err)
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
		return nil, fmt.Errorf("encode task start fingerprint: %w", err)
	}
	canonical, err := jsoncanon.Transform(fields)
	if err != nil {
		return nil, fmt.Errorf("canonicalize task start fingerprint: %w", err)
	}
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTaskStart,
		scope:         []byte(taskDeclaredID),
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationTaskStart, canonical), nil
		},
	}}, nil
}

func NewTaskChildInvokeRequest(
	environmentID uuid.UUID,
	parentRunID uuid.UUID,
	taskDeclaredID string,
	key string,
	input TaskChildInvokeFingerprint,
) (Request, error) {
	if parentRunID == uuid.Nil {
		return nil, errors.New("parent run ID is required")
	}
	if taskDeclaredID == "" {
		return nil, errors.New("task declared ID is required")
	}
	if input.Method != "start" && input.Method != "call" {
		return nil, errors.New("child task invocation method is invalid")
	}
	taskFingerprint := TaskStartFingerprint{
		PayloadPresent: input.PayloadPresent,
		Payload:        input.Payload,
		Workspace:      input.Workspace,
		QueueName:      input.QueueName,
		ConcurrencyKey: input.ConcurrencyKey,
		Priority:       input.Priority,
		QueuedTTLMS:    input.QueuedTTLMS,
		RetryPolicy:    input.RetryPolicy,
		Metadata:       input.Metadata,
		Tags:           input.Tags,
	}
	taskRequest, err := NewTaskStartRequest(
		environmentID,
		taskDeclaredID,
		key,
		taskFingerprint,
	)
	if err != nil {
		return nil, err
	}
	base := taskRequest.idempotencyRequest()
	fingerprint, err := base.fingerprint()
	if err != nil {
		return nil, err
	}
	fields, err := json.Marshal(struct {
		Method          string `json:"method"`
		TaskFingerprint string `json:"taskFingerprint"`
	}{
		Method:          input.Method,
		TaskFingerprint: fmt.Sprintf("%x", fingerprint),
	})
	if err != nil {
		return nil, fmt.Errorf("encode child task invocation fingerprint: %w", err)
	}
	scope := make([]byte, 0, len(parentRunID)+1+len(taskDeclaredID))
	scope = append(scope, parentRunID[:]...)
	scope = append(scope, 0)
	scope = append(scope, taskDeclaredID...)
	return sealedRequest{value: request{
		environmentID: environmentID,
		operation:     operationTaskChildInvoke,
		scope:         scope,
		key:           key,
		fingerprint: func() ([sha256.Size]byte, error) {
			return operationFingerprint(operationTaskChildInvoke, fields), nil
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
	if environmentID == uuid.Nil {
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

func TransactionFor(tx pgx.Tx) (*Transaction, error) {
	if tx == nil {
		return nil, errors.New("idempotency transaction is required")
	}
	queries := db.New(tx)
	return &Transaction{store: queries, queries: queries}, nil
}

func TransactionForQueries(queries db.Querier) (*Transaction, error) {
	if queries == nil {
		return nil, errors.New("idempotency query transaction is required")
	}
	return &Transaction{store: queries}, nil
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

	slotHash := idempotencySlotHash(request)
	fingerprint, err := request.fingerprint()
	if err != nil {
		return Result{}, fmt.Errorf("fingerprint idempotency request: %w", err)
	}

	for {
		locked, err := t.store.LockLiveIdempotencyClaim(ctx, db.LockLiveIdempotencyClaimParams{
			EnvironmentID: pgvalue.UUID(request.environmentID),
			Operation:     string(request.operation),
			SlotHash:      slotHash[:],
		})
		switch {
		case err == nil:
			claim := claimFromRow(locked)
			if locked.Expired {
				if _, err := t.store.RetireExpiredIdempotencyClaim(ctx, db.RetireExpiredIdempotencyClaimParams{
					EnvironmentID: pgvalue.UUID(request.environmentID),
					ID:            claim.ID,
				}); err != nil {
					return Result{}, fmt.Errorf("retire idempotency claim: %w", err)
				}
				continue
			}
			if !bytes.Equal(claim.RequestFingerprint, fingerprint[:]) {
				claimID, _ := pgvalue.UUIDValue(claim.ID)
				return Result{}, ConflictError{ClaimID: claimID}
			}
			return Result{Claim: claim}, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Result{}, fmt.Errorf("lock live idempotency claim: %w", err)
		}

		created, err := t.create(ctx, request, slotHash, fingerprint)
		if err == nil {
			return created, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
	}
}

func supportedOperation(value operation) bool {
	switch value {
	case operationDeploymentCreate, operationSecretCreate, operationSecretRotate, operationSecretRevoke, operationRunMetadata,
		operationActorStart, operationActorInputSend, operationActorOutputAppend, operationActorClose,
		operationTaskStart, operationTaskChildInvoke, operationTokenCreate, operationTokenComplete, operationTokenCancel,
		operationWorkspaceCreate, operationWorkspaceExec, operationWorkspaceDelete, operationWorkspaceImageBuild:
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

func (t *Transaction) create(
	ctx context.Context,
	request request,
	slotHash [sha256.Size]byte,
	fingerprint [sha256.Size]byte,
) (Result, error) {
	claim, err := t.store.CreateIdempotencyClaim(ctx, db.CreateIdempotencyClaimParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:      pgvalue.UUID(request.environmentID),
		Operation:          string(request.operation),
		SlotHash:           slotHash[:],
		RequestFingerprint: fingerprint[:],
	})
	if err != nil {
		return Result{}, fmt.Errorf("create idempotency claim: %w", err)
	}
	return Result{Claim: claim, New: true}, nil
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

func claimFromRow(row db.LockLiveIdempotencyClaimRow) db.IdempotencyClaim {
	return db.IdempotencyClaim{
		ID:                 row.ID,
		EnvironmentID:      row.EnvironmentID,
		Operation:          row.Operation,
		SlotHash:           row.SlotHash,
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
	LockLiveIdempotencyClaim(context.Context, db.LockLiveIdempotencyClaimParams) (db.LockLiveIdempotencyClaimRow, error)
	CreateIdempotencyClaim(context.Context, db.CreateIdempotencyClaimParams) (db.IdempotencyClaim, error)
	RetireExpiredIdempotencyClaim(context.Context, db.RetireExpiredIdempotencyClaimParams) (db.IdempotencyClaim, error)
	CompleteIdempotencyClaim(context.Context, db.CompleteIdempotencyClaimParams) (db.IdempotencyClaim, error)
	FailIdempotencyClaim(context.Context, db.FailIdempotencyClaimParams) (db.IdempotencyClaim, error)
}
