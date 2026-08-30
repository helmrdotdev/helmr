package controlplane

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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
		parsed.kind != workerapi.RunFinalizationCapture ||
		parsed.fingerprint == "" {
		t.Fatalf("parsed finalization = %+v", parsed)
	}
}

func TestRunFinalizationFingerprintIncludesLeaseFence(t *testing.T) {
	first := validRunFinalizationRequest()
	second := first
	second.Lease.LeaseSequence++
	left, err := parseRunFinalization(first)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := parseRunFinalization(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint == changed.fingerprint {
		t.Fatal("changed Lease fence did not change fingerprint")
	}
}

func TestParseRunFinalizationRejectsMismatchedQuiescenceProof(t *testing.T) {
	request := validRunFinalizationRequest()
	request.ProgramQuiesced.AttemptNumber = 0
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("invalid Attempt was accepted")
	}

	request = validRunFinalizationRequest()
	request.ProgramQuiesced.RunLeaseID = uuid.NewV7().String()
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
	wantExpiry := store.finalizationTime.Time.Add(run.FinalizationTTL)
	if !first.ExpiresAt.Equal(wantExpiry) ||
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
	if store.finalizationWrites != 3 || replayed.Lease != first.Lease ||
		!replayed.ExpiresAt.Equal(first.ExpiresAt) ||
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

func TestBeginRunFinalizationRejectsMissingSameWorkspaceParentEdge(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	parentID := pgvalue.UUID(uuid.NewV7())
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
	request.OperationID = uuid.NewV7().String()
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
	actorID := pgvalue.UUID(uuid.NewV7())
	store.renewal.SessionID = actorID
	store.authority.run.EntrypointKind = "actor"
	store.authority.run.SessionID = actorID
	store.authority.attempt.EntrypointKind = "actor"
	store.authority.actor = db.Session{
		ID: actorID, CurrentRunID: store.authority.run.ID, WorkspaceID: store.authority.workspace.ID,
		State: "open",
	}

	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); err != nil {
		t.Fatal(err)
	}
}

func TestBeginRunFinalizationAcceptsDifferentWorkspaceParentOwnedChild(t *testing.T) {
	server, store, worker, request, parsed := validRunFinalizationFixture(t)
	parentID := pgvalue.UUID(uuid.NewV7())
	store.renewal.ParentRunID = parentID
	store.renewal.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	store.authority.run.ParentRunID = parentID
	store.authority.run.ParentOwnsLifecycle = store.renewal.ParentOwnsLifecycle
	store.authority.parentRun = db.Run{
		ID: parentID, WorkspaceID: pgvalue.UUID(uuid.NewV7()), Status: db.RunStatusWaiting,
	}

	if _, err := server.beginRunFinalization(context.Background(), worker, request, parsed); err != nil {
		t.Fatal(err)
	}
}

