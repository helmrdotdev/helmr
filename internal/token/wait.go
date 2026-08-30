package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrWaitAuthority = errors.New("token wait reconciliation authority is inconsistent")

const maxWaitBatch = int32(1000)

type WaitDB interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type WaitBatch struct {
	Examined int
	Resolved int
	Deferred int
}

type WaitRegistration struct {
	TokenID                       uuid.UUID
	WaitID                        uuid.UUID
	ResumeAttachID                uuid.UUID
	RunLeaseID                    uuid.UUID
	LeaseSequence                 int64
	WorkerGroupID                 string
	WorkerInstanceID              uuid.UUID
	WorkerEpoch                   int64
	RequestFingerprint            string
	ActorSpeculativeInputSequence pgtype.Int8
	TimeoutAt                     pgtype.Timestamptz
	IdleTimeoutMS                 pgtype.Int8
	CheckpointDueAt               pgtype.Timestamptz
	Metadata                      json.RawMessage
	Tags                          []string
}

type WaitRegistrationResult struct {
	WaitID          uuid.UUID
	RunStateVersion int64
	ConditionState  db.WaitState
	SuspensionState db.RunWaitState
	Result          json.RawMessage
	ReasonCode      string
}

type WaitReconciler struct {
	db      WaitDB
	queries *db.Queries
}

func NewWaitReconciler(database WaitDB) (*WaitReconciler, error) {
	if database == nil {
		return nil, errors.New("token wait reconciliation database is required")
	}
	return &WaitReconciler{db: database, queries: db.New(database)}, nil
}

