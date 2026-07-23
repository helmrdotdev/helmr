package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAppendActorInputCompletedClaimBypassesCurrentActorState(t *testing.T) {
	environmentID := uuid.New()
	actorID := uuid.New()
	recordID := uuid.New()
	store, manager := newActorInputClaimStore(t)
	completeActorInputClaim(
		t,
		store,
		manager,
		environmentID,
		actorID,
		"message:1",
		[]byte(`{"a":1,"b":2}`),
		recordID,
		7,
	)
	store.calls = nil

	server := &Server{db: store, claims: manager}
	authorized := false
	record, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		ActorID:        actorID,
		RecordID:       uuid.New(),
		Data:           []byte(`{"b":2,"a":1}`),
		SourceKind:     "external",
		IdempotencyKey: "message:1",
		Authorize: func(context.Context, db.Querier) error {
			authorized = true
			return errors.New("completed replay must not reauthorize")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != pgvalue.UUID(recordID) || record.Sequence != 7 {
		t.Fatalf("record = %+v", record)
	}
	if store.actorReads != 0 {
		t.Fatalf("Actor reads = %d, want 0", store.actorReads)
	}
	if authorized {
		t.Fatal("completed claim replay reauthorized current source state")
	}
	if store.commits != 1 || store.rollbacks != 0 {
		t.Fatalf("transactions: commits=%d rollbacks=%d", store.commits, store.rollbacks)
	}
	if !slices.Equal(store.calls, []string{"claim_lock", "hmac_lock", "claim_find", "commit"}) {
		t.Fatalf("calls = %v", store.calls)
	}
}

func TestAppendActorInputRollsBackNewClaimWhenActorIsUnavailable(t *testing.T) {
	environmentID := uuid.New()
	actorID := uuid.New()
	store, manager := newActorInputClaimStore(t)
	server := &Server{db: store, claims: manager}

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		ActorID:        actorID,
		RecordID:       uuid.New(),
		Data:           []byte(`{"message":"queued"}`),
		SourceKind:     "external",
		IdempotencyKey: "message:2",
	})
	if !errors.Is(err, errActorInputUnavailable) {
		t.Fatalf("error = %v, want Actor input unavailable", err)
	}
	if store.commits != 0 || store.rollbacks != 1 {
		t.Fatalf("transactions: commits=%d rollbacks=%d", store.commits, store.rollbacks)
	}
	if !slices.Equal(store.calls, []string{
		"claim_lock",
		"hmac_lock",
		"claim_find",
		"claim_generation",
		"claim_create",
		"actor_get",
		"rollback",
	}) {
		t.Fatalf("calls = %v", store.calls)
	}
}

func TestAppendActorInputRollsBackProvisionalRunSourceWhenAuthorityIsStale(t *testing.T) {
	environmentID := uuid.New()
	actorID := uuid.New()
	sourceRunID := uuid.New()
	store, manager := newActorInputClaimStore(t)
	store.locator = db.Actor{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       pgvalue.UUID(uuid.New()),
		State:             "open",
		NextInputSequence: 3,
	}
	server := &Server{db: store, claims: manager}
	authorized := false

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		ActorID:        actorID,
		RecordID:       uuid.New(),
		Data:           []byte(`{"message":"queued"}`),
		SourceKind:     "run",
		SourceRunID:    sourceRunID,
		IdempotencyKey: "message:stale-source",
		Authorize: func(context.Context, db.Querier) error {
			authorized = true
			return errStaleActorInputSend
		},
	})
	if !errors.Is(err, errStaleActorInputSend) {
		t.Fatalf("error = %v, want stale Actor input source", err)
	}
	if !authorized {
		t.Fatal("new Run-sourced append did not authorize its source")
	}
	if store.commits != 0 || store.rollbacks != 1 {
		t.Fatalf("transactions: commits=%d rollbacks=%d", store.commits, store.rollbacks)
	}
}

func TestAppendActorInputMapsPostgresStorageBoundAndRollsBack(t *testing.T) {
	environmentID := uuid.New()
	actorID := uuid.New()
	store, manager := newActorInputClaimStore(t)
	store.locator = db.Actor{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       pgvalue.UUID(uuid.New()),
		NextInputSequence: 1,
	}
	store.appendErr = &pgconn.PgError{
		Code:           "23514",
		ConstraintName: "actor_records_data_size_check",
	}
	server := &Server{db: store, claims: manager}

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		ActorID:        actorID,
		RecordID:       uuid.New(),
		Data:           []byte(`{"message":"queued"}`),
		SourceKind:     "external",
		IdempotencyKey: "message:3",
	})
	if !errors.Is(err, errActorInputTooLarge) {
		t.Fatalf("error = %v, want Actor input too large", err)
	}
	if store.commits != 0 || store.rollbacks != 1 {
		t.Fatalf("transactions: commits=%d rollbacks=%d", store.commits, store.rollbacks)
	}
}

