package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEncodeTaskChildInvokeFingerprintPreservesAbsentRetryPolicy(t *testing.T) {
	encoded, err := EncodeTaskChildInvokeFingerprint(TaskChildInvokeFingerprint{
		Method:         "call",
		PayloadPresent: true,
		Payload:        json.RawMessage(`{"value":1}`),
		Computer:       json.RawMessage(`{"id":"computer"}`),
		QueueName:      "default",
		Metadata:       json.RawMessage(`{}`),
		Tags:           []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"retryPolicy"`)) {
		t.Fatalf("absent retry policy was serialized: %s", encoded)
	}

	var decoded TaskChildInvokeFingerprint
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.RetryPolicy) != 0 {
		t.Fatalf("decoded retry policy = %s", decoded.RetryPolicy)
	}
}

func TestEncodeTaskChildInvokeFingerprintPreservesCanonicalRetryPolicy(t *testing.T) {
	for _, retryPolicy := range []string{
		`{"enabled":false}`,
		`{"backoff":{"factor":2,"jitter":"full","maxMs":30000,"minMs":1000},"enabled":true,"maxAttempts":3}`,
	} {
		t.Run(retryPolicy, func(t *testing.T) {
			encoded, err := EncodeTaskChildInvokeFingerprint(TaskChildInvokeFingerprint{
				Method:      "call",
				Computer:    json.RawMessage(`{}`),
				RetryPolicy: json.RawMessage(retryPolicy),
				Metadata:    json.RawMessage(`{}`),
				Tags:        []string{},
			})
			if err != nil {
				t.Fatal(err)
			}
			var decoded TaskChildInvokeFingerprint
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if string(decoded.RetryPolicy) != retryPolicy {
				t.Fatalf("decoded retry policy = %s", decoded.RetryPolicy)
			}
		})
	}
}

func TestTransactionCreateReplayAndConflict(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	environmentID := uuid.New()
	actorID := uuid.New()
	first, err := NewSessionOperationRequest(
		environmentID,
		actorID,
		"message-1",
		"session.send", json.RawMessage(`{"b":2,"a":1}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	created, err := transaction.Acquire(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if !created.New {
		t.Fatal("first acquisition did not create a claim")
	}
	completed, err := transaction.Complete(
		t.Context(),
		created.Claim,
		[]byte(`{"turnId":"turn-1"}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	equivalent, err := NewSessionOperationRequest(
		environmentID,
		actorID,
		"message-1",
		"session.send", json.RawMessage("{\n\"a\":1.0,\"b\":2}"),
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := transaction.Acquire(t.Context(), equivalent)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.New || replayed.Claim.ID != created.Claim.ID ||
		!bytes.Equal(replayed.Claim.Receipt, completed.Receipt) {
		t.Fatalf("replayed claim = %+v", replayed)
	}

	conflicting, err := NewSessionOperationRequest(
		environmentID,
		actorID,
		"message-1",
		"session.send", json.RawMessage(`{"a":1,"b":3}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transaction.Acquire(t.Context(), conflicting)
	var conflict ConflictError
	if !errors.As(err, &conflict) || conflict.ClaimID != pgvalue.MustUUIDValue(created.Claim.ID) {
		t.Fatalf("conflict = %v", err)
	}
}

func TestDeploymentFinalizeFingerprintBindsBundleDigest(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	environmentID := uuid.New()
	projectID := uuid.New()
	fingerprint := DeploymentFinalizeFingerprint{BundleDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64))}
	first, err := NewDeploymentFinalizeRequest(environmentID, projectID, "deploy-1", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	created, err := transaction.Acquire(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Complete(
		t.Context(),
		created.Claim,
		[]byte(`{"deploymentId":"0198f061-f8a1-7e90-a731-263efde79842"}`),
	); err != nil {
		t.Fatal(err)
	}
	replay, err := NewDeploymentFinalizeRequest(environmentID, projectID, "deploy-1", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := transaction.Acquire(t.Context(), replay)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.New || replayed.Claim.ID != created.Claim.ID {
		t.Fatalf("replayed claim = %+v", replayed)
	}
	fingerprint.BundleDigest = "sha256:" + string(bytes.Repeat([]byte{'b'}, 64))
	conflicting, err := NewDeploymentFinalizeRequest(environmentID, projectID, "deploy-1", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transaction.Acquire(t.Context(), conflicting)
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflict = %v", err)
	}
}

func TestComputerCreateSlotsBindSourceAuthority(t *testing.T) {
	environmentID := uuid.New()
	firstRunID := uuid.New()
	secondRunID := uuid.New()
	fingerprint := ComputerCreateFingerprint{Secrets: []byte(`[]`)}
	external, err := NewExternalComputerCreateRequest(
		environmentID, "sandbox", "create-1", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := NewRuntimeComputerCreateRequest(
		environmentID, firstRunID, "sandbox", "create-1", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstRunReplay, err := NewRuntimeComputerCreateRequest(
		environmentID, firstRunID, "sandbox", "create-1", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := NewRuntimeComputerCreateRequest(
		environmentID, secondRunID, "sandbox", "create-1", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}

	externalSlot := idempotencySlotHash(external.idempotencyRequest())
	firstRunSlot := idempotencySlotHash(firstRun.idempotencyRequest())
	if externalSlot == firstRunSlot {
		t.Fatal("external and Run-internal Computer creation shared a claim slot")
	}
	if firstRunSlot != idempotencySlotHash(firstRunReplay.idempotencyRequest()) {
		t.Fatal("same source Run did not reproduce its Computer creation slot")
	}
	if firstRunSlot == idempotencySlotHash(secondRun.idempotencyRequest()) {
		t.Fatal("different source Runs shared a Computer creation slot")
	}
}

func TestPrunedReceiptNeverReusesOperationIdentity(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	request, err := NewSecretCreateRequest(uuid.New(), "API_TOKEN", "create-1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := transaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Complete(t.Context(), first.Claim, []byte(`{"secretId":"created"}`)); err != nil {
		t.Fatal(err)
	}
	store.live.Receipt = nil
	store.live.ReceiptPrunedAt = pgtype.Timestamptz{Valid: true}
	_, err = transaction.Acquire(t.Context(), request)
	var expired ExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("pruned retry = %v", err)
	}
	if store.live.ID != first.Claim.ID {
		t.Fatal("pruned operation changed identity")
	}
	changed := request.idempotencyRequest()
	changed.fingerprint = func() ([sha256.Size]byte, error) { return sha256.Sum256([]byte("changed")), nil }
	_, err = transaction.Acquire(t.Context(), sealedRequest{value: changed})
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("changed pruned retry = %v", err)
	}
}

func TestSlotHashFramesEveryAuthorityField(t *testing.T) {
	environmentID := uuid.New()
	base := request{
		environmentID: environmentID,
		operation:     operation("session.send"),
		scope:         []byte("ab"),
		key:           "c",
	}
	first := idempotencySlotHash(base)
	equivalent := request{
		environmentID: environmentID,
		operation:     operation("session.send"),
		scope:         []byte("ab"),
		key:           "c",
	}
	if first != idempotencySlotHash(equivalent) {
		t.Fatal("slot hash is not deterministic")
	}
	changes := []request{
		{environmentID: uuid.New(), operation: base.operation, scope: base.scope, key: base.key},
		{environmentID: environmentID, operation: operation("session.close"), scope: base.scope, key: base.key},
		{environmentID: environmentID, operation: base.operation, scope: []byte("a"), key: "bc"},
	}
	for _, changed := range changes {
		if first == idempotencySlotHash(changed) {
			t.Fatalf("distinct authority tuple produced the same slot hash: %+v", changed)
		}
	}
}

type claimMemory struct {
	live *db.IdempotencyClaim
}

func (s *claimMemory) LockIdempotencyClaim(
	_ context.Context,
	arg db.LockIdempotencyClaimParams,
) (db.IdempotencyClaim, error) {
	if s.live == nil ||
		s.live.EnvironmentID != arg.EnvironmentID ||
		s.live.Operation != arg.Operation ||
		!bytes.Equal(s.live.SlotHash, arg.SlotHash) {
		return db.IdempotencyClaim{}, pgx.ErrNoRows
	}
	return *s.live, nil
}

func (s *claimMemory) CreateIdempotencyClaim(
	_ context.Context,
	arg db.CreateIdempotencyClaimParams,
) (db.IdempotencyClaim, error) {
	if s.live != nil {
		return db.IdempotencyClaim{}, pgx.ErrNoRows
	}
	claim := db.IdempotencyClaim{
		ID:                 arg.ID,
		EnvironmentID:      arg.EnvironmentID,
		Operation:          arg.Operation,
		SlotHash:           bytes.Clone(arg.SlotHash),
		RequestFingerprint: bytes.Clone(arg.RequestFingerprint),
		Status:             "pending",
	}
	s.live = &claim
	return claim, nil
}

func (s *claimMemory) CompleteIdempotencyClaim(
	_ context.Context,
	arg db.CompleteIdempotencyClaimParams,
) (db.IdempotencyClaim, error) {
	return s.finish(arg.EnvironmentID, arg.ID, arg.RequestFingerprint, arg.Receipt, "completed")
}

func (s *claimMemory) FailIdempotencyClaim(
	_ context.Context,
	arg db.FailIdempotencyClaimParams,
) (db.IdempotencyClaim, error) {
	return s.finish(arg.EnvironmentID, arg.ID, arg.RequestFingerprint, arg.Receipt, "failed")
}

func (s *claimMemory) finish(
	environmentID pgtype.UUID,
	id pgtype.UUID,
	fingerprint []byte,
	receipt []byte,
	state string,
) (db.IdempotencyClaim, error) {
	if s.live == nil ||
		s.live.EnvironmentID != environmentID ||
		s.live.ID != id ||
		s.live.Status != "pending" ||
		!bytes.Equal(s.live.RequestFingerprint, fingerprint) {
		return db.IdempotencyClaim{}, pgx.ErrNoRows
	}
	s.live.Status = state
	s.live.Receipt = bytes.Clone(receipt)
	s.live.CompletedAt = pgtype.Timestamptz{Valid: true}
	return *s.live, nil
}