// RegisterWait serializes the Run-to-Token race. The Wait is inserted before
// the Token is locked, so either registration observes a prior terminal Token
// or a concurrent terminalization publishes an intent after this transaction.
func (r *WaitReconciler) RegisterWait(
	ctx context.Context,
	request WaitRegistration,
) (WaitRegistrationResult, error) {
	if request.TokenID == uuid.Nil() || request.WaitID == uuid.Nil() ||
		request.ResumeAttachID == uuid.Nil() || request.RunLeaseID == uuid.Nil() ||
		request.WorkerInstanceID == uuid.Nil() {
		return WaitRegistrationResult{}, errors.New("token wait registration IDs are required")
	}
	if request.LeaseSequence <= 0 || request.WorkerEpoch <= 0 || request.WorkerGroupID == "" ||
		len(request.RequestFingerprint) != 71 || request.RequestFingerprint[:7] != "sha256:" {
		return WaitRegistrationResult{}, errors.New("token wait registration fences are invalid")
	}
	if request.ActorSpeculativeInputSequence.Valid && request.ActorSpeculativeInputSequence.Int64 < 0 {
		return WaitRegistrationResult{}, errors.New("token wait actor speculative cursor is invalid")
	}
	metadata := request.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) {
		return WaitRegistrationResult{}, errors.New("token wait registration metadata is invalid")
	}
	tags := request.Tags
	if tags == nil {
		tags = []string{}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return WaitRegistrationResult{}, fmt.Errorf("begin token wait registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	// An exact existing registration is immutable and may outlive its run
	// lease. This read-only replay does not linearize creation; the mutable
	// path repeats it after locking the run lineage.
	if replay, found, err := replayTokenWaitRegistration(ctx, q, request, metadata, tags); err != nil {
		return WaitRegistrationResult{}, err
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return WaitRegistrationResult{}, fmt.Errorf("commit token wait registration replay: %w", err)
		}
		return replay, nil
	}
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(request.RunLeaseID), LeaseSequence: request.LeaseSequence,
		WorkerGroupID: request.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(request.WorkerInstanceID),
		WorkerEpoch: request.WorkerEpoch,
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("load token wait lease authority", err)
	}
	environmentID := pgvalue.MustUUIDValue(locators.EnvironmentID)
	runID := pgvalue.MustUUIDValue(locators.RunID)
	attemptNumber := locators.AttemptNumber
	locator, err := q.GetTokenWaitRegistrationLocator(
		ctx,
		db.GetTokenWaitRegistrationLocatorParams{
			EnvironmentID: locators.EnvironmentID,
			RunID:         locators.RunID,
		},
	)
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("load token wait registration locator", err)
	}
	var lockedActorCurrentRunID pgtype.UUID
	var lockedActorCommittedInputSequence, lockedActorNextInputSequence int64
	if locator.OwnerSessionID.Valid {
		actor, err := q.LockTokenWaitActor(ctx, locator.OwnerSessionID)
		if err != nil {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("lock owning actor", err)
		}
		if actor.State != "open" && actor.State != "closing" {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("owning actor is not active", nil)
		}
		lockedActorCurrentRunID = actor.CurrentRunID
		lockedActorCommittedInputSequence = actor.CommittedInputSequence
		lockedActorNextInputSequence = actor.NextInputSequence
	}

	lineage, run, err := lockTokenWaitLineage(ctx, q, environmentID, runID)
	if err != nil {
		return WaitRegistrationResult{}, err
	}
	if replay, found, err := replayTokenWaitRegistration(ctx, q, request, metadata, tags); err != nil {
		return WaitRegistrationResult{}, err
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return WaitRegistrationResult{}, fmt.Errorf("commit token wait registration replay: %w", err)
		}
		return replay, nil
	}
	if pgvalue.UUID(run.workspaceID) != locator.WorkspaceID || run.status != db.RunStatusRunning ||
		run.currentAttempt != attemptNumber ||
		!run.currentRunLeaseID.Valid || uuid.UUID(run.currentRunLeaseID.Bytes) != request.RunLeaseID ||
		!run.activeStartedAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("run registration fence does not match", nil)
	}
	workspace, err := validateAndLockTokenWaitWorkspace(
		ctx,
		q,
		environmentID,
		locator,
		lineage,
		lockedActorCurrentRunID,
	)
	if err != nil {
		return WaitRegistrationResult{}, err
	}
	attempt, err := q.LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID:         locators.RunID,
		AttemptNumber: attemptNumber,
		WorkspaceID:   locator.WorkspaceID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current run attempt", err)
	}
	if err := validateTokenWaitActorCursor(
		request.ActorSpeculativeInputSequence, locator.OwnerSessionID, lockedActorCurrentRunID,
		lockedActorCommittedInputSequence, lockedActorNextInputSequence,
		run, attempt.EntrypointKind, attempt.SessionInputStartSequence,
	); err != nil {
		return WaitRegistrationResult{}, err
	}
	workerGroup, err := q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: request.WorkerGroupID, RegionID: locators.RegionID,
	})
	if err != nil ||
		(workerGroup.State != db.WorkerGroupStateActive && workerGroup.State != db.WorkerGroupStateDraining) {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock active worker group", err)
	}
	worker, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerGroupID: request.WorkerGroupID,
	})
	if err != nil ||
		(worker.State != db.WorkerInstanceStateActive && worker.State != db.WorkerInstanceStateDraining) ||
		!worker.CurrentEpoch.Valid ||
		worker.CurrentEpoch.Int64 != request.WorkerEpoch ||
		!worker.RuntimeIdentityID.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current worker epoch", err)
	}
	runtime, err := q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: locators.EnvironmentID,
		RegionID: locators.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		WorkspaceID: locator.WorkspaceID,
	})
	if err != nil || runtime.RuntimeIdentityID != worker.RuntimeIdentityID.String ||
		runtime.DesiredState != db.RuntimeDesiredStateReady || runtime.ObservedState != db.RuntimeObservedStateReady ||
		runtime.ObservedDesiredVersion != runtime.DesiredVersion || runtime.TerminalAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock ready runtime", err)
	}
	leaseState, err := q.LockTokenWaitRunLease(ctx, db.LockTokenWaitRunLeaseParams{
		ID:                pgvalue.UUID(request.RunLeaseID),
		RunID:             locators.RunID,
		AttemptNumber:     attemptNumber,
		WorkspaceID:       locator.WorkspaceID,
		LeaseSequence:     request.LeaseSequence,
		WorkerGroupID:     request.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(request.WorkerInstanceID),
		WorkerEpoch:       request.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		RuntimeIdentityID: runtime.RuntimeIdentityID,
		RegionID:          locators.RegionID,
	})
	if err != nil || db.RunLeaseState(leaseState) != db.RunLeaseStateRunning {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired run lease", err)
	}
	mount, err := q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: locators.EnvironmentID,
		RegionID: locators.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locator.WorkspaceID,
	})
	if err != nil || mount.State != db.WorkspaceMountStateMounted {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock mounted workspace", err)
	}
	workspaceLease, err := q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: locators.EnvironmentID,
		RegionID: locators.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locator.WorkspaceID,
		WorkspaceMountID:  locators.WorkspaceMountID,
	})
	if err != nil || !workspaceLease.OwnerRunLeaseID.Valid ||
		uuid.UUID(workspaceLease.OwnerRunLeaseID.Bytes) != request.RunLeaseID ||
		workspaceLease.OwnershipGeneration != workspace.OwnershipGeneration ||
		workspaceLease.WriterGeneration != workspace.WriterGeneration ||
		workspaceLease.MountFencingGeneration != mount.FencingGeneration {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired workspace lease", err)
	}
	if err := lockOuterTokenWait(ctx, q, runID); err != nil {
		return WaitRegistrationResult{}, err
	}

	registered, err := q.RegisterTokenWait(ctx, db.RegisterTokenWaitParams{
		WaitID:                        pgvalue.UUID(request.WaitID),
		EnvironmentID:                 locators.EnvironmentID,
		TimeoutAt:                     request.TimeoutAt,
		IdleTimeoutMs:                 request.IdleTimeoutMS,
		TokenID:                       pgvalue.UUID(request.TokenID),
		ExpectedRunningStateVersion:   run.stateVersion,
		RequestFingerprint:            request.RequestFingerprint,
		AttemptNumber:                 attemptNumber,
		ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
		CurrentRunLeaseID:             pgvalue.UUID(request.RunLeaseID),
		CheckpointDueAt:               request.CheckpointDueAt,
		ResumeAttachID:                pgvalue.UUID(request.ResumeAttachID),
		Metadata:                      metadata,
		Tags:                          tags,
		RunID:                         locators.RunID,
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("insert token wait", err)
	}
	waitingVersion := registered.ExpectedRunStateVersion

	condition, err := q.LockTokenWaitCondition(ctx, db.LockTokenWaitConditionParams{
		EnvironmentID: locators.EnvironmentID,
		TokenID:       pgvalue.UUID(request.TokenID),
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock token registration condition", err)
	}
	tokenState := db.TokenState(condition.State)

	result := WaitRegistrationResult{
		WaitID: request.WaitID, RunStateVersion: waitingVersion,
		ConditionState: db.WaitStatePending, SuspensionState: db.RunWaitStateHot,
	}
	if tokenState != db.TokenStatePending {
		resolution, err := tokenWaitTerminalResolution(tokenState, condition.Result)
		if err != nil {
			return WaitRegistrationResult{}, err
		}
		wait := tokenWaitLockedWait{
			id: request.WaitID, runID: runID,
			workspaceID: pgvalue.MustUUIDValue(locator.WorkspaceID), kind: db.WaitKindToken,
			conditionState: db.WaitStatePending, suspensionState: db.RunWaitStateHot,
			expectedRunStateVersion: waitingVersion, attemptNumber: attemptNumber,
			currentRunLeaseID: pgtype.UUID{Bytes: request.RunLeaseID, Valid: true},
		}
		run.stateVersion = waitingVersion
		run.status = db.RunStatusWaiting
		if err := reconcileHotTokenWait(ctx, q, run, wait, resolution); err != nil {
			return WaitRegistrationResult{}, err
		}
		result.RunStateVersion = waitingVersion + 1
		result.ConditionState = resolution.conditionState
		result.SuspensionState = db.RunWaitStateReleased
		result.Result = resolution.result
		if resolution.reasonCode != nil {
			result.ReasonCode = *resolution.reasonCode
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return WaitRegistrationResult{}, fmt.Errorf("commit token wait registration: %w", err)
	}
	return result, nil
}

func replayTokenWaitRegistration(
	ctx context.Context,
	q *db.Queries,
	request WaitRegistration,
	metadata json.RawMessage,
	tags []string,
) (WaitRegistrationResult, bool, error) {
	replay, err := q.GetTokenWaitRegistrationReplay(
		ctx,
		db.GetTokenWaitRegistrationReplayParams{
			RunLeaseID:                    pgvalue.UUID(request.RunLeaseID),
			WaitID:                        pgvalue.UUID(request.WaitID),
			TokenID:                       pgvalue.UUID(request.TokenID),
			ResumeAttachID:                pgvalue.UUID(request.ResumeAttachID),
			RequestFingerprint:            request.RequestFingerprint,
			Metadata:                      metadata,
			Tags:                          tags,
			LeaseSequence:                 request.LeaseSequence,
			WorkerGroupID:                 request.WorkerGroupID,
			WorkerInstanceID:              pgvalue.UUID(request.WorkerInstanceID),
			WorkerEpoch:                   request.WorkerEpoch,
			ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
		},
	)
	if err == nil {
		result := WaitRegistrationResult{
			WaitID:          pgvalue.MustUUIDValue(replay.WaitID),
			RunStateVersion: replay.RunStateVersion,
			ConditionState:  db.WaitState(replay.ConditionState),
			SuspensionState: db.RunWaitState(replay.SuspensionState),
			Result:          json.RawMessage(replay.ConditionResult),
		}
		if replay.ConditionReasonCode.Valid {
			result.ReasonCode = replay.ConditionReasonCode.String
		}
		return result, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("load token wait registration replay", err)
	}
	conflicting, err := q.TokenWaitExists(ctx, pgvalue.UUID(request.WaitID))
	if err != nil {
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("check token wait registration replay conflict", err)
	}
	if conflicting {
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("token wait registration replay does not match", nil)
	}
	return WaitRegistrationResult{}, false, nil
}

func (r *WaitReconciler) ReconcileBatch(
	ctx context.Context,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	limit int32,
) (WaitBatch, error) {
	if environmentID == uuid.Nil() || tokenID == uuid.Nil() {
		return WaitBatch{}, errors.New("token wait reconciliation IDs are required")
	}
	if limit <= 0 {
		return WaitBatch{}, errors.New("token wait reconciliation limit must be positive")
	}
	if limit > maxWaitBatch {
		return WaitBatch{}, fmt.Errorf(
			"token wait reconciliation limit must not exceed %d",
			maxWaitBatch,
		)
	}

	candidates, err := r.queries.ListTokenWaitCandidates(
		ctx,
		db.ListTokenWaitCandidatesParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			TokenID:       pgvalue.UUID(tokenID),
			RowLimit:      limit,
		},
	)
	if err != nil {
		return WaitBatch{}, fmt.Errorf("discover pending token waits: %w", err)
	}

	batch := WaitBatch{Examined: len(candidates)}
	for _, candidate := range candidates {
		resolved, deferred, err := r.reconcileOne(
			ctx,
			environmentID,
			tokenID,
			pgvalue.MustUUIDValue(candidate.WaitID),
			pgvalue.MustUUIDValue(candidate.RunID),
			false,
		)
		if err != nil {
			return batch, err
		}
		if resolved {
			batch.Resolved++
		}
		if deferred {
			batch.Deferred++
		}
	}
	return batch, nil
}

func (r *WaitReconciler) ReconcileTimeouts(
	ctx context.Context,
	limit int32,
) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if limit > maxWaitBatch {
		return 0, fmt.Errorf(
			"token wait timeout reconciliation limit must not exceed %d",
			maxWaitBatch,
		)
	}
	candidates, err := r.queries.ListTimedOutTokenWaitCandidates(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("discover timed out token waits: %w", err)
	}
	resolved := 0
	for _, candidate := range candidates {
		didResolve, _, err := r.reconcileOne(
			ctx,
			pgvalue.MustUUIDValue(candidate.EnvironmentID),
			pgvalue.MustUUIDValue(candidate.TokenID),
			pgvalue.MustUUIDValue(candidate.WaitID),
			pgvalue.MustUUIDValue(candidate.RunID),
			true,
		)
		if err != nil {
			return resolved, err
		}
		if didResolve {
			resolved++
		}
	}
	return resolved, nil
}

