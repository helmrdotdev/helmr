package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseRunFinalization(t *testing.T) {
	request := validRunFinalizationRequest()
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.operationID.String() != request.OperationID ||
		parsed.kind != api.WorkerRunFinalizationCapture ||
		parsed.fingerprint == "" {
		t.Fatalf("parsed finalization = %+v", parsed)
	}
}

func TestRunFinalizationFingerprintUsesUTCInstants(t *testing.T) {
	first := validRunFinalizationRequest()
	second := first
	second.Lease.StartDeadlineAt = first.Lease.StartDeadlineAt.In(time.FixedZone("offset", 9*60*60))
	second.Lease.ExpiresAt = first.Lease.ExpiresAt.In(time.FixedZone("offset", 9*60*60))

	left, err := parseRunFinalization(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := parseRunFinalization(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint != right.fingerprint {
		t.Fatalf("fingerprints differ: %q != %q", left.fingerprint, right.fingerprint)
	}

	second.Lease.ExpiresAt = second.Lease.ExpiresAt.Add(time.Nanosecond)
	changed, err := parseRunFinalization(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint == changed.fingerprint {
		t.Fatal("changed receipt expiry did not change fingerprint")
	}
}

func TestParseRunFinalizationRejectsMismatchedQuiescenceProof(t *testing.T) {
	request := validRunFinalizationRequest()
	request.ProgramQuiesced.RunID = uuid.Must(uuid.NewV7()).String()
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("mismatched Run was accepted")
	}

	request = validRunFinalizationRequest()
	request.ProgramQuiesced.AttemptNumber++
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("mismatched Attempt was accepted")
	}

	request = validRunFinalizationRequest()
	request.ProgramQuiesced.RunLeaseID = uuid.Must(uuid.NewV7()).String()
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("mismatched Run Lease was accepted")
	}
}

func TestParseRunFinalizationRejectsUnknownKind(t *testing.T) {
	request := validRunFinalizationRequest()
	request.Kind = "archive"
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("unknown finalization kind was accepted")
	}
}

func TestBeginRunFinalizationFreezesAuthorityAndReplays(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	previousExpiry := store.authority.runLease.ExpiresAt
	previousRenewedAt := store.authority.runLease.RenewedAt
	previousReplayExpiry := store.authority.runLease.PreviousExpiresAt

	first, err := server.beginRunFinalization(context.Background(), worker, request, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if store.finalizationWrites != 3 || store.authority.runLease.State != db.RunLeaseStateFinalizing ||
		store.authority.run.ActiveStartedAt.Valid {
		t.Fatalf("finalization state = %+v, writes = %d", store.authority, store.finalizationWrites)
	}
	wantExpiry := store.finalizationTime.Time.Add(server.runFinalizationTTL)
	if !first.Lease.ExpiresAt.Equal(wantExpiry) ||
		!store.authority.workspaceLease.ExpiresAt.Time.Equal(wantExpiry) ||
		first.OperationID != request.OperationID || first.Kind != request.Kind ||
		!first.StartedAt.Equal(store.finalizationTime.Time) {
		t.Fatalf("frozen response = %+v, workspace expiry = %s", first, store.authority.workspaceLease.ExpiresAt.Time)
	}
	if store.authority.runLease.RenewedAt != previousRenewedAt ||
		store.authority.runLease.PreviousExpiresAt != previousReplayExpiry ||
		!store.authority.runLease.ExpiresAt.Time.After(previousExpiry.Time) {
		t.Fatal("Begin rewrote renewal replay authority")
	}

	replayed, err := server.beginRunFinalization(context.Background(), worker, request, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if store.finalizationWrites != 3 || !equalRunLeaseReceipt(replayed.Lease, first.Lease) ||
		replayed.OperationID != first.OperationID || replayed.Kind != first.Kind ||
		!replayed.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("replay = %+v, writes = %d", replayed, store.finalizationWrites)
	}
}

func TestBeginRunFinalizationRejectsUnenteredAttempt(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	store.authority.attempt.EntrypointEnteredAt = pgtype.Timestamptz{}
	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); !errors.Is(err, errStaleRunFinalization) {
		t.Fatalf("error = %v, want stale finalization", err)
	}
	if store.finalizationWrites != 0 {
		t.Fatalf("writes = %d, want zero", store.finalizationWrites)
	}
}

