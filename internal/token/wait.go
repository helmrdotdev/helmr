package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrWaitAuthority = errors.New("Token Wait reconciliation authority is inconsistent")

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
	EnvironmentID                 uuid.UUID
	RunID                         uuid.UUID
	TokenID                       uuid.UUID
	WaitID                        uuid.UUID
	ResumeAttachID                uuid.UUID
	ExpectedRunStateVersion       int64
	AttemptNumber                 int32
	CurrentRunLeaseID             uuid.UUID
	LeaseSequence                 int64
	WorkerGroupID                 string
	WorkerInstanceID              uuid.UUID
	WorkerEpoch                   int64
	WorkerProtocolVersion         string
	RuntimeInstanceID             uuid.UUID
	RuntimeIdentityID             string
	RegionID                      string
	NetworkSlotID                 uuid.UUID
	NetworkSlotGeneration         int64
	WorkspaceMountID              uuid.UUID
	WorkspaceLeaseID              uuid.UUID
	OwnershipGeneration           int64
	WriterGeneration              int64
	MountFencingGeneration        int64
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
		return nil, errors.New("Token Wait reconciliation database is required")
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
	if request.EnvironmentID == uuid.Nil || request.RunID == uuid.Nil || request.TokenID == uuid.Nil ||
		request.WaitID == uuid.Nil || request.ResumeAttachID == uuid.Nil || request.CurrentRunLeaseID == uuid.Nil ||
		request.WorkerInstanceID == uuid.Nil || request.RuntimeInstanceID == uuid.Nil || request.NetworkSlotID == uuid.Nil ||
		request.WorkspaceMountID == uuid.Nil || request.WorkspaceLeaseID == uuid.Nil {
		return WaitRegistrationResult{}, errors.New("Token Wait registration IDs are required")
	}
	if request.ExpectedRunStateVersion < 0 || request.AttemptNumber <= 0 || request.LeaseSequence <= 0 ||
		request.WorkerEpoch <= 0 || request.NetworkSlotGeneration <= 0 || request.OwnershipGeneration <= 0 ||
		request.WriterGeneration <= 0 || request.MountFencingGeneration <= 0 || request.WorkerGroupID == "" ||
		request.WorkerProtocolVersion == "" || request.RuntimeIdentityID == "" || request.RegionID == "" ||
		len(request.RequestFingerprint) != 71 || request.RequestFingerprint[:7] != "sha256:" {
		return WaitRegistrationResult{}, errors.New("Token Wait registration fences are invalid")
	}
	if request.ActorSpeculativeInputSequence.Valid && request.ActorSpeculativeInputSequence.Int64 < 0 {
		return WaitRegistrationResult{}, errors.New("Token Wait Actor speculative cursor is invalid")
	}
	metadata := request.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) {
		return WaitRegistrationResult{}, errors.New("Token Wait registration metadata is invalid")
	}
	tags := request.Tags
	if tags == nil {
		tags = []string{}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return WaitRegistrationResult{}, fmt.Errorf("begin Token Wait registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	if err := q.LockTokenWaitRegistration(ctx, pgvalue.UUID(request.WaitID)); err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("serialize Token Wait registration", err)
	}
	if replay, found, err := replayTokenWaitRegistration(ctx, q, request, metadata, tags); err != nil {
		return WaitRegistrationResult{}, err
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return WaitRegistrationResult{}, fmt.Errorf("commit Token Wait registration replay: %w", err)
		}
		return replay, nil
	}

	locator, err := q.GetTokenWaitRegistrationLocator(
		ctx,
		db.GetTokenWaitRegistrationLocatorParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			RunID:         pgvalue.UUID(request.RunID),
		},
	)
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("load Token Wait registration locator", err)
	}
	var lockedActorCurrentRunID pgtype.UUID
	var lockedActorCommittedInputSequence, lockedActorNextInputSequence int64
	if locator.OwnerActorID.Valid {
		actor, err := q.LockTokenWaitActor(ctx, locator.OwnerActorID)
		if err != nil {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("lock owning Actor", err)
		}
		if actor.State != "open" && actor.State != "closing" {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("owning Actor is not active", nil)
		}
		lockedActorCurrentRunID = actor.CurrentRunID
		lockedActorCommittedInputSequence = actor.CommittedInputSequence
		lockedActorNextInputSequence = actor.NextInputSequence
	}

	lineage, run, err := lockTokenWaitLineage(ctx, q, request.EnvironmentID, request.RunID)
	if err != nil {
		return WaitRegistrationResult{}, err
	}
	if pgvalue.UUID(run.workspaceID) != locator.WorkspaceID || run.status != db.RunStatusRunning ||
		run.stateVersion != request.ExpectedRunStateVersion || run.currentAttempt != request.AttemptNumber ||
		!run.currentRunLeaseID.Valid || uuid.UUID(run.currentRunLeaseID.Bytes) != request.CurrentRunLeaseID ||
		!run.activeStartedAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("Run registration fence does not match", nil)
	}
	if err := validateAndLockTokenWaitWorkspace(
		ctx,
		q,
		request.EnvironmentID,
		locator,
		lineage,
		lockedActorCurrentRunID,
	); err != nil {
		return WaitRegistrationResult{}, err
	}
	attempt, err := q.LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID:         pgvalue.UUID(request.RunID),
		AttemptNumber: request.AttemptNumber,
		WorkspaceID:   locator.WorkspaceID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current Run Attempt", err)
	}
	if err := validateTokenWaitActorCursor(
		request.ActorSpeculativeInputSequence, locator.OwnerActorID, lockedActorCurrentRunID,
		lockedActorCommittedInputSequence, lockedActorNextInputSequence,
		run, attempt.EntrypointKind, attempt.ActorStartInputSequence,
	); err != nil {
		return WaitRegistrationResult{}, err
	}
	workerGroup, err := q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: request.WorkerGroupID, RegionID: request.RegionID,
	})
	if err != nil ||
		(workerGroup.State != db.WorkerGroupStateActive && workerGroup.State != db.WorkerGroupStateDraining) ||
		!workerGroup.AllowsRun ||
		workerGroup.ProtocolVersion != request.WorkerProtocolVersion {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock active worker group", err)
	}
	worker, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerGroupID: request.WorkerGroupID,
	})
	if err != nil ||
		(worker.State != db.WorkerInstanceStateActive && worker.State != db.WorkerInstanceStateDraining) ||
		!worker.CurrentEpoch.Valid ||
		worker.CurrentEpoch.Int64 != request.WorkerEpoch ||
		!worker.SupportsRun || worker.ProtocolVersion != request.WorkerProtocolVersion ||
		!worker.RuntimeIdentityID.Valid || worker.RuntimeIdentityID.String != request.RuntimeIdentityID {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current worker epoch", err)
	}
	slot, err := q.LockRunLeaseClaimNetworkSlot(ctx, db.LockRunLeaseClaimNetworkSlotParams{
		ID: pgtype.UUID{Bytes: request.NetworkSlotID, Valid: true}, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true},
		WorkerEpoch:      request.WorkerEpoch, Generation: request.NetworkSlotGeneration,
		RuntimeInstanceID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true},
	})
	if err != nil || slot.State != db.WorkerNetworkSlotStateBound {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock bound worker network slot", err)
	}
	runtime, err := q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID: pgvalue.UUID(request.RuntimeInstanceID), OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		RegionID: request.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		WorkspaceID: locator.WorkspaceID,
	})
	if err != nil || runtime.RuntimeIdentityID != request.RuntimeIdentityID ||
		runtime.DesiredState != db.RuntimeDesiredStateReady || runtime.ObservedState != db.RuntimeObservedStateReady ||
		runtime.ObservedDesiredVersion != runtime.DesiredVersion || runtime.TerminalAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock ready runtime", err)
	}
	leaseState, err := q.LockTokenWaitRunLease(ctx, db.LockTokenWaitRunLeaseParams{
		ID:                    pgvalue.UUID(request.CurrentRunLeaseID),
		RunID:                 pgvalue.UUID(request.RunID),
		AttemptNumber:         request.AttemptNumber,
		WorkspaceID:           locator.WorkspaceID,
		LeaseSequence:         request.LeaseSequence,
		WorkerGroupID:         request.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(request.WorkerInstanceID),
		WorkerEpoch:           request.WorkerEpoch,
		RuntimeInstanceID:     pgvalue.UUID(request.RuntimeInstanceID),
		NetworkSlotID:         pgvalue.UUID(request.NetworkSlotID),
		NetworkSlotGeneration: request.NetworkSlotGeneration,
		RuntimeIdentityID:     request.RuntimeIdentityID,
		WorkerProtocolVersion: request.WorkerProtocolVersion,
		RegionID:              request.RegionID,
	})
	if err != nil || db.RunLeaseState(leaseState) != db.RunLeaseStateRunning {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired Run Lease", err)
	}
	mount, err := q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID: pgvalue.UUID(request.WorkspaceMountID), OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		RegionID: request.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		RuntimeInstanceID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true},
		WorkspaceID:       locator.WorkspaceID,
	})
	if err != nil || mount.State != db.WorkspaceMountStateMounted || mount.FencingGeneration != request.MountFencingGeneration {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock mounted Workspace", err)
	}
	workspaceLease, err := q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID: pgvalue.UUID(request.WorkspaceLeaseID), OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		RegionID: request.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		RuntimeInstanceID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true},
		WorkspaceID:       locator.WorkspaceID,
		WorkspaceMountID:  pgtype.UUID{Bytes: request.WorkspaceMountID, Valid: true},
	})
	if err != nil || !workspaceLease.OwnerRunLeaseID.Valid ||
		uuid.UUID(workspaceLease.OwnerRunLeaseID.Bytes) != request.CurrentRunLeaseID ||
		workspaceLease.OwnershipGeneration != request.OwnershipGeneration ||
		workspaceLease.WriterGeneration != request.WriterGeneration ||
		workspaceLease.MountFencingGeneration != request.MountFencingGeneration {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired Workspace Lease", err)
	}
	if err := lockOuterTokenWait(ctx, q, request.RunID); err != nil {
		return WaitRegistrationResult{}, err
	}

	registered, err := q.RegisterTokenWait(ctx, db.RegisterTokenWaitParams{
		WaitID:                        pgvalue.UUID(request.WaitID),
		EnvironmentID:                 pgvalue.UUID(request.EnvironmentID),
		TimeoutAt:                     request.TimeoutAt,
		IdleTimeoutMs:                 request.IdleTimeoutMS,
		TokenID:                       pgvalue.UUID(request.TokenID),
		ExpectedRunningStateVersion:   request.ExpectedRunStateVersion,
		RequestFingerprint:            request.RequestFingerprint,
		AttemptNumber:                 request.AttemptNumber,
		ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
		CurrentRunLeaseID:             pgvalue.UUID(request.CurrentRunLeaseID),
		CheckpointDueAt:               request.CheckpointDueAt,
		ResumeAttachID:                pgvalue.UUID(request.ResumeAttachID),
		Metadata:                      metadata,
		Tags:                          tags,
		RunID:                         pgvalue.UUID(request.RunID),
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("insert Token Wait", err)
	}
	waitingVersion := registered.ExpectedRunStateVersion

	condition, err := q.LockTokenWaitCondition(ctx, db.LockTokenWaitConditionParams{
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		TokenID:       pgvalue.UUID(request.TokenID),
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock Token registration condition", err)
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
			id: request.WaitID, runID: request.RunID,
			workspaceID: pgvalue.MustUUIDValue(locator.WorkspaceID), kind: db.WaitKindToken,
			conditionState: db.WaitStatePending, suspensionState: db.RunWaitStateHot,
			expectedRunStateVersion: waitingVersion, attemptNumber: request.AttemptNumber,
			currentRunLeaseID: pgtype.UUID{Bytes: request.CurrentRunLeaseID, Valid: true},
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
		return WaitRegistrationResult{}, fmt.Errorf("commit Token Wait registration: %w", err)
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
			CurrentRunLeaseID:             pgvalue.UUID(request.CurrentRunLeaseID),
			WorkspaceLeaseID:              pgvalue.UUID(request.WorkspaceLeaseID),
			WaitID:                        pgvalue.UUID(request.WaitID),
			EnvironmentID:                 pgvalue.UUID(request.EnvironmentID),
			RunID:                         pgvalue.UUID(request.RunID),
			TokenID:                       pgvalue.UUID(request.TokenID),
			ResumeAttachID:                pgvalue.UUID(request.ResumeAttachID),
			AttemptNumber:                 request.AttemptNumber,
			RequestFingerprint:            request.RequestFingerprint,
			Metadata:                      metadata,
			Tags:                          tags,
			LeaseSequence:                 request.LeaseSequence,
			WorkerGroupID:                 request.WorkerGroupID,
			WorkerInstanceID:              pgvalue.UUID(request.WorkerInstanceID),
			WorkerEpoch:                   request.WorkerEpoch,
			WorkerProtocolVersion:         request.WorkerProtocolVersion,
			RuntimeInstanceID:             pgvalue.UUID(request.RuntimeInstanceID),
			RuntimeIdentityID:             request.RuntimeIdentityID,
			WorkspaceMountID:              pgvalue.UUID(request.WorkspaceMountID),
			OwnershipGeneration:           request.OwnershipGeneration,
			WriterGeneration:              request.WriterGeneration,
			MountFencingGeneration:        request.MountFencingGeneration,
			NetworkSlotID:                 pgvalue.UUID(request.NetworkSlotID),
			NetworkSlotGeneration:         request.NetworkSlotGeneration,
			RegionID:                      request.RegionID,
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
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("load Token Wait registration replay", err)
	}
	conflicting, err := q.TokenWaitExists(ctx, pgvalue.UUID(request.WaitID))
	if err != nil {
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("check Token Wait registration replay conflict", err)
	}
	if conflicting {
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("Token Wait registration replay does not match", nil)
	}
	return WaitRegistrationResult{}, false, nil
}

func (r *WaitReconciler) ReconcileBatch(
	ctx context.Context,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	limit int32,
) (WaitBatch, error) {
	if environmentID == uuid.Nil || tokenID == uuid.Nil {
		return WaitBatch{}, errors.New("Token Wait reconciliation IDs are required")
	}
	if limit <= 0 {
		return WaitBatch{}, errors.New("Token Wait reconciliation limit must be positive")
	}
	if limit > maxWaitBatch {
		return WaitBatch{}, fmt.Errorf(
			"Token Wait reconciliation limit must not exceed %d",
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
		return WaitBatch{}, fmt.Errorf("discover pending Token Waits: %w", err)
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
			"Token Wait timeout reconciliation limit must not exceed %d",
			maxWaitBatch,
		)
	}
	candidates, err := r.queries.ListTimedOutTokenWaitCandidates(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("discover timed out Token Waits: %w", err)
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
) error {
	workspace, err := q.LockTokenWaitWorkspace(ctx, db.LockTokenWaitWorkspaceParams{
		WorkspaceID:   locator.WorkspaceID,
		EnvironmentID: pgvalue.UUID(environmentID),
	})
	if err != nil {
		return tokenWaitAuthorityError("lock Run Workspace", err)
	}
	if db.WorkspaceState(workspace.State) != db.WorkspaceStateActive ||
		db.WorkspaceDesiredState(workspace.DesiredState) != db.WorkspaceDesiredStateActive ||
		workspace.OwnerActorID != locator.OwnerActorID {
		return tokenWaitAuthorityError("Workspace ownership changed", nil)
	}
	if workspace.OwnerActorID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return tokenWaitAuthorityError("Actor current Run is outside the locked lineage", nil)
		}
	} else if !workspace.OwnerRunID.Valid ||
		!tokenWaitLineageContains(lineage, workspace.OwnerRunID) {
		return tokenWaitAuthorityError("Workspace owner Run is outside the locked lineage", nil)
	}
	return nil
}