func validateAndLockTokenWaitWorkspace(
	ctx context.Context,
	q *db.Queries,
	environmentID uuid.UUID,
	locator db.GetTokenWaitRegistrationLocatorRow,
	lineage []tokenWaitLockedRun,
	lockedActorCurrentRunID pgtype.UUID,
) (db.LockTokenWaitWorkspaceRow, error) {
	workspace, err := q.LockTokenWaitWorkspace(ctx, db.LockTokenWaitWorkspaceParams{
		WorkspaceID:   locator.WorkspaceID,
		EnvironmentID: pgvalue.UUID(environmentID),
	})
	if err != nil {
		return db.LockTokenWaitWorkspaceRow{}, tokenWaitAuthorityError("lock run workspace", err)
	}
	if db.WorkspaceState(workspace.State) != db.WorkspaceStateActive ||
		db.WorkspaceDesiredState(workspace.DesiredState) != db.WorkspaceDesiredStateActive ||
		workspace.OwnerSessionID != locator.OwnerSessionID {
		return db.LockTokenWaitWorkspaceRow{}, tokenWaitAuthorityError("workspace ownership changed", nil)
	}
	if workspace.OwnerSessionID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return db.LockTokenWaitWorkspaceRow{}, tokenWaitAuthorityError("actor current run is outside the locked lineage", nil)
		}
	} else if !workspace.OwnerRunID.Valid ||
		!tokenWaitLineageContains(lineage, workspace.OwnerRunID) {
		return db.LockTokenWaitWorkspaceRow{}, tokenWaitAuthorityError("workspace owner run is outside the locked lineage", nil)
	}
	return workspace, nil
}

