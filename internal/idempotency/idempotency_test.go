package idempotency

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTransactionCreateReplayAndConflict(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	environmentID := uuid.New()
	actorID := uuid.New()
	first, err := NewActorInputSendRequest(
		environmentID,
		actorID,
		"message-1",
		[]byte(`{"b":2,"a":1}`),
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
		[]byte(`{"recordId":"record-1"}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	equivalent, err := NewActorInputSendRequest(
		environmentID,
		actorID,
		"message-1",
		[]byte("{\n\"a\":1.0,\"b\":2}"),
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

	conflicting, err := NewActorInputSendRequest(
		environmentID,
		actorID,
		"message-1",
		[]byte(`{"a":1,"b":3}`),
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

func TestDeploymentCreateFingerprintBindsBuildAuthority(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	environmentID := uuid.New()
	projectID := uuid.New()
	fingerprint := DeploymentCreateFingerprint{
		SourceDigest:         "sha256:source",
		LockfileDigest:       "sha256:lockfile",
		LockfileName:         "pnpm-lock.yaml",
		NodeVersion:          "24.16.0",
		ManagerName:          "pnpm",
		ManagerVersion:       "11.1.0",
		ManagerIntegrity:     "sha256.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BuildContractVersion: "helmr.program-build.v0",
		ImageCacheMode:       "prefer",
	}
	first, err := NewDeploymentCreateRequest(environmentID, projectID, "deploy-1", fingerprint)
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
	replay, err := NewDeploymentCreateRequest(environmentID, projectID, "deploy-1", fingerprint)
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
	fingerprint.ManagerIntegrity = "sha256.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	conflicting, err := NewDeploymentCreateRequest(environmentID, projectID, "deploy-1", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transaction.Acquire(t.Context(), conflicting)
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflict = %v", err)
	}
}

func TestDeploymentCreateFingerprintBindsImageCacheMode(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	environmentID := uuid.New()
	projectID := uuid.New()
	fingerprint := DeploymentCreateFingerprint{
		SourceDigest: "sha256:source", LockfileDigest: "sha256:lockfile",
		LockfileName: "pnpm-lock.yaml", NodeVersion: "24.16.0",
		ManagerName: "pnpm", ManagerVersion: "11.1.0",
		BuildContractVersion: "helmr.program-build.v0", ImageCacheMode: "prefer",
	}
	request, err := NewDeploymentCreateRequest(environmentID, projectID, "deploy-1", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Acquire(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	fingerprint.ImageCacheMode = "bypass"
	conflicting, err := NewDeploymentCreateRequest(environmentID, projectID, "deploy-1", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Acquire(t.Context(), conflicting); err == nil {
		t.Fatal("cache mode change did not conflict")
	}
}

func TestWorkspaceImageBuildRequestBindsLeaseGenerationSlotAndFingerprint(t *testing.T) {
	store := &claimMemory{}
	transaction := &Transaction{store: store}
	environmentID := uuid.New()
	buildLeaseID := uuid.New()
	fingerprint := testWorkspaceImageBuildFingerprint()
	request, err := NewWorkspaceImageBuildRequest(
		environmentID, buildLeaseID, 1, "workspace/base", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := transaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewWorkspaceImageBuildRequest(
		environmentID, buildLeaseID, 1, "workspace/base", fingerprint,
	)
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
	fingerprint.PlanDigest = "sha256:changed"
	changed, err := NewWorkspaceImageBuildRequest(
		environmentID, buildLeaseID, 1, "workspace/base", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Acquire(t.Context(), changed); err == nil {
		t.Fatal("changed image operation fingerprint did not conflict")
	}
	nextGeneration, err := NewWorkspaceImageBuildRequest(
		environmentID, buildLeaseID, 2, "workspace/base", fingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if idempotencySlotHash(nextGeneration.idempotencyRequest()) ==
		idempotencySlotHash(request.idempotencyRequest()) {
		t.Fatal("Build Lease generation did not change the image operation slot")
	}
	publicSlot, err := WorkspaceImageBuildSlotHash(
		environmentID, buildLeaseID, 1, "workspace/base",
	)
	if err != nil {
		t.Fatal(err)
	}
	if publicSlot != idempotencySlotHash(request.idempotencyRequest()) {
		t.Fatal("public Workspace image slot authority diverged from claim framing")
	}
}

func testWorkspaceImageBuildFingerprint() WorkspaceImageBuildFingerprint {
	return WorkspaceImageBuildFingerprint{
		Architecture:           "x86_64",
		PlanDigest:             "sha256:plan",
		SubmittedSourceDigest:  "sha256:source",
		BuildTreeDigest:        "sha256:tree",
		BuildTreeSizeBytes:     4096,
		AdmittedPathSetDigest:  "sha256:paths",
		SourceArchiveDigest:    "sha256:archive",
		SourceArchiveSizeBytes: 1024,
		SourceArchiveEntries:   1,
		ImageCacheMode:         "prefer",
		CacheScope:             "environment/workspace/base",
		ExecutionABI:           "helmr.image-build.v0",
		LLBABI:                 "helmr.image-llb.v0",
		CacheABI:               "helmr.image-cache.v0",
		Quotas: WorkspaceImageBuildQuotas{
			CPUMillis: 3000, MemoryBytes: 4 << 30, ScratchBytes: 32 << 30,
			PIDs: 1024, MaxSourceArchiveBytes: 11 << 30,
			MaxSourceArchiveEntries: 100000, MaxOCIArchiveBytes: 16 << 30,
		},
		Output: WorkspaceImageBuildOutputContract{
			Architecture: "x86_64",
			MediaType:    "application/vnd.helmr.workspace-image.v0.oci-tar",
			MaxSizeBytes: 16 << 30,
		},
	}
}

func TestExpiredClaimRebindUsesClaimIDAsCompletionFence(t *testing.T) {
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
	store.expired = true

	rebound, err := transaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !rebound.New || rebound.Claim.ID == first.Claim.ID {
		t.Fatalf("rebound claim = %+v", rebound)
	}
	if _, err := transaction.Complete(
		t.Context(),
		first.Claim,
		[]byte(`{"secretId":"stale"}`),
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale completion error = %v", err)
	}
}

func TestSlotHashFramesEveryAuthorityField(t *testing.T) {
	environmentID := uuid.New()
	base := request{
		environmentID: environmentID,
		operation:     operationActorInputSend,
		scope:         []byte("ab"),
		key:           "c",
	}
	first := idempotencySlotHash(base)
	equivalent := request{
		environmentID: environmentID,
		operation:     operationActorInputSend,
		scope:         []byte("ab"),
		key:           "c",
	}
	if first != idempotencySlotHash(equivalent) {
		t.Fatal("slot hash is not deterministic")
	}
	changes := []request{
		{environmentID: uuid.New(), operation: base.operation, scope: base.scope, key: base.key},
		{environmentID: environmentID, operation: operationActorClose, scope: base.scope, key: base.key},
		{environmentID: environmentID, operation: base.operation, scope: []byte("a"), key: "bc"},
	}
	for _, changed := range changes {
		if first == idempotencySlotHash(changed) {
			t.Fatalf("distinct authority tuple produced the same slot hash: %+v", changed)
		}
	}
}

type claimMemory struct {
	live    *db.IdempotencyClaim
	expired bool
}

func (s *claimMemory) LockLiveIdempotencyClaim(
	_ context.Context,
	arg db.LockLiveIdempotencyClaimParams,
) (db.LockLiveIdempotencyClaimRow, error) {
	if s.live == nil ||
		s.live.EnvironmentID != arg.EnvironmentID ||
		s.live.Operation != arg.Operation ||
		!bytes.Equal(s.live.SlotHash, arg.SlotHash) {
		return db.LockLiveIdempotencyClaimRow{}, pgx.ErrNoRows
	}
	return lockRow(*s.live, s.expired), nil
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
		State:              "pending",
	}
	s.live = &claim
	return claim, nil
}

func (s *claimMemory) RetireExpiredIdempotencyClaim(
	_ context.Context,
	arg db.RetireExpiredIdempotencyClaimParams,
) (db.IdempotencyClaim, error) {
	if s.live == nil || s.live.ID != arg.ID || s.live.EnvironmentID != arg.EnvironmentID || !s.expired {
		return db.IdempotencyClaim{}, pgx.ErrNoRows
	}
	retired := *s.live
	retired.RetiredAt = pgtype.Timestamptz{Valid: true}
	s.live = nil
	s.expired = false
	return retired, nil
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
		s.live.State != "pending" ||
		!bytes.Equal(s.live.RequestFingerprint, fingerprint) {
		return db.IdempotencyClaim{}, pgx.ErrNoRows
	}
	s.live.State = state
	s.live.Receipt = bytes.Clone(receipt)
	s.live.CompletedAt = pgtype.Timestamptz{Valid: true}
	return *s.live, nil
}

func lockRow(claim db.IdempotencyClaim, expired bool) db.LockLiveIdempotencyClaimRow {
	return db.LockLiveIdempotencyClaimRow{
		ID:                 claim.ID,
		EnvironmentID:      claim.EnvironmentID,
		Operation:          claim.Operation,
		SlotHash:           claim.SlotHash,
		RequestFingerprint: claim.RequestFingerprint,
		State:              claim.State,
		Receipt:            claim.Receipt,
		AcceptedAt:         claim.AcceptedAt,
		ExpiresAt:          claim.ExpiresAt,
		RetiredAt:          claim.RetiredAt,
		CompletedAt:        claim.CompletedAt,
		Expired:            expired,
	}
}
