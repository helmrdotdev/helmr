package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAcquireCreatesAndReplaysCurrentClaim(t *testing.T) {
	manager := testManager(t)
	store := &fakeClaimStore{}
	transaction := &Transaction{manager: manager, store: store}
	request := testRequest()

	created, err := transaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !created.New || created.Claim.Generation != 1 || created.Claim.HashKeyVersion != 1 {
		t.Fatalf("created = %+v", created)
	}
	completed, err := transaction.Complete(t.Context(), created.Claim, []byte(`{"secretId":"sec_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := transaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.New || replayed.Claim.State != "completed" || !bytes.Equal(replayed.Claim.Receipt, completed.Receipt) {
		t.Fatalf("replayed = %+v", replayed)
	}
}

func TestAcquireUsesMatchedHashVersionForFingerprint(t *testing.T) {
	store := &fakeClaimStore{}
	oldTransaction := &Transaction{manager: testManager(t), store: store}
	request := testRequest()
	created, err := oldTransaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	store.current = 2

	var fingerprintVersion int32
	request = testRequestWithAuthenticator(func(version int32) ([sha256.Size]byte, error) {
		fingerprintVersion = version
		return sha256.Sum256([]byte("value")), nil
	})
	currentTransaction := &Transaction{manager: testManager(t), store: store}
	replayed, err := currentTransaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.New || replayed.Claim.ID != created.Claim.ID || fingerprintVersion != 1 {
		t.Fatalf("replayed = %+v fingerprint version = %d", replayed, fingerprintVersion)
	}
}

func TestAcquireRebindsExpiredClaimUnderCurrentHashVersion(t *testing.T) {
	store := &fakeClaimStore{}
	oldTransaction := &Transaction{manager: testManager(t), store: store}
	created, err := oldTransaction.Acquire(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	store.expired[created.Claim.ID.Bytes] = true
	store.current = 2

	request := testRequestWithAuthenticator(func(int32) ([sha256.Size]byte, error) {
		return sha256.Sum256([]byte("different value")), nil
	})
	currentTransaction := &Transaction{manager: testManager(t), store: store}
	rebound, err := currentTransaction.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !rebound.New || rebound.Claim.Generation != 2 || rebound.Claim.HashKeyVersion != 2 {
		t.Fatalf("rebound = %+v", rebound)
	}
}

func TestAcquireAdvancesPastRetiredHistory(t *testing.T) {
	store := &fakeClaimStore{}
	transaction := &Transaction{manager: testManager(t), store: store}
	created, err := transaction.Acquire(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	created.Claim.RetiredAt = pgtype.Timestamptz{Valid: true}
	store.claims[0] = created.Claim

	rebound, err := transaction.Acquire(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !rebound.New || rebound.Claim.Generation != 2 {
		t.Fatalf("rebound = %+v", rebound)
	}
}

func TestAcquireRejectsFingerprintConflict(t *testing.T) {
	store := &fakeClaimStore{}
	transaction := &Transaction{manager: testManager(t), store: store}
	if _, err := transaction.Acquire(t.Context(), testRequest()); err != nil {
		t.Fatal(err)
	}
	conflicting := testRequestWithAuthenticator(func(int32) ([sha256.Size]byte, error) {
		return sha256.Sum256([]byte("different value")), nil
	})
	_, err := transaction.Acquire(t.Context(), conflicting)
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestAcquireFailsClosedOnMultipleLiveClaims(t *testing.T) {
	store := &fakeClaimStore{}
	transaction := &Transaction{manager: testManager(t), store: store}
	created, err := transaction.Acquire(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	duplicate := created.Claim
	duplicate.ID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store.claims = append(store.claims, duplicate)
	if _, err := transaction.Acquire(t.Context(), testRequest()); err == nil {
		t.Fatal("expected multiple claim error")
	}
}

func TestCompletionRequiresObjectReceipt(t *testing.T) {
	store := &fakeClaimStore{}
	transaction := &Transaction{manager: testManager(t), store: store}
	created, err := transaction.Acquire(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Complete(t.Context(), created.Claim, []byte(`[]`)); err == nil {
		t.Fatal("expected receipt validation error")
	}
}

func TestAdvisoryLockFrameIsInjective(t *testing.T) {
	requestA := testRequest().idempotencyRequest()
	requestA.scope = []byte("ab")
	requestA.key = "c"
	requestB := testRequest().idempotencyRequest()
	requestB.scope = []byte("a")
	requestB.key = "bc"
	if lockKey(requestA) == lockKey(requestB) {
		t.Fatal("distinct scope and key tuples produced the same lock key")
	}
}

func TestSecretRequestCanonicalization(t *testing.T) {
	environmentID := uuid.MustParse("00000000-0000-0000-0000-000000000301")
	secretID := uuid.MustParse("00000000-0000-0000-0000-000000000401")
	authenticator := func(int32) ([sha256.Size]byte, error) {
		return sha256.Sum256([]byte("value")), nil
	}
	create, err := NewSecretCreateRequest(environmentID, "API_TOKEN", "request-key", authenticator)
	if err != nil {
		t.Fatal(err)
	}
	rotate, err := NewSecretRotateRequest(environmentID, secretID, "request-key", authenticator)
	if err != nil {
		t.Fatal(err)
	}
	revoke, err := NewSecretRevokeRequest(environmentID, secretID, "request-key")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name            string
		request         Request
		wantOperation   operation
		wantScopeHex    string
		wantFingerprint string
	}{
		{
			name:            "create",
			request:         create,
			wantOperation:   operationSecretCreate,
			wantScopeHex:    "00000000000000094150495f544f4b454e",
			wantFingerprint: "2b2b777e96e93bf74245313aac9035ff1b8f511452d84177c3994ace6b4528b0",
		},
		{
			name:            "rotate",
			request:         rotate,
			wantOperation:   operationSecretRotate,
			wantScopeHex:    "00000000000000000000000000000401",
			wantFingerprint: "80347c4d64a6cb4ad6e2c30c831ac07884502a0b52797ed3bc4085f33dadc14c",
		},
		{
			name:            "revoke",
			request:         revoke,
			wantOperation:   operationSecretRevoke,
			wantScopeHex:    "00000000000000000000000000000401",
			wantFingerprint: "5cc923317668762bf99d982ea23ccd118c2da2ec676dbdfa8211b07e6436c5cc",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := test.request.idempotencyRequest()
			if value.operation != test.wantOperation || hex.EncodeToString(value.scope) != test.wantScopeHex {
				t.Fatalf("operation = %q scope = %x", value.operation, value.scope)
			}
			fingerprint, err := value.fingerprint(1)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(fingerprint[:]) != test.wantFingerprint {
				t.Fatalf("fingerprint = %x", fingerprint)
			}
		})
	}
}

func TestActorInputRequestUsesActorScopeAndCanonicalInputFingerprint(t *testing.T) {
	environmentID := uuid.MustParse("00000000-0000-0000-0000-000000000301")
	actorID := uuid.MustParse("00000000-0000-0000-0000-000000000501")
	first, err := NewActorInputSendRequest(environmentID, actorID, "message-1", []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewActorInputSendRequest(environmentID, actorID, "message-1", []byte("{\n\"a\":1.0,\"b\":2}"))
	if err != nil {
		t.Fatal(err)
	}
	firstValue := first.idempotencyRequest()
	secondValue := second.idempotencyRequest()
	firstFingerprint, err := firstValue.fingerprint(1)
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint, err := secondValue.fingerprint(2)
	if err != nil {
		t.Fatal(err)
	}
	if firstValue.operation != operationActorInputSend || !bytes.Equal(firstValue.scope, actorID[:]) {
		t.Fatalf("operation = %q scope = %x", firstValue.operation, firstValue.scope)
	}
	if firstFingerprint != secondFingerprint {
		t.Fatalf("canonical-equivalent input fingerprints differ: %x != %x", firstFingerprint, secondFingerprint)
	}
}

func testManager(t *testing.T) Manager {
	t.Helper()
	hashes, err := keyedhash.New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, keyedhash.KeySize),
		2: bytes.Repeat([]byte{2}, keyedhash.KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(hashes)
}

func testRequest() Request {
	return testRequestWithAuthenticator(func(int32) ([sha256.Size]byte, error) {
		return sha256.Sum256([]byte("value")), nil
	})
}

func testRequestWithAuthenticator(authenticator func(int32) ([sha256.Size]byte, error)) Request {
	request, err := NewSecretCreateRequest(
		uuid.MustParse("00000000-0000-0000-0000-000000000301"),
		"API_TOKEN",
		"request-key",
		authenticator,
	)
	if err != nil {
		panic(err)
	}
	return request
}

type fakeClaimStore struct {
	claims  []db.IdempotencyClaim
	expired map[[16]byte]bool
	current int32
}

func (f *fakeClaimStore) LockIdempotencySlot(context.Context, int64) error {
	if f.expired == nil {
		f.expired = map[[16]byte]bool{}
	}
	return nil
}

func (f *fakeClaimStore) LockActiveLookupHMACVersions(context.Context) ([]db.LookupHmacVersion, error) {
	current := f.current
	if current == 0 {
		current = 1
	}
	return testAuthorityRows(current), nil
}

func testAuthorityRows(current int32) []db.LookupHmacVersion {
	hashes, err := keyedhash.New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, keyedhash.KeySize),
		2: bytes.Repeat([]byte{2}, keyedhash.KeySize),
	})
	if err != nil {
		panic(err)
	}
	rows := make([]db.LookupHmacVersion, 0, 2)
	for _, version := range []int32{1, 2} {
		fingerprint, err := hashes.Fingerprint(version)
		if err != nil {
			panic(err)
		}
		rows = append(rows, db.LookupHmacVersion{
			Version:        version,
			KeyFingerprint: fingerprint[:],
			IsCurrent:      version == current,
		})
	}
	return rows
}

func (f *fakeClaimStore) FindLiveIdempotencyClaims(_ context.Context, arg db.FindLiveIdempotencyClaimsParams) ([]db.FindLiveIdempotencyClaimsRow, error) {
	rows := make([]db.FindLiveIdempotencyClaimsRow, 0)
	for _, claim := range f.claims {
		if claim.RetiredAt.Valid || claim.EnvironmentID != arg.EnvironmentID || claim.Operation != arg.Operation {
			continue
		}
		for i, version := range arg.HashKeyVersions {
			if claim.HashKeyVersion == version && bytes.Equal(claim.ScopeHash, arg.ScopeHashes[i]) && bytes.Equal(claim.KeyHash, arg.KeyHashes[i]) {
				rows = append(rows, findRow(claim, f.expired[claim.ID.Bytes]))
			}
		}
	}
	return rows, nil
}

func (f *fakeClaimStore) CreateIdempotencyClaim(_ context.Context, arg db.CreateIdempotencyClaimParams) (db.IdempotencyClaim, error) {
	claim := db.IdempotencyClaim{
		ID:                 arg.ID,
		EnvironmentID:      arg.EnvironmentID,
		Operation:          arg.Operation,
		ScopeHash:          bytes.Clone(arg.ScopeHash),
		KeyHash:            bytes.Clone(arg.KeyHash),
		HashKeyVersion:     arg.HashKeyVersion,
		Generation:         arg.Generation,
		RequestFingerprint: bytes.Clone(arg.RequestFingerprint),
		State:              "pending",
	}
	f.claims = append(f.claims, claim)
	return claim, nil
}

func (f *fakeClaimStore) GetLatestIdempotencyClaimGeneration(_ context.Context, arg db.GetLatestIdempotencyClaimGenerationParams) (int64, error) {
	var latest int64
	for _, claim := range f.claims {
		if claim.EnvironmentID != arg.EnvironmentID || claim.Operation != arg.Operation {
			continue
		}
		for i, version := range arg.HashKeyVersions {
			if claim.HashKeyVersion == version && bytes.Equal(claim.ScopeHash, arg.ScopeHashes[i]) && bytes.Equal(claim.KeyHash, arg.KeyHashes[i]) && claim.Generation > latest {
				latest = claim.Generation
			}
		}
	}
	return latest, nil
}

func (f *fakeClaimStore) RetireExpiredIdempotencyClaim(_ context.Context, arg db.RetireExpiredIdempotencyClaimParams) (db.IdempotencyClaim, error) {
	for i, claim := range f.claims {
		if claim.ID != arg.ID || claim.EnvironmentID != arg.EnvironmentID || !f.expired[claim.ID.Bytes] {
			continue
		}
		claim.RetiredAt = pgtype.Timestamptz{Valid: true}
		f.claims[i] = claim
		return claim, nil
	}
	return db.IdempotencyClaim{}, pgx.ErrNoRows
}

func (f *fakeClaimStore) CompleteIdempotencyClaim(_ context.Context, arg db.CompleteIdempotencyClaimParams) (db.IdempotencyClaim, error) {
	return f.finish(arg.EnvironmentID, arg.ID, arg.RequestFingerprint, arg.Receipt, "completed")
}

func (f *fakeClaimStore) FailIdempotencyClaim(_ context.Context, arg db.FailIdempotencyClaimParams) (db.IdempotencyClaim, error) {
	return f.finish(arg.EnvironmentID, arg.ID, arg.RequestFingerprint, arg.Receipt, "failed")
}

func (f *fakeClaimStore) finish(environmentID pgtype.UUID, id pgtype.UUID, fingerprint []byte, receipt []byte, state string) (db.IdempotencyClaim, error) {
	for i, claim := range f.claims {
		if claim.EnvironmentID != environmentID || claim.ID != id || claim.State != "pending" || !bytes.Equal(claim.RequestFingerprint, fingerprint) {
			continue
		}
		claim.State = state
		claim.Receipt = bytes.Clone(receipt)
		f.claims[i] = claim
		return claim, nil
	}
	return db.IdempotencyClaim{}, pgx.ErrNoRows
}

func findRow(claim db.IdempotencyClaim, expired bool) db.FindLiveIdempotencyClaimsRow {
	return db.FindLiveIdempotencyClaimsRow{
		ID:                 claim.ID,
		EnvironmentID:      claim.EnvironmentID,
		Operation:          claim.Operation,
		ScopeHash:          bytes.Clone(claim.ScopeHash),
		KeyHash:            bytes.Clone(claim.KeyHash),
		HashKeyVersion:     claim.HashKeyVersion,
		Generation:         claim.Generation,
		RequestFingerprint: bytes.Clone(claim.RequestFingerprint),
		State:              claim.State,
		Receipt:            bytes.Clone(claim.Receipt),
		AcceptedAt:         claim.AcceptedAt,
		ExpiresAt:          claim.ExpiresAt,
		RetiredAt:          claim.RetiredAt,
		CompletedAt:        claim.CompletedAt,
		Expired:            expired,
	}
}