func validateTokenWaitActorCursor(
	cursor pgtype.Int8,
	ownerSessionID pgtype.UUID,
	actorCurrentRunID pgtype.UUID,
	actorCommittedInputSequence int64,
	actorNextInputSequence int64,
	run tokenWaitLockedRun,
	attemptEntrypointKind string,
	attemptSessionInputStartSequence pgtype.Int8,
) error {
	switch run.entrypointKind {
	case "task":
		if run.actorID.Valid || ownerSessionID.Valid || cursor.Valid || attemptEntrypointKind != "task" ||
			attemptSessionInputStartSequence.Valid {
			return tokenWaitAuthorityError("task token wait carries actor authority", nil)
		}
	case "actor":
		if !run.actorID.Valid || run.actorID != ownerSessionID || !actorCurrentRunID.Valid ||
			uuid.UUID(actorCurrentRunID.Bytes) != run.id || attemptEntrypointKind != "actor" ||
			!attemptSessionInputStartSequence.Valid || !cursor.Valid ||
			attemptSessionInputStartSequence.Int64 > actorCommittedInputSequence ||
			cursor.Int64 < actorCommittedInputSequence ||
			cursor.Int64 > actorCommittedInputSequence+1 ||
			cursor.Int64 >= actorNextInputSequence {
			return tokenWaitAuthorityError("actor token wait cursor authority does not match", nil)
		}
	default:
		return tokenWaitAuthorityError("token wait entrypoint kind is invalid", nil)
	}
	return nil
}