func TestBeginRunFinalizationAcceptsReset(t *testing.T) {
	server, _, worker, request, _ := validRunFinalizationFixture(t)
	request.Kind = workerapi.RunFinalizationReset
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.beginRunFinalization(context.Background(), worker, request, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != workerapi.RunFinalizationReset {
		t.Fatalf("kind = %q, want reset", response.Kind)
	}
}

func TestLockLiveRunFinalizationAuthorityLocksLineageBeforePhysicalAuthority(t *testing.T) {
	_, store, worker, receipt := validRunLeaseRenewalFixture(t)
	id := func() pgtype.UUID { return pgvalue.UUID(uuid.NewV7()) }
	environmentID := store.renewal.EnvironmentID
	workspaceID := store.renewal.WorkspaceID
	actorID := id()
	rootID, middleID, parentID, childID := id(), id(), id(), id()
	store.renewal.RunID = childID
	store.renewal.ParentRunID = parentID
	store.renewal.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	store.authority.run.ID = childID
	store.authority.run.ParentRunID = parentID
	store.authority.run.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	store.authority.attempt.RunID = childID
	store.authority.runLease.RunID = childID
	lineageRun := func(runID, parentRunID pgtype.UUID) db.Run {
		return db.Run{
			ID: runID, OrgID: store.renewal.OrgID, ProjectID: store.renewal.ProjectID,
			EnvironmentID: environmentID,
			WorkspaceID:   workspaceID, Status: db.RunStatusWaiting, CurrentAttemptNumber: 1,
			ParentRunID: parentRunID,
			ParentOwnsLifecycle: pgtype.Bool{
				Bool: parentRunID.Valid, Valid: parentRunID.Valid,
			},
		}
	}
	root := lineageRun(rootID, pgtype.UUID{})
	root.EntrypointKind = "actor"
	root.SessionID = actorID
	store.authority.actor = db.Session{
		ID: actorID, CurrentRunID: rootID, WorkspaceID: workspaceID, State: "open",
	}
	store.finalizationLineage = []db.ListSameWorkspaceAncestorRunsRow{
		{Run: root, Depth: 2},
		{Run: lineageRun(middleID, rootID), Depth: 1},
		{Run: lineageRun(parentID, middleID), Depth: 0},
	}
	store.calls = nil
	authority, err := lockLiveRunFinalizationAuthority(
		context.Background(),
		store,
		worker,
		pgvalue.UUID(uuid.MustParse(receipt.ID)),
		receipt.LeaseSequence,
		store.renewal,
	)
	if err != nil {
		t.Fatal(err)
	}
	if authority.parentRun.ID != parentID {
		t.Fatalf("parent = %s, want %s", pgvalue.UUIDString(authority.parentRun.ID), pgvalue.UUIDString(parentID))
	}
	want := []string{
		"actor",
		"parent_run:" + pgvalue.UUIDString(rootID),
		"parent_run:" + pgvalue.UUIDString(middleID),
		"parent_run:" + pgvalue.UUIDString(parentID),
		"run",
		"workspace",
		"lineage_attempt:" + pgvalue.UUIDString(rootID),
		"lineage_attempt:" + pgvalue.UUIDString(middleID),
		"lineage_attempt:" + pgvalue.UUIDString(parentID),
		"attempt",
		"worker_group",
		"worker",
		"runtime",
		"renewal_lease",
		"workspace_mount",
		"workspace_lease",
	}
	if !slices.Equal(store.calls, want) {
		t.Fatalf("lock calls = %v, want %v", store.calls, want)
	}
}

func assertRunFinalizationRejected(
	t *testing.T,
	server *Server,
	store *runLeaseClaimStore,
	worker workerActor,
	request workerapi.BeginRunFinalizationRequest,
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
) (*Server, *runLeaseClaimStore, workerActor, workerapi.BeginRunFinalizationRequest, parsedRunFinalization) {
	t.Helper()
	server, store, worker, receipt := validRunLeaseRenewalFixture(t)
	now := store.renewalTime.Time
	store.authority.attempt.EntrypointEnteredAt = pgvalue.Timestamptz(now.Add(-30 * time.Second))
	store.finalizationTime = pgvalue.Timestamptz(now)
	store.finalizationClear = pgtype.Bool{Bool: true, Valid: true}
	request := workerapi.BeginRunFinalizationRequest{
		Lease: receipt.Fence(),
		ProgramQuiesced: workerapi.RunQuiescenceProof{
			RunID: receipt.RunID, AttemptNumber: receipt.AttemptNumber, RunLeaseID: receipt.ID,
		},
		OperationID: uuid.NewV7().String(), Kind: workerapi.RunFinalizationCapture,
	}
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	return server, store, worker, request, parsed
}

func (s *runLeaseClaimStore) LockParentOwnedChildWait(
	_ context.Context,
	params db.LockParentOwnedChildWaitParams,
) (db.RunWait, error) {
	if s.authority.enclosingWait.ID.Valid && params.ParentRunID == s.authority.enclosingWait.RunID {
		s.calls = append(s.calls, "enclosing_wait")
		return s.authority.enclosingWait, nil
	}
	if s.authority.runWait.ID.Valid {
		s.calls = append(s.calls, "same_workspace_wait")
		return s.authority.runWait, nil
	}
	return db.RunWait{}, pgx.ErrNoRows
}

func (s *runLeaseClaimStore) ListSameWorkspaceAncestorRuns(
	context.Context,
	db.ListSameWorkspaceAncestorRunsParams,
) ([]db.ListSameWorkspaceAncestorRunsRow, error) {
	return s.finalizationLineage, nil
}

func (s *runLeaseClaimStore) LockRunFinalizationParentRun(
	_ context.Context,
	params db.LockRunFinalizationParentRunParams,
) (db.Run, error) {
	s.calls = append(s.calls, "parent_run:"+pgvalue.UUIDString(params.ID))
	for _, row := range s.finalizationLineage {
		if row.Run.ID == params.ID {
			return row.Run, nil
		}
	}
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

func validRunFinalizationRequest() workerapi.BeginRunFinalizationRequest {
	lease := validRunLeaseAssignment(uuid.NewV7())
	lease.StartDeadlineAt = time.Unix(1_800_000_000, 123_456_789).UTC()
	lease.ExpiresAt = time.Unix(1_800_000_100, 987_654_321).UTC()
	return workerapi.BeginRunFinalizationRequest{
		Lease: lease.Fence(),
		ProgramQuiesced: workerapi.RunQuiescenceProof{
			RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunLeaseID: lease.ID,
		},
		OperationID: uuid.NewV7().String(),
		Kind:        workerapi.RunFinalizationCapture,
	}
}