func TestBeginRunFinalizationRejectsSameWorkspaceChild(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	parentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store.renewal.ParentRunID = parentID
	store.renewal.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	store.authority.run.ParentRunID = parentID
	store.authority.run.ParentOwnsLifecycle = store.renewal.ParentOwnsLifecycle
	store.authority.parentRun = db.Run{
		ID: parentID, WorkspaceID: store.authority.run.WorkspaceID, Status: db.RunStatusWaiting,
	}
	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); !errors.Is(err, errStaleRunFinalization) {
		t.Fatalf("error = %v, want stale finalization", err)
	}
	if store.finalizationWrites != 0 {
		t.Fatalf("writes = %d, want zero", store.finalizationWrites)
	}
}

func TestBeginRunFinalizationRejectsUnclearScope(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	store.finalizationClear = pgtype.Bool{Bool: false, Valid: true}
	assertRunFinalizationRejected(t, server, store, worker, request, parsed)
}

func TestBeginRunFinalizationRejectsExpiredAuthority(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	store.finalizationTime = store.authority.runLease.ExpiresAt
	assertRunFinalizationRejected(t, server, store, worker, request, parsed)
}

func TestBeginRunFinalizationRejectsExhaustedActiveBudget(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	store.authority.run.ActiveElapsedMs = store.authority.run.MaxActiveDurationMs
	assertRunFinalizationRejected(t, server, store, worker, request, parsed)
}

func TestBeginRunFinalizationRejectsActiveDeadline(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	store.authority.run.ActiveStartedAt = pgvalue.Timestamptz(
		store.finalizationTime.Time.Add(-time.Duration(store.authority.run.MaxActiveDurationMs) * time.Millisecond),
	)
	assertRunFinalizationRejected(t, server, store, worker, request, parsed)
}

func TestBeginRunFinalizationRejectsCheckpointingLease(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	store.authority.runLease.State = db.RunLeaseStateCheckpointing
	assertRunFinalizationRejected(t, server, store, worker, request, parsed)
}

func TestBeginRunFinalizationRejectsChangedReplay(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); err != nil {
		t.Fatal(err)
	}
	request.Lease.RequestedCPUMillis++
	changed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.beginRunFinalization(context.Background(), worker, request, changed); !errors.Is(err, errStaleRunFinalization) {
		t.Fatalf("error = %v, want stale finalization", err)
	}
	if store.finalizationWrites != 3 {
		t.Fatalf("writes = %d, want three from the original Begin", store.finalizationWrites)
	}
}

func TestBeginRunFinalizationAcceptsActorOwner(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	actorID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store.renewal.ActorID = actorID
	store.authority.run.EntrypointKind = "actor"
	store.authority.run.ActorID = actorID
	store.authority.attempt.EntrypointKind = "actor"
	store.authority.actor = db.Actor{
		ID: actorID, CurrentRunID: store.authority.run.ID, WorkspaceID: store.authority.workspace.ID,
		State: "open",
	}

	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); err != nil {
		t.Fatal(err)
	}
}

func TestBeginRunFinalizationAcceptsDifferentWorkspaceParentOwnedChild(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	parentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store.renewal.ParentRunID = parentID
	store.renewal.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	store.authority.run.ParentRunID = parentID
	store.authority.run.ParentOwnsLifecycle = store.renewal.ParentOwnsLifecycle
	store.authority.parentRun = db.Run{
		ID: parentID, WorkspaceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Status: db.RunStatusWaiting,
	}

	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); err != nil {
		t.Fatal(err)
	}
}

func TestBeginRunFinalizationAcceptsReset(t *testing.T) {
	server, _, worker, request, _ := validRunFinalizationFixture(t)
	request.Kind = api.WorkerRunFinalizationReset
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.beginRunFinalization(context.Background(), worker, request, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != api.WorkerRunFinalizationReset {
		t.Fatalf("kind = %q, want reset", response.Kind)
	}
}

func assertRunFinalizationRejected(
	t *testing.T,
	server *Server,
	store *runLeaseClaimStore,
	worker workerActor,
	request api.WorkerBeginRunFinalizationRequest,
	parsed parsedRunFinalization,
) {
	t.Helper()
	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); !errors.Is(err, errStaleRunFinalization) {
		t.Fatalf("error = %v, want stale finalization", err)
	}
	if store.finalizationWrites != 0 {
		t.Fatalf("writes = %d, want zero", store.finalizationWrites)
	}
}

func validRunFinalizationFixture(
	t *testing.T,
) (*Server, *runLeaseClaimStore, workerActor, api.WorkerBeginRunFinalizationRequest, parsedRunFinalization) {
	t.Helper()
	server, store, worker, receipt := validRunLeaseRenewalFixture(t)
	now := store.renewalTime.Time
	server.runFinalizationTTL = 30 * time.Minute
	store.authority.attempt.EntrypointEnteredAt = pgvalue.Timestamptz(now.Add(-30 * time.Second))
	store.finalizationTime = pgvalue.Timestamptz(now)
	store.finalizationClear = pgtype.Bool{Bool: true, Valid: true}
	request := api.WorkerBeginRunFinalizationRequest{
		Lease: receipt,
		ProgramQuiesced: api.WorkerRunQuiescenceProof{
			RunID: receipt.RunID, AttemptNumber: receipt.AttemptNumber, RunLeaseID: receipt.ID,
		},
		OperationID: uuid.Must(uuid.NewV7()).String(), Kind: api.WorkerRunFinalizationCapture,
	}
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	return server, store, worker, request, parsed
}

