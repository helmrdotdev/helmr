package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAppendActorInputCompletedClaimBypassesCurrentActorState(t *testing.T) {
	environmentID := uuid.NewV7()
	actorID := uuid.NewV7()
	recordID := uuid.NewV7()
	store := newActorInputClaimStore()
	completeActorInputClaim(
		t,
		store,
		environmentID,
		actorID,
		"message:1",
		[]byte(`{"a":1,"b":2}`),
		recordID,
		7,
	)
	store.calls = nil

	server := &Server{db: store}
	authorized := false
	record, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		SessionID:      actorID,
		RecordID:       uuid.NewV7(),
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
	if !slices.Equal(store.calls, []string{"claim_lock", "record_get", "commit"}) {
		t.Fatalf("calls = %v", store.calls)
	}
}

func TestAppendActorInputRollsBackNewClaimWhenActorIsUnavailable(t *testing.T) {
	environmentID := uuid.NewV7()
	actorID := uuid.NewV7()
	store := newActorInputClaimStore()
	server := &Server{db: store}

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		SessionID:      actorID,
		RecordID:       uuid.NewV7(),
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
		"claim_create",
		"actor_get",
		"rollback",
	}) {
		t.Fatalf("calls = %v", store.calls)
	}
}

func TestAppendActorInputRollsBackProvisionalRunSourceWhenAuthorityIsStale(t *testing.T) {
	environmentID := uuid.NewV7()
	actorID := uuid.NewV7()
	sourceRunID := uuid.NewV7()
	store := newActorInputClaimStore()
	store.locator = db.Session{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       pgvalue.UUID(uuid.NewV7()),
		State:             "open",
		NextInputSequence: 3,
	}
	server := &Server{db: store}
	authorized := false

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		SessionID:      actorID,
		RecordID:       uuid.NewV7(),
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

func TestAppendActorInputRejectsOversizedCanonicalInputBeforeTransaction(t *testing.T) {
	environmentID := uuid.NewV7()
	actorID := uuid.NewV7()
	store := newActorInputClaimStore()
	server := &Server{db: store}
	data := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, maxActorInputBytes)...)
	data = append(data, '"')

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  environmentID,
		SessionID:      actorID,
		RecordID:       uuid.NewV7(),
		Data:           data,
		SourceKind:     "external",
		IdempotencyKey: "message:3",
	})
	if !errors.Is(err, errActorInputTooLarge) {
		t.Fatalf("error = %v, want Actor input too large", err)
	}
	if store.commits != 0 || store.rollbacks != 0 || len(store.calls) != 0 {
		t.Fatalf("transactions: commits=%d rollbacks=%d", store.commits, store.rollbacks)
	}
}

func TestAppendActorInputClassifiesLockedSequenceExhaustion(t *testing.T) {
	environmentID := uuid.NewV7()
	actorID := uuid.NewV7()
	store := newActorInputClaimStore()
	store.locator = db.Session{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       pgvalue.UUID(uuid.NewV7()),
		State:             "open",
		NextInputSequence: maxActorSequence,
	}
	store.locatorAfterAppend = db.Session{
		ID:                pgvalue.UUID(actorID),
		EnvironmentID:     pgvalue.UUID(environmentID),
		WorkspaceID:       store.locator.WorkspaceID,
		State:             "open",
		NextInputSequence: maxActorSequence + 1,
	}
	store.appendErr = pgx.ErrNoRows
	server := &Server{db: store}

	_, err := server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID: environmentID,
		SessionID:     actorID,
		RecordID:      uuid.NewV7(),
		Data:          []byte(`{"message":"queued"}`),
		SourceKind:    "external",
	})
	if !errors.Is(err, errActorSequenceExhausted) {
		t.Fatalf("error = %v, want Actor sequence exhausted", err)
	}
}

