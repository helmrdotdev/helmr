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

	lineage, run, err := lockTokenWaitLineage(ctx, tx, request.EnvironmentID, request.RunID)
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
	if err := lockOuterTokenWait(ctx, tx, request.RunID); err != nil {
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
			id: request.WaitID, environmentID: request.EnvironmentID, runID: request.RunID,
			workspaceID: pgvalue.MustUUIDValue(locator.WorkspaceID),
			tokenID:     request.TokenID, kind: db.WaitKindToken,
			conditionState: db.WaitStatePending, suspensionState: db.RunWaitStateHot,
			expectedRunStateVersion: waitingVersion, attemptNumber: request.AttemptNumber,
			currentRunLeaseID: pgtype.UUID{Bytes: request.CurrentRunLeaseID, Valid: true},
		}
		run.stateVersion = waitingVersion
		run.status = db.RunStatusWaiting
		if err := reconcileHotTokenWait(ctx, tx, run, wait, resolution); err != nil {
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

type tokenWaitLocator struct {
	waitID       uuid.UUID
	runID        uuid.UUID
	workspaceID  uuid.UUID
	attempt      int32
	ownerActorID pgtype.UUID
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
	environmentID           uuid.UUID
	runID                   uuid.UUID
	workspaceID             uuid.UUID
	tokenID                 uuid.UUID
	kind                    db.WaitKind
	conditionState          db.WaitState
	suspensionState         db.RunWaitState
	expectedRunStateVersion int64
	attemptNumber           int32
	currentRunLeaseID       pgtype.UUID
	priorRunLeaseID         pgtype.UUID
	suspendCheckpointID     pgtype.UUID
	resumeRequestVersion    int64
	timeoutAt               pgtype.Timestamptz
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

	locator, err := loadTokenWaitLocator(
		ctx,
		tx,
		environmentID,
		tokenID,
		waitID,
		runID,
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
	if locator.ownerActorID.Valid {
		var actorState string
		err = tx.QueryRow(ctx, `
SELECT state, current_run_id
  FROM actors
 WHERE id = $1
 FOR UPDATE`, locator.ownerActorID).Scan(&actorState, &lockedActorCurrentRunID)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock owning Actor", err)
		}
		if actorState != "open" && actorState != "closing" {
			return false, false, tokenWaitAuthorityError("owning Actor is not active", nil)
		}
	}

	lineage, addressedRun, err := lockTokenWaitLineage(ctx, tx, environmentID, locator.runID)
	if err != nil {
		return false, false, err
	}
	if addressedRun.workspaceID != locator.workspaceID || addressedRun.currentAttempt != locator.attempt {
		return false, false, tokenWaitAuthorityError("Run locator changed", nil)
	}
	var ownerActorID, ownerRunID pgtype.UUID
	var workspaceState db.WorkspaceState
	var desiredState db.WorkspaceDesiredState
	err = tx.QueryRow(ctx, `
SELECT owner_actor_id, owner_run_id, state, desired_state
  FROM workspaces
 WHERE id = $1
   AND environment_id = $2
 FOR UPDATE`, locator.workspaceID, environmentID).Scan(
		&ownerActorID,
		&ownerRunID,
		&workspaceState,
		&desiredState,
	)
	if err != nil {
		return false, false, tokenWaitAuthorityError("lock Run Workspace", err)
	}
	if workspaceState != db.WorkspaceStateActive || desiredState != db.WorkspaceDesiredStateActive ||
		ownerActorID != locator.ownerActorID {
		return false, false, tokenWaitAuthorityError("Workspace ownership changed", nil)
	}
	if ownerActorID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return false, false, tokenWaitAuthorityError("Actor current Run is outside the locked lineage", nil)
		}
	} else if !ownerRunID.Valid || !tokenWaitLineageContains(lineage, ownerRunID) {
		return false, false, tokenWaitAuthorityError("Workspace owner Run is outside the locked lineage", nil)
	}

	var attemptTerminalAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
SELECT terminal_at
  FROM run_attempts
 WHERE run_id = $1
   AND number = $2
   AND workspace_id = $3
 FOR UPDATE`, locator.runID, locator.attempt, locator.workspaceID).Scan(&attemptTerminalAt)
	if err != nil || attemptTerminalAt.Valid {
		return false, false, tokenWaitAuthorityError("lock current Run Attempt", err)
	}

	if err := lockOuterTokenWait(ctx, tx, locator.runID); err != nil {
		return false, false, err
	}
	wait, err := lockCurrentTokenWait(ctx, tx, environmentID, tokenID, locator)
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
		var now pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
			return false, false, tokenWaitAuthorityError("load Token Wait timeout time", err)
		}
		if !wait.timeoutAt.Valid || !now.Valid || now.Time.Before(wait.timeoutAt.Time) {
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
		var tokenState db.TokenState
		var completionData []byte
		err = tx.QueryRow(ctx, `