type tokenWaitLockedRun struct {
	id                uuid.UUID
	parentRunID       pgtype.UUID
	workspaceID       uuid.UUID
	actorID           pgtype.UUID
	entrypointKind    string
	status            db.RunStatus
	stateVersion      int64
	currentAttempt    int32
	currentRunLeaseID pgtype.UUID
	activeStartedAt   pgtype.Timestamptz
	depth             int32
	cycle             bool
}

type tokenWaitLockedWait struct {
	id                      uuid.UUID
	runID                   uuid.UUID
	workspaceID             uuid.UUID
	kind                    db.WaitKind
	conditionState          db.WaitState
	suspensionState         db.RunWaitState
	expectedRunStateVersion int64
	attemptNumber           int32
	currentRunLeaseID       pgtype.UUID
	priorRunLeaseID         pgtype.UUID
	suspendCheckpointID     pgtype.UUID
	timeoutAt               pgtype.Timestamptz
	timedOut                bool
}

type tokenWaitResolution struct {
	conditionState db.WaitState
	result         json.RawMessage
	reasonCode     *string
	conditionError json.RawMessage
}

func (r *WaitReconciler) reconcileOne(
	ctx context.Context,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	waitID uuid.UUID,
	runID uuid.UUID,
	timeout bool,
) (resolved bool, deferred bool, returnErr error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("begin token wait reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)

	locator, err := q.GetTokenWaitLocator(
		ctx,
		db.GetTokenWaitLocatorParams{
			WaitID:        pgvalue.UUID(waitID),
			EnvironmentID: pgvalue.UUID(environmentID),
			RunID:         pgvalue.UUID(runID),
			TokenID:       pgvalue.UUID(tokenID),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit stale token wait reconciliation: %w", err)
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}

	var lockedActorCurrentRunID pgtype.UUID
	if locator.OwnerSessionID.Valid {
		actor, err := q.LockTokenWaitActor(ctx, locator.OwnerSessionID)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock owning actor", err)
		}
		if actor.State != "open" && actor.State != "closing" {
			return false, false, tokenWaitAuthorityError("owning actor is not active", nil)
		}
		lockedActorCurrentRunID = actor.CurrentRunID
	}

	lineage, addressedRun, err := lockTokenWaitLineage(ctx, q, environmentID, runID)
	if err != nil {
		return false, false, err
	}
	workspaceID := pgvalue.MustUUIDValue(locator.WorkspaceID)
	if addressedRun.workspaceID != workspaceID ||
		addressedRun.currentAttempt != locator.AttemptNumber {
		return false, false, tokenWaitAuthorityError("run locator changed", nil)
	}
	workspace, err := q.LockTokenWaitWorkspace(ctx, db.LockTokenWaitWorkspaceParams{
		WorkspaceID:   locator.WorkspaceID,
		EnvironmentID: pgvalue.UUID(environmentID),
	})
	if err != nil {
		return false, false, tokenWaitAuthorityError("lock run workspace", err)
	}
	if db.WorkspaceState(workspace.State) != db.WorkspaceStateActive ||
		db.WorkspaceDesiredState(workspace.DesiredState) != db.WorkspaceDesiredStateActive ||
		workspace.OwnerSessionID != locator.OwnerSessionID {
		return false, false, tokenWaitAuthorityError("workspace ownership changed", nil)
	}
	if workspace.OwnerSessionID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return false, false, tokenWaitAuthorityError("actor current run is outside the locked lineage", nil)
		}
	} else if !workspace.OwnerRunID.Valid ||
		!tokenWaitLineageContains(lineage, workspace.OwnerRunID) {
		return false, false, tokenWaitAuthorityError("workspace owner run is outside the locked lineage", nil)
	}

	attempt, err := q.LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID:         locator.RunID,
		AttemptNumber: locator.AttemptNumber,
		WorkspaceID:   locator.WorkspaceID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return false, false, tokenWaitAuthorityError("lock current run attempt", err)
	}

	if err := lockOuterTokenWait(ctx, q, runID); err != nil {
		return false, false, err
	}
	wait, err := lockCurrentTokenWait(ctx, q, environmentID, tokenID, locator)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit converged token wait reconciliation: %w", err)
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if err := validateLockedTokenWait(addressedRun, wait); err != nil {
		return false, false, err
	}
	if wait.conditionState != db.WaitStatePending {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit deferred token wait reconciliation: %w", err)
		}
		return false, true, nil
	}

	var resolution tokenWaitResolution
	if timeout {
		if !wait.timeoutAt.Valid || !wait.timedOut {
			if err := tx.Commit(ctx); err != nil {
				return false, false, fmt.Errorf("commit early token wait timeout reconciliation: %w", err)
			}
			return false, false, nil
		}
		reason := "wait_timeout"
		resolution = tokenWaitResolution{
			conditionState: db.WaitStateFailed,
			reasonCode:     &reason,
			conditionError: json.RawMessage(`{"code":"wait_timeout","retryable":false}`),
		}
	} else {
		condition, err := q.LockTokenWaitCondition(ctx, db.LockTokenWaitConditionParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			TokenID:       pgvalue.UUID(tokenID),
		})
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock terminal token", err)
		}
		resolution, err = tokenWaitTerminalResolution(
			db.TokenState(condition.State),
			condition.Result,
		)
		if err != nil {
			return false, false, err
		}
	}

	switch wait.suspensionState {
	case db.RunWaitStateHot:
		err = reconcileHotTokenWait(ctx, q, addressedRun, wait, resolution)
	case db.RunWaitStateCheckpointing:
		err = reconcileCheckpointingTokenWait(ctx, q, wait, resolution)
	case db.RunWaitStateParked:
		err = reconcileParkedTokenWait(ctx, q, addressedRun, wait, resolution)
	default:
		err = tokenWaitAuthorityError("pending token wait has an ineligible suspension state", nil)
	}
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("commit token wait reconciliation: %w", err)
	}
	return true, wait.suspensionState == db.RunWaitStateCheckpointing, nil
}