type actorInputClaimStore struct {
	db.Querier
	claim              db.IdempotencyClaim
	addressed          db.Session
	locator            db.Session
	locatorAfterAppend db.Session
	replayRecord       db.SessionRecord
	project            db.Project
	environment        db.Environment
	appendErr          error
	calls              []string
	actorReads         int
	addressReads       int
	commits            int
	rollbacks          int
}

func newActorInputClaimStore() *actorInputClaimStore {
	return &actorInputClaimStore{}
}

func completeActorInputClaim(
	t *testing.T,
	store *actorInputClaimStore,
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
	claims, err := idempotency.TransactionForQueries(store)
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
		`{"session_record_id":"` + recordID.String() + `","sequence":` +
			fmt.Sprintf("%d", sequence) + `}`,
	)
	store.replayRecord = db.SessionRecord{
		ID: pgvalue.UUID(recordID), EnvironmentID: pgvalue.UUID(environmentID),
		SessionID: pgvalue.UUID(actorID), Direction: "input", Sequence: sequence,
		Data: input, ContentType: "application/json", SourceKind: pgvalue.Text("external"),
		ClaimID: store.claim.ID, CreatedAt: pgvalue.Timestamptz(time.Now().UTC()),
	}
}

func (s *actorInputClaimStore) GetActorInputRecordByIDForUpdate(
	_ context.Context,
	params db.GetActorInputRecordByIDForUpdateParams,
) (db.SessionRecord, error) {
	s.calls = append(s.calls, "record_get")
	if s.replayRecord.EnvironmentID != params.EnvironmentID ||
		s.replayRecord.SessionID != params.SessionID ||
		s.replayRecord.ID != params.ID {
		return db.SessionRecord{}, pgx.ErrNoRows
	}
	return s.replayRecord, nil
}

func (s *actorInputClaimStore) BeginQuerier(context.Context) (db.Querier, transaction, error) {
	return s, actorInputClaimTransaction{store: s}, nil
}

func (s *actorInputClaimStore) LockLiveIdempotencyClaim(
	context.Context,
	db.LockLiveIdempotencyClaimParams,
) (db.LockLiveIdempotencyClaimRow, error) {
	s.calls = append(s.calls, "claim_lock")
	if !s.claim.ID.Valid {
		return db.LockLiveIdempotencyClaimRow{}, pgx.ErrNoRows
	}
	return db.LockLiveIdempotencyClaimRow{
		ID:                 s.claim.ID,
		EnvironmentID:      s.claim.EnvironmentID,
		Operation:          s.claim.Operation,
		SlotHash:           s.claim.SlotHash,
		RequestFingerprint: s.claim.RequestFingerprint,
		State:              s.claim.State,
		Receipt:            s.claim.Receipt,
		AcceptedAt:         s.claim.AcceptedAt,
		ExpiresAt:          s.claim.ExpiresAt,
		RetiredAt:          s.claim.RetiredAt,
		CompletedAt:        s.claim.CompletedAt,
	}, nil
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
		SlotHash:           params.SlotHash,
		RequestFingerprint: params.RequestFingerprint,
		State:              "pending",
	}
	return s.claim, nil
}

func (s *actorInputClaimStore) GetActor(_ context.Context, params db.GetActorParams) (db.Session, error) {
	s.calls = append(s.calls, "actor_get")
	s.actorReads++
	if s.actorReads > 1 && s.locatorAfterAppend.ID == params.ID && s.locatorAfterAppend.EnvironmentID == params.EnvironmentID {
		return s.locatorAfterAppend, nil
	}
	if s.locator.ID == params.ID && s.locator.EnvironmentID == params.EnvironmentID {
		return s.locator, nil
	}
	return db.Session{}, pgx.ErrNoRows
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
		SessionID:     params.SessionID,
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
) (db.Session, error) {
	s.addressReads++
	if !s.addressed.ID.Valid ||
		s.addressed.EnvironmentID != params.EnvironmentID ||
		s.addressed.ActorDeclaredID != params.ActorDeclaredID ||
		s.addressed.Key != params.Key {
		return db.Session{}, pgx.ErrNoRows
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