func (s *runLeaseClaimStore) LockRunFinalizationParentRun(
	context.Context,
	db.LockRunFinalizationParentRunParams,
) (db.Run, error) {
	s.calls = append(s.calls, "parent_run")
	return s.authority.parentRun, nil
}

func (s *runLeaseClaimStore) RunFinalizationScopeIsClear(
	context.Context,
	db.RunFinalizationScopeIsClearParams,
) (pgtype.Bool, error) {
	s.calls = append(s.calls, "finalization_scope")
	return s.finalizationClear, nil
}

func (s *runLeaseClaimStore) GetRunFinalizationTime(context.Context) (pgtype.Timestamptz, error) {
	s.calls = append(s.calls, "finalization_time")
	return s.finalizationTime, nil
}

func (s *runLeaseClaimStore) CloseRunActiveIntervalForFinalization(
	_ context.Context,
	params db.CloseRunActiveIntervalForFinalizationParams,
) (db.Run, error) {
	s.calls = append(s.calls, "close_active_interval")
	if !s.authority.run.ActiveStartedAt.Valid ||
		params.ExpectedStateVersion != s.authority.run.StateVersion {
		return db.Run{}, pgx.ErrNoRows
	}
	elapsed := params.FinalizationStartedAt.Time.Sub(s.authority.run.ActiveStartedAt.Time).Milliseconds()
	s.authority.run.ActiveElapsedMs += elapsed
	s.authority.run.ActiveStartedAt = pgtype.Timestamptz{}
	s.authority.run.StateVersion++
	s.finalizationWrites++
	return s.authority.run, nil
}

func (s *runLeaseClaimStore) BeginRunLeaseFinalization(
	_ context.Context,
	params db.BeginRunLeaseFinalizationParams,
) (db.RunLease, error) {
	s.calls = append(s.calls, "begin_run_finalization")
	if s.authority.runLease.State != db.RunLeaseStateRunning ||
		!s.authority.runLease.ExpiresAt.Time.Equal(params.PreviousExpiresAt.Time) ||
		!params.ExpiresAt.Time.After(params.PreviousExpiresAt.Time) {
		return db.RunLease{}, pgx.ErrNoRows
	}
	s.authority.runLease.State = db.RunLeaseStateFinalizing
	s.authority.runLease.ExpiresAt = params.ExpiresAt
	s.authority.runLease.FinalizationOperationID = params.FinalizationOperationID
	s.authority.runLease.FinalizationKind = params.FinalizationKind
	s.authority.runLease.FinalizationStartedAt = params.FinalizationStartedAt
	s.authority.runLease.FinalizationRequestFingerprint = params.FinalizationRequestFingerprint
	s.finalizationWrites++
	return s.authority.runLease, nil
}

func (s *runLeaseClaimStore) BeginRunWorkspaceLeaseFinalization(
	_ context.Context,
	params db.BeginRunWorkspaceLeaseFinalizationParams,
) (db.WorkspaceLease, error) {
	s.calls = append(s.calls, "begin_workspace_finalization")
	if !s.authority.workspaceLease.ExpiresAt.Time.Equal(params.PreviousExpiresAt.Time) ||
		!params.ExpiresAt.Time.After(params.PreviousExpiresAt.Time) {
		return db.WorkspaceLease{}, pgx.ErrNoRows
	}
	s.authority.workspaceLease.ExpiresAt = params.ExpiresAt
	s.finalizationWrites++
	return s.authority.workspaceLease, nil
}

func validRunFinalizationRequest() api.WorkerBeginRunFinalizationRequest {
	lease := validRunLeaseReceipt(uuid.Must(uuid.NewV7()))
	lease.StartDeadlineAt = time.Unix(1_800_000_000, 123_456_789).UTC()
	lease.ExpiresAt = time.Unix(1_800_000_100, 987_654_321).UTC()
	return api.WorkerBeginRunFinalizationRequest{
		Lease: lease,
		ProgramQuiesced: api.WorkerRunQuiescenceProof{
			RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunLeaseID: lease.ID,
		},
		OperationID: uuid.Must(uuid.NewV7()).String(),
		Kind:        api.WorkerRunFinalizationCapture,
	}
}