func lockTokenWaitLineage(
	ctx context.Context,
	q *db.Queries,
	environmentID uuid.UUID,
	runID uuid.UUID,
) ([]tokenWaitLockedRun, tokenWaitLockedRun, error) {
	rows, err := q.LockTokenWaitRunLineage(
		ctx,
		db.LockTokenWaitRunLineageParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			RunID:         pgvalue.UUID(runID),
		},
	)
	if err != nil {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("lock run lineage", err)
	}
	lineage := make([]tokenWaitLockedRun, 0, len(rows))
	var addressed tokenWaitLockedRun
	foundAddressed := false
	for _, locked := range rows {
		row := tokenWaitLockedRun{
			id:                pgvalue.MustUUIDValue(locked.ID),
			parentRunID:       locked.ParentRunID,
			workspaceID:       pgvalue.MustUUIDValue(locked.WorkspaceID),
			actorID:           locked.SessionID,
			entrypointKind:    locked.EntrypointKind,
			status:            db.RunStatus(locked.Status),
			stateVersion:      locked.StateVersion,
			currentAttempt:    locked.CurrentAttemptNumber,
			currentRunLeaseID: locked.CurrentRunLeaseID,
			activeStartedAt:   locked.ActiveStartedAt,
			depth:             locked.Depth,
			cycle:             locked.Cycle,
		}
		if row.cycle {
			return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("run lineage contains a cycle", nil)
		}
		lineage = append(lineage, row)
		if row.depth == 0 && row.id == runID {
			addressed = row
			foundAddressed = true
		}
	}
	if !foundAddressed {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("addressed run does not exist", nil)
	}
	return lineage, addressed, nil
}