func validateTokenWaitActorCursor(
	cursor pgtype.Int8,
	ownerActorID pgtype.UUID,
	actorCurrentRunID pgtype.UUID,
	actorCommittedInputSequence int64,
	actorNextInputSequence int64,
	run tokenWaitLockedRun,
	attemptEntrypointKind string,
	attemptActorStartInputSequence pgtype.Int8,
) error {
	switch run.entrypointKind {
	case "task":
		if run.actorID.Valid || ownerActorID.Valid || cursor.Valid || attemptEntrypointKind != "task" ||
			attemptActorStartInputSequence.Valid {
			return tokenWaitAuthorityError("Task Token Wait carries Actor authority", nil)
		}
	case "actor":
		if !run.actorID.Valid || run.actorID != ownerActorID || !actorCurrentRunID.Valid ||
			uuid.UUID(actorCurrentRunID.Bytes) != run.id || attemptEntrypointKind != "actor" ||
			!attemptActorStartInputSequence.Valid || !cursor.Valid ||
			attemptActorStartInputSequence.Int64 > actorCommittedInputSequence ||
			cursor.Int64 < actorCommittedInputSequence ||
			cursor.Int64 > actorCommittedInputSequence+1 ||
			cursor.Int64 >= actorNextInputSequence {
			return tokenWaitAuthorityError("Actor Token Wait cursor authority does not match", nil)
		}
	default:
		return tokenWaitAuthorityError("Token Wait entrypoint kind is invalid", nil)
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
		return false, false, fmt.Errorf("begin Token Wait reconciliation: %w", err)
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
			return false, false, fmt.Errorf("commit stale Token Wait reconciliation: %w", err)
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}

	var lockedActorCurrentRunID pgtype.UUID
	if locator.OwnerActorID.Valid {
		actor, err := q.LockTokenWaitActor(ctx, locator.OwnerActorID)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock owning Actor", err)
		}
		if actor.State != "open" && actor.State != "closing" {
			return false, false, tokenWaitAuthorityError("owning Actor is not active", nil)
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
		return false, false, tokenWaitAuthorityError("Run locator changed", nil)
	}
	workspace, err := q.LockTokenWaitWorkspace(ctx, db.LockTokenWaitWorkspaceParams{
		WorkspaceID:   locator.WorkspaceID,
		EnvironmentID: pgvalue.UUID(environmentID),
	})
	if err != nil {
		return false, false, tokenWaitAuthorityError("lock Run Workspace", err)
	}
	if db.WorkspaceState(workspace.State) != db.WorkspaceStateActive ||
		db.WorkspaceDesiredState(workspace.DesiredState) != db.WorkspaceDesiredStateActive ||
		workspace.OwnerActorID != locator.OwnerActorID {
		return false, false, tokenWaitAuthorityError("Workspace ownership changed", nil)
	}
	if workspace.OwnerActorID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return false, false, tokenWaitAuthorityError("Actor current Run is outside the locked lineage", nil)
		}
	} else if !workspace.OwnerRunID.Valid ||
		!tokenWaitLineageContains(lineage, workspace.OwnerRunID) {
		return false, false, tokenWaitAuthorityError("Workspace owner Run is outside the locked lineage", nil)
	}

	attempt, err := q.LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID:         locator.RunID,
		AttemptNumber: locator.AttemptNumber,
		WorkspaceID:   locator.WorkspaceID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return false, false, tokenWaitAuthorityError("lock current Run Attempt", err)
	}

	if err := lockOuterTokenWait(ctx, q, runID); err != nil {
		return false, false, err
	}
	wait, err := lockCurrentTokenWait(ctx, q, environmentID, tokenID, locator)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit converged Token Wait reconciliation: %w", err)
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
			return false, false, fmt.Errorf("commit deferred Token Wait reconciliation: %w", err)
		}
		return false, true, nil
	}

	var resolution tokenWaitResolution
	if timeout {
		if !wait.timeoutAt.Valid || !wait.timedOut {
			if err := tx.Commit(ctx); err != nil {
				return false, false, fmt.Errorf("commit early Token Wait timeout reconciliation: %w", err)
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
			return false, false, tokenWaitAuthorityError("lock terminal Token", err)
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
		err = tokenWaitAuthorityError("pending Token Wait has an ineligible suspension state", nil)
	}
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("commit Token Wait reconciliation: %w", err)
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
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("lock Run lineage", err)
	}
	lineage := make([]tokenWaitLockedRun, 0, len(rows))
	var addressed tokenWaitLockedRun
	foundAddressed := false
	for _, locked := range rows {
		row := tokenWaitLockedRun{
			id:                pgvalue.MustUUIDValue(locked.ID),
			parentRunID:       locked.ParentRunID,
			workspaceID:       pgvalue.MustUUIDValue(locked.WorkspaceID),
			actorID:           locked.ActorID,
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
			return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("Run lineage contains a cycle", nil)
		}
		lineage = append(lineage, row)
		if row.depth == 0 && row.id == runID {
			addressed = row
			foundAddressed = true
		}
	}
	if !foundAddressed {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("addressed Run does not exist", nil)
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
		return tokenWaitAuthorityError("lock enclosing Run Wait", err)
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
		return tokenWaitAuthorityError("Run and Token Wait fences do not match", nil)
	}
	switch wait.suspensionState {
	case db.RunWaitStateHot, db.RunWaitStateCheckpointing:
		if wait.conditionState != db.WaitStatePending && wait.suspensionState != db.RunWaitStateCheckpointing {
			return tokenWaitAuthorityError("terminal Token Wait is not awaiting checkpoint readiness", nil)
		}
		if !run.currentRunLeaseID.Valid || !wait.currentRunLeaseID.Valid ||
			run.currentRunLeaseID != wait.currentRunLeaseID || wait.priorRunLeaseID.Valid ||
			!run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("hot Token Wait Lease fence does not match", nil)
		}
	case db.RunWaitStateParked:
		if wait.conditionState != db.WaitStatePending {
			return tokenWaitAuthorityError("parked Token Wait is already terminal", nil)
		}
		if run.currentRunLeaseID.Valid || wait.currentRunLeaseID.Valid ||
			!wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("parked Token Wait provenance does not match", nil)
		}
	default:
		return tokenWaitAuthorityError("pending Token Wait suspension is not completable", nil)
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
		return tokenWaitResolution{}, tokenWaitAuthorityError("Token is not terminal", nil)
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
		return tokenWaitAuthorityError("resolve hot Token Wait", err)
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
		return tokenWaitAuthorityError("complete checkpointing Token Wait", err)
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
		OutboxMessageID:         pgvalue.NewUUIDv7(),
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
		return tokenWaitAuthorityError("resolve parked Token Wait", err)
	}
	return nil
}

func tokenWaitAuthorityError(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrWaitAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrWaitAuthority, operation, cause)
}