func TestAppendActorInputClassifiesLockedSequenceExhaustion(t *testing.T) {
	environmentID := uuid.New()
	actorID := uuid.New()
	store, manager := newActorInputClaimStore(t)
	store.locator = db.Actor{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       pgvalue.UUID(uuid.New()),
		State:             "open",
		NextInputSequence: maxActorSequence,
	}
	store.locatorAfterAppend = db.Actor{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       store.locator.WorkspaceID,
		State:             "open",
		NextInputSequence: maxActorSequence + 1,
	}
	store.appendErr = pgx.ErrNoRows
	server := &Server{db: store, claims: manager}

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID: environmentID,
		ActorID:       actorID,
		RecordID:      uuid.New(),
		Data:          []byte(`{"message":"queued"}`),
		SourceKind:    "external",
	})
	if !errors.Is(err, errActorSequenceExhausted) {
		t.Fatalf("error = %v, want Actor sequence exhausted", err)
	}
}

type actorInputClaimStore struct {
	db.Querier
	hmacVersion        db.LookupHmacVersion
	claim              db.IdempotencyClaim
	addressed          db.Actor
	locator            db.Actor
	locatorAfterAppend db.Actor
	project            db.Project
	environment        db.Environment
	appendErr          error
	calls              []string
	actorReads         int
	addressReads       int
	commits            int
	rollbacks          int
}

func newActorInputClaimStore(t *testing.T) (*actorInputClaimStore, idempotency.Manager) {
	t.Helper()
	hashes, err := keyedhash.New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, keyedhash.KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := hashes.Fingerprint(1)
	if err != nil {
		t.Fatal(err)
	}
	return &actorInputClaimStore{
		hmacVersion: db.LookupHmacVersion{
			Version:        1,
			KeyFingerprint: fingerprint[:],
			IsCurrent:      true,
		},
	}, idempotency.New(hashes)
}

func completeActorInputClaim(
	t *testing.T,
	store *actorInputClaimStore,
	manager idempotency.Manager,
	environmentID uuid.UUID,
	actorID uuid.UUID,
	key string,
	input []byte,
	recordID uuid.UUID,
	sequence int64,
) {
	t.Helper()
	request, err := idempotency.NewActorInputSendRequest(
		environmentID,
		actorID,
		key,
		input,
	)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := manager.TransactionForQueries(store)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := claims.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	store.claim = acquired.Claim
	store.claim.State = "completed"
	store.claim.Receipt = []byte(
		`{"recordId":"` + recordID.String() + `","sequence":` +
			fmt.Sprintf("%d", sequence) + `}`,
	)
}

func (s *actorInputClaimStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	return s, actorInputClaimTransaction{store: s}, nil
}

func (s *actorInputClaimStore) LockIdempotencySlot(context.Context, int64) error {
	s.calls = append(s.calls, "claim_lock")
	return nil
}

func (s *actorInputClaimStore) LockActiveLookupHMACVersions(context.Context) ([]db.LookupHmacVersion, error) {
	s.calls = append(s.calls, "hmac_lock")
	return []db.LookupHmacVersion{s.hmacVersion}, nil
}

func (s *actorInputClaimStore) FindLiveIdempotencyClaims(
	context.Context,
	db.FindLiveIdempotencyClaimsParams,
) ([]db.FindLiveIdempotencyClaimsRow, error) {
	s.calls = append(s.calls, "claim_find")
	if !s.claim.ID.Valid {
		return nil, nil
	}
	return []db.FindLiveIdempotencyClaimsRow{{
		ID:                 s.claim.ID,
		EnvironmentID:      s.claim.EnvironmentID,
		Operation:          s.claim.Operation,
		ScopeHash:          s.claim.ScopeHash,
		KeyHash:            s.claim.KeyHash,
		HashKeyVersion:     s.claim.HashKeyVersion,
		Generation:         s.claim.Generation,
		RequestFingerprint: s.claim.RequestFingerprint,
		State:              s.claim.State,
		Receipt:            s.claim.Receipt,
		AcceptedAt:         s.claim.AcceptedAt,
		ExpiresAt:          s.claim.ExpiresAt,
		RetiredAt:          s.claim.RetiredAt,
		CompletedAt:        s.claim.CompletedAt,
	}}, nil
}