func tokenWaitLineageContains(lineage []tokenWaitLockedRun, id pgtype.UUID) bool {
	if !id.Valid {
		return false
	}
	for _, run := range lineage {
		if run.id == uuid.UUID(id.Bytes) {
			return true
		}
	}
	return false
}

func lockOuterTokenWait(ctx context.Context, q *db.Queries, runID uuid.UUID) error {
	_, err := q.LockEnclosingRunWaits(ctx, pgvalue.UUID(runID))
	if err != nil {
		return tokenWaitAuthorityError("lock enclosing run wait", err)
	}
	return nil
}

func lockCurrentTokenWait(
	ctx context.Context,
	q *db.Queries,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	locator db.GetTokenWaitLocatorRow,
) (tokenWaitLockedWait, error) {
	locked, err := q.LockTokenWait(ctx, db.LockTokenWaitParams{
		WaitID:        locator.WaitID,
		EnvironmentID: pgvalue.UUID(environmentID),
		RunID:         locator.RunID,
		WorkspaceID:   locator.WorkspaceID,
		AttemptNumber: locator.AttemptNumber,
		TokenID:       pgvalue.UUID(tokenID),
	})
	if err != nil {
		return tokenWaitLockedWait{}, err
	}
	return tokenWaitLockedWait{
		id:                      pgvalue.MustUUIDValue(locked.ID),
		runID:                   pgvalue.MustUUIDValue(locked.RunID),
		workspaceID:             pgvalue.MustUUIDValue(locked.WorkspaceID),
		kind:                    locked.Kind,
		conditionState:          db.WaitState(locked.ConditionState),
		suspensionState:         db.RunWaitState(locked.SuspensionState),
		expectedRunStateVersion: locked.ExpectedRunStateVersion,
		attemptNumber:           locked.AttemptNumber,
		currentRunLeaseID:       locked.CurrentRunLeaseID,
		priorRunLeaseID:         locked.PriorRunLeaseID,
		suspendCheckpointID:     locked.SuspendCheckpointID,
		timeoutAt:               locked.TimeoutAt,
		timedOut:                locked.TimedOut,
	}, nil
}

func validateLockedTokenWait(run tokenWaitLockedRun, wait tokenWaitLockedWait) error {
	if wait.kind != db.WaitKindToken ||
		wait.runID != run.id || wait.workspaceID != run.workspaceID ||
		wait.attemptNumber != run.currentAttempt || wait.expectedRunStateVersion != run.stateVersion ||
		run.status != db.RunStatusWaiting {
		return tokenWaitAuthorityError("run and token wait fences do not match", nil)
	}
	switch wait.suspensionState {
	case db.RunWaitStateHot, db.RunWaitStateCheckpointing:
		if wait.conditionState != db.WaitStatePending && wait.suspensionState != db.RunWaitStateCheckpointing {
			return tokenWaitAuthorityError("terminal token wait is not awaiting checkpoint readiness", nil)
		}
		if !run.currentRunLeaseID.Valid || !wait.currentRunLeaseID.Valid ||
			run.currentRunLeaseID != wait.currentRunLeaseID || wait.priorRunLeaseID.Valid ||
			!run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("hot token wait lease fence does not match", nil)
		}
	case db.RunWaitStateParked:
		if wait.conditionState != db.WaitStatePending {
			return tokenWaitAuthorityError("parked token wait is already terminal", nil)
		}
		if run.currentRunLeaseID.Valid || wait.currentRunLeaseID.Valid ||
			!wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("parked token wait provenance does not match", nil)
		}
	default:
		return tokenWaitAuthorityError("pending token wait suspension is not completable", nil)
	}
	return nil
}