SELECT state, result
  FROM tokens
 WHERE environment_id = $1
   AND id = $2
 FOR UPDATE`, environmentID, tokenID).Scan(&tokenState, &completionData)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock terminal Token", err)
		}
		resolution, err = tokenWaitTerminalResolution(tokenState, completionData)
		if err != nil {
			return false, false, err
		}
	}

	switch wait.suspensionState {
	case db.RunWaitStateHot:
		err = reconcileHotTokenWait(ctx, tx, addressedRun, wait, resolution)
	case db.RunWaitStateCheckpointing:
		err = reconcileCheckpointingTokenWait(ctx, tx, wait, resolution)
	case db.RunWaitStateParked:
		err = reconcileParkedTokenWait(ctx, tx, addressedRun, wait, resolution)
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

func loadTokenWaitLocator(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	waitID uuid.UUID,
	runID uuid.UUID,
) (tokenWaitLocator, error) {
	var locator tokenWaitLocator
	err := tx.QueryRow(ctx, `
SELECT run_waits.id,
       run_waits.run_id,
       run_waits.workspace_id,
       run_waits.attempt_number,
       workspaces.owner_actor_id
  FROM run_waits
  JOIN runs
    ON runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
  JOIN workspaces
    ON workspaces.id = run_waits.workspace_id
 WHERE run_waits.id = $1
   AND run_waits.environment_id = $2
   AND run_waits.run_id = $3
   AND run_waits.token_id = $4
   AND run_waits.kind = 'token'
   AND (run_waits.condition_state = 'pending'
        OR run_waits.suspension_state = 'checkpointing')`,
		waitID,
		environmentID,
		runID,
		tokenID,
	).Scan(
		&locator.waitID,
		&locator.runID,
		&locator.workspaceID,
		&locator.attempt,
		&locator.ownerActorID,
	)
	if err != nil {
		return tokenWaitLocator{}, err
	}
	return locator, nil
}

func lockTokenWaitLineage(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	runID uuid.UUID,
) ([]tokenWaitLockedRun, tokenWaitLockedRun, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE lineage AS (
    SELECT id, parent_run_id, 0::integer AS depth, ARRAY[id] AS path, false AS cycle
      FROM runs
     WHERE environment_id = $1
       AND id = $2
    UNION ALL
    SELECT parent.id,
           parent.parent_run_id,
           child.depth + 1,
           child.path || parent.id,
           parent.id = ANY(child.path)
      FROM lineage AS child
      JOIN runs AS parent
        ON parent.environment_id = $1
       AND parent.id = child.parent_run_id
     WHERE NOT child.cycle
)
SELECT runs.id,
       runs.parent_run_id,
       runs.workspace_id,
       runs.actor_id,
	   runs.entrypoint_kind,
       runs.status,
       runs.state_version,
	       runs.current_attempt_number,
	       runs.current_run_lease_id,
	       runs.active_started_at,
	       lineage.depth,
       lineage.cycle
  FROM lineage
  JOIN runs ON runs.id = lineage.id
 ORDER BY lineage.depth DESC, runs.id
 FOR UPDATE OF runs`, environmentID, runID)
	if err != nil {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("lock Run lineage", err)
	}
	defer rows.Close()
	var lineage []tokenWaitLockedRun
	var addressed tokenWaitLockedRun
	foundAddressed := false
	for rows.Next() {
		var row tokenWaitLockedRun
		if err := rows.Scan(
			&row.id,
			&row.parentRunID,
			&row.workspaceID,
			&row.actorID,
			&row.entrypointKind,
			&row.status,
			&row.stateVersion,
			&row.currentAttempt,
			&row.currentRunLeaseID,
			&row.activeStartedAt,
			&row.depth,
			&row.cycle,
		); err != nil {
			return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("scan locked Run lineage", err)
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
	if err := rows.Err(); err != nil {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("iterate locked Run lineage", err)
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

func lockOuterTokenWait(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	rows, err := tx.Query(ctx, `
SELECT id
  FROM run_waits
 WHERE child_run_id = $1
   AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming')
 ORDER BY id
 FOR UPDATE`, runID)
	if err != nil {
		return tokenWaitAuthorityError("lock enclosing Run Wait", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return tokenWaitAuthorityError("scan enclosing Run Wait", err)
		}
	}
	if err := rows.Err(); err != nil {
		return tokenWaitAuthorityError("iterate enclosing Run Wait", err)
	}
	return nil
}

func lockCurrentTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	locator tokenWaitLocator,
) (tokenWaitLockedWait, error) {
	var wait tokenWaitLockedWait
	err := tx.QueryRow(ctx, `
SELECT id,
       environment_id,
       run_id,
       workspace_id,
       token_id,
       kind,
       condition_state,
       suspension_state,
       expected_run_state_version,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       resume_request_version,
       timeout_at
  FROM run_waits
 WHERE id = $1
   AND environment_id = $2
   AND run_id = $3
   AND workspace_id = $4
   AND attempt_number = $5
   AND token_id = $6
   AND kind = 'token'
   AND (condition_state = 'pending' OR suspension_state = 'checkpointing')
 FOR UPDATE`,
		locator.waitID,
		environmentID,
		locator.runID,
		locator.workspaceID,
		locator.attempt,
		tokenID,
	).Scan(
		&wait.id,
		&wait.environmentID,
		&wait.runID,
		&wait.workspaceID,
		&wait.tokenID,
		&wait.kind,
		&wait.conditionState,
		&wait.suspensionState,
		&wait.expectedRunStateVersion,
		&wait.attemptNumber,
		&wait.currentRunLeaseID,
		&wait.priorRunLeaseID,
		&wait.suspendCheckpointID,
		&wait.resumeRequestVersion,
		&wait.timeoutAt,
	)
	if err != nil {
		return tokenWaitLockedWait{}, err
	}
	return wait, nil
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
	tx pgx.Tx,
	run tokenWaitLockedRun,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	var nextVersion int64
	err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'running',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id = $4
RETURNING state_version`,
		run.id,
		run.stateVersion,
		run.currentAttempt,
		run.currentRunLeaseID,
	).Scan(&nextVersion)
	if err != nil {
		return tokenWaitAuthorityError("resume hot Token Wait Run", err)
	}
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = $1,
       condition_result = $2::jsonb,
       condition_reason_code = $3,
       condition_error = $4::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released',
       expected_run_state_version = $5,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $6
   AND run_id = $7
   AND condition_state = 'pending'
   AND suspension_state = 'hot'
   AND expected_run_state_version = $8
   AND current_run_lease_id = $9`,
		resolution.conditionState,
		resolution.result,
		resolution.reasonCode,
		resolution.conditionError,
		nextVersion,
		wait.id,
		wait.runID,
		wait.expectedRunStateVersion,
		wait.currentRunLeaseID,
	)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("release hot Token Wait", err)
	}
	return nil
}

func reconcileCheckpointingTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = $1,
       condition_result = $2::jsonb,
       condition_reason_code = $3,
       condition_error = $4::jsonb,
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $5
   AND run_id = $6
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
   AND expected_run_state_version = $7
   AND current_run_lease_id = $8`,
		resolution.conditionState,
		resolution.result,
		resolution.reasonCode,
		resolution.conditionError,
		wait.id,
		wait.runID,
		wait.expectedRunStateVersion,
		wait.currentRunLeaseID,
	)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("complete checkpointing Token Wait", err)
	}
	return nil
}

func reconcileParkedTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	run tokenWaitLockedRun,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	nextResumeVersion := wait.resumeRequestVersion + 1
	var nextVersion int64
	err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'queued',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id IS NULL
RETURNING state_version`, run.id, run.stateVersion, run.currentAttempt).Scan(&nextVersion)
	if err != nil {
		return tokenWaitAuthorityError("queue parked Token Wait Run", err)
	}
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = $1,
       condition_result = $2::jsonb,
       condition_reason_code = $3,
       condition_error = $4::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending',
       resume_request_version = $5,
       expected_run_state_version = $6,
       updated_at = transaction_timestamp()
 WHERE id = $7
   AND run_id = $8
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
   AND expected_run_state_version = $9
   AND current_run_lease_id IS NULL
   AND prior_run_lease_id = $10
   AND suspend_checkpoint_id = $11`,
		resolution.conditionState,
		resolution.result,
		resolution.reasonCode,
		resolution.conditionError,
		nextResumeVersion,
		nextVersion,
		wait.id,
		wait.runID,
		wait.expectedRunStateVersion,
		wait.priorRunLeaseID,
		wait.suspendCheckpointID,
	)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("resume parked Token Wait", err)
	}
	command, err = tx.Exec(ctx, `
INSERT INTO outbox_messages (
    id,
    lane,
    topic,
    partition_key,
    payload,
    available_at
) VALUES (
    $1,
    'control',
    'run.resume',
    $2::uuid::text,
    jsonb_build_object(
        'environmentId', $3::uuid::text,
        'runId', $4::uuid::text,
        'runWaitId', $5::uuid::text,
        'resumeRequestVersion', $6::bigint
    ),
    transaction_timestamp()
)`, uuid.Must(uuid.NewV7()), wait.workspaceID, wait.environmentID, wait.runID, wait.id, nextResumeVersion)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("publish parked Token Wait resume intent", err)
	}
	return nil
}

func tokenWaitAuthorityError(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrWaitAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrWaitAuthority, operation, cause)
}