func (s *actorInputClaimStore) GetLatestIdempotencyClaimGeneration(
	context.Context,
	db.GetLatestIdempotencyClaimGenerationParams,
) (int64, error) {
	s.calls = append(s.calls, "claim_generation")
	return 0, nil
}

func (s *actorInputClaimStore) CreateIdempotencyClaim(
	_ context.Context,
	params db.CreateIdempotencyClaimParams,
) (db.IdempotencyClaim, error) {
	s.calls = append(s.calls, "claim_create")
	s.claim = db.IdempotencyClaim{
		ID:                 params.ID,
		EnvironmentID:      params.EnvironmentID,
		Operation:          params.Operation,
		ScopeHash:          params.ScopeHash,
		KeyHash:            params.KeyHash,
		HashKeyVersion:     params.HashKeyVersion,
		Generation:         params.Generation,
		RequestFingerprint: params.RequestFingerprint,
		State:              "pending",
	}
	return s.claim, nil
}

func (s *actorInputClaimStore) GetActor(context.Context, db.GetActorParams) (db.Actor, error) {
	s.calls = append(s.calls, "actor_get")
	s.actorReads++
	if s.actorReads > 1 && s.locatorAfterAppend.ID.Valid {
		return s.locatorAfterAppend, nil
	}
	if s.locator.ID.Valid {
		return s.locator, nil
	}
	return db.Actor{}, pgx.ErrNoRows
}

func (s *actorInputClaimStore) LockWorkspaceSecretsForAdmission(
	context.Context,
	pgtype.UUID,
) ([]db.LockWorkspaceSecretsForAdmissionRow, error) {
	return nil, nil
}

func (s *actorInputClaimStore) AppendActorInputRecord(
	_ context.Context,
	params db.AppendActorInputRecordParams,
) (db.AppendActorInputRecordRow, error) {
	if s.appendErr != nil {
		return db.AppendActorInputRecordRow{}, s.appendErr
	}
	return db.AppendActorInputRecordRow{
		ID:            params.ID,
		EnvironmentID: params.EnvironmentID,
		ActorID:       params.ActorID,
		Direction:     "input",
		Sequence:      s.locator.NextInputSequence,
		Data:          params.Data,
		ContentType:   "application/json",
		SourceKind:    params.SourceKind,
		SourceRunID:   params.SourceRunID,
		ClaimID:       params.ClaimID,
		Appended:      true,
	}, nil
}

func (s *actorInputClaimStore) GetActorByKey(
	_ context.Context,
	params db.GetActorByKeyParams,
) (db.Actor, error) {
	s.addressReads++
	if !s.addressed.ID.Valid ||
		s.addressed.EnvironmentID != params.EnvironmentID ||
		s.addressed.ActorDeclaredID != params.ActorDeclaredID ||
		s.addressed.Key != params.Key {
		return db.Actor{}, pgx.ErrNoRows
	}
	return s.addressed, nil
}

func (s *actorInputClaimStore) GetProject(
	_ context.Context,
	params db.GetProjectParams,
) (db.Project, error) {
	if s.project.ID != params.ID || s.project.OrgID != params.OrgID {
		return db.Project{}, pgx.ErrNoRows
	}
	return s.project, nil
}

func (s *actorInputClaimStore) GetEnvironment(
	_ context.Context,
	params db.GetEnvironmentParams,
) (db.Environment, error) {
	if s.environment.ID != params.ID ||
		s.environment.ProjectID != params.ProjectID ||
		s.environment.OrgID != params.OrgID {
		return db.Environment{}, pgx.ErrNoRows
	}
	return s.environment, nil
}

type actorInputClaimTransaction struct {
	store *actorInputClaimStore
}

func (tx actorInputClaimTransaction) Commit(context.Context) error {
	tx.store.calls = append(tx.store.calls, "commit")
	tx.store.commits++
	return nil
}

func (tx actorInputClaimTransaction) Rollback(context.Context) error {
	tx.store.calls = append(tx.store.calls, "rollback")
	tx.store.rollbacks++
	return nil
}