func tokenWaitTerminalResolution(state db.TokenState, completionData []byte) (tokenWaitResolution, error) {
	switch state {
	case db.TokenStateCompleted:
		result := json.RawMessage(completionData)
		if len(result) == 0 {
			result = json.RawMessage(`null`)
		}
		return tokenWaitResolution{conditionState: db.WaitStateCompleted, result: result}, nil
	case db.TokenStateCancelled:
		reason := "token_cancelled"
		return tokenWaitResolution{
			conditionState: db.WaitStateCancelled,
			reasonCode:     &reason,
			conditionError: json.RawMessage(`{"code":"token_cancelled","retryable":false}`),
		}, nil
	case db.TokenStateExpired:
		reason := "token_expired"
		return tokenWaitResolution{
			conditionState: db.WaitStateFailed,
			reasonCode:     &reason,
			conditionError: json.RawMessage(`{"code":"token_expired","retryable":false}`),
		}, nil
	default:
		return tokenWaitResolution{}, tokenWaitAuthorityError("token is not terminal", nil)
	}
}

func reconcileHotTokenWait(
	ctx context.Context,
	q *db.Queries,
	run tokenWaitLockedRun,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	_, err := q.ResolveHotTokenWait(ctx, db.ResolveHotTokenWaitParams{
		ConditionState:          string(resolution.conditionState),
		ConditionResult:         resolution.result,
		ReasonCode:              pgvalue.TextPtr(resolution.reasonCode),
		ConditionError:          resolution.conditionError,
		WaitID:                  pgvalue.UUID(wait.id),
		RunID:                   pgvalue.UUID(run.id),
		ExpectedRunStateVersion: run.stateVersion,
		CurrentRunLeaseID:       run.currentRunLeaseID,
		AttemptNumber:           run.currentAttempt,
	})
	if err != nil {
		return tokenWaitAuthorityError("resolve hot token wait", err)
	}
	return nil
}

func reconcileCheckpointingTokenWait(
	ctx context.Context,
	q *db.Queries,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	_, err := q.ResolveCheckpointingTokenWait(
		ctx,
		db.ResolveCheckpointingTokenWaitParams{
			ConditionState:          string(resolution.conditionState),
			ConditionResult:         resolution.result,
			ReasonCode:              pgvalue.TextPtr(resolution.reasonCode),
			ConditionError:          resolution.conditionError,
			WaitID:                  pgvalue.UUID(wait.id),
			RunID:                   pgvalue.UUID(wait.runID),
			ExpectedRunStateVersion: wait.expectedRunStateVersion,
			CurrentRunLeaseID:       wait.currentRunLeaseID,
		},
	)
	if err != nil {
		return tokenWaitAuthorityError("complete checkpointing token wait", err)
	}
	return nil
}

func reconcileParkedTokenWait(
	ctx context.Context,
	q *db.Queries,
	run tokenWaitLockedRun,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	_, err := q.ResolveParkedTokenWait(ctx, db.ResolveParkedTokenWaitParams{
		RunID:                   pgvalue.UUID(run.id),
		ExpectedRunStateVersion: run.stateVersion,
		AttemptNumber:           run.currentAttempt,
		ConditionState:          string(resolution.conditionState),
		ConditionResult:         resolution.result,
		ReasonCode:              pgvalue.TextPtr(resolution.reasonCode),
		ConditionError:          resolution.conditionError,
		WaitID:                  pgvalue.UUID(wait.id),
		PriorRunLeaseID:         wait.priorRunLeaseID,
		SuspendCheckpointID:     wait.suspendCheckpointID,
	})
	if err != nil {
		return tokenWaitAuthorityError("resolve parked token wait", err)
	}
	return nil
}

func tokenWaitAuthorityError(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrWaitAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrWaitAuthority, operation, cause)
}
