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
	TurnID                        pgtype.UUID
	RunGeneration                 pgtype.Int8
	TokenID                       uuid.UUID
	WaitID                        uuid.UUID
	RunLeaseID                    uuid.UUID
	LeaseSequence                 int64
	WorkerGroupID                 uuid.UUID
	WorkerHostID                  uuid.UUID
	WorkerEpoch                   int64
	RequestFingerprint            string
	ActorSpeculativeInputSequence pgtype.Int8
	TimeoutAt                     pgtype.Timestamptz
	IdleTimeoutMS                 pgtype.Int8
	Metadata                      json.RawMessage
	Tags                          []string
}

type WaitRegistrationResult struct {
	WaitID           uuid.UUID
	RunRevision      int64
	ConditionStatus  db.WaitStatus
	SuspensionStatus db.RunWaitStatus
	Result           json.RawMessage
	ReasonCode       string
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
		request.RunLeaseID == uuid.Nil() ||
		request.WorkerHostID == uuid.Nil() {
		return WaitRegistrationResult{}, errors.New("token wait registration IDs are required")
	}
	if request.LeaseSequence <= 0 || request.WorkerEpoch <= 0 || request.WorkerGroupID == uuid.Nil() ||
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
	// path repeats it after locking the Run.
	if !request.TurnID.Valid {
		if replay, found, err := replayTokenWaitRegistration(ctx, q, request, metadata, tags); err != nil {
			return WaitRegistrationResult{}, err
		} else if found {
			if err := tx.Commit(ctx); err != nil {
				return WaitRegistrationResult{}, fmt.Errorf("commit token wait registration replay: %w", err)
			}
			return replay, nil
		}
	}
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(request.RunLeaseID), LeaseSequence: request.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(request.WorkerGroupID), WorkerHostID: pgvalue.UUID(request.WorkerHostID),
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
	workerGroup, err := q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: pgvalue.UUID(request.WorkerGroupID), RegionID: locators.RegionID,
	})
	if err != nil ||
		(workerGroup.Status != db.WorkerGroupStatusActive && workerGroup.Status != db.WorkerGroupStatusDraining) {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock active worker group", err)
	}
	worker, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgtype.UUID{Bytes: request.WorkerHostID, Valid: true}, WorkerGroupID: pgvalue.UUID(request.WorkerGroupID),
	})
	if err != nil ||
		(worker.Status != db.WorkerHostStatusActive && worker.Status != db.WorkerHostStatusDraining) ||
		!worker.CurrentEpoch.Valid ||
		worker.CurrentEpoch.Int64 != request.WorkerEpoch ||
		!worker.VMPlatformID.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current worker epoch", err)
	}
	computer, err := q.LockTokenWaitComputer(ctx, db.LockTokenWaitComputerParams{
		ComputerID: locator.ComputerID, EnvironmentID: locators.EnvironmentID,
	})
	if err != nil || computer.Status != db.ComputerStatusActive || computer.DesiredState != db.ComputerDesiredStateActive {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock active Computer", err)
	}
	runtime, err := q.LockRunLeaseClaimInstance(ctx, db.LockRunLeaseClaimInstanceParams{
		ID: locators.ComputerInstanceID, OrgID: locator.OrgID,
		ProjectID: locator.ProjectID, EnvironmentID: locators.EnvironmentID,
		RegionID: locators.RegionID, WorkerGroupID: pgvalue.UUID(request.WorkerGroupID),
		WorkerHostID: pgtype.UUID{Bytes: request.WorkerHostID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		ComputerID: locator.ComputerID,
	})
	if err != nil || runtime.VMPlatformID != worker.VMPlatformID.String ||
		runtime.DesiredState != db.RuntimeDesiredStateReady || runtime.ObservedState != db.RuntimeObservedStateReady ||
		runtime.ObservedDesiredVersion != runtime.DesiredVersion || runtime.TerminalAt.Valid ||
		runtime.ReclaimedAt.Valid || runtime.MountState != "mounted" || runtime.WriterGeneration != computer.WriterGeneration ||
		runtime.WriterGeneration != locators.WriterGeneration || (runtime.AdmissionState != "open" && runtime.AdmissionState != "draining") {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock ready runtime", err)
	}

	var lockedActorCurrentRunID pgtype.UUID
	var lockedActor db.Session
	var lockedActorCommittedInputSequence, lockedActorNextInputSequence int64
	if locator.SessionID.Valid {
		actor, err := q.LockTokenWaitActor(ctx, locator.SessionID)
		if err != nil {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("lock owning actor", err)
		}
		if actor.Status != "open" && actor.Status != "closing" {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("owning actor is not active", nil)
		}
		lockedActor = actor
		lockedActorCurrentRunID = actor.CurrentRunID
		lockedActorCommittedInputSequence = actor.CommittedInputSequence
		lockedActorNextInputSequence = actor.NextInputSequence
	}

	run, err := lockTokenWaitRun(ctx, q, environmentID, runID)
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
	if pgvalue.UUID(run.computerID) != locator.ComputerID || run.status != db.RunStatusRunning ||
		run.currentAttempt != attemptNumber ||
		!run.currentRunLeaseID.Valid || uuid.UUID(run.currentRunLeaseID.Bytes) != request.RunLeaseID ||
		!run.activeStartedAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("run registration fence does not match", nil)
	}
	attempt, err := q.LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID:         locators.RunID,
		AttemptNumber: attemptNumber,
		ComputerID:    locator.ComputerID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current run attempt", err)
	}
	if err := validateTokenWaitActorCursor(
		request.ActorSpeculativeInputSequence, locator.SessionID, lockedActorCurrentRunID,
		lockedActorCommittedInputSequence, lockedActorNextInputSequence,
		run, attempt.EntrypointKind, attempt.SessionInputStartSequence,
	); err != nil {
		return WaitRegistrationResult{}, err
	}
	if run.entrypointKind == "actor" {
		want := lockedActorCommittedInputSequence
		if request.TurnID.Valid {
			want++
		}
		if request.ActorSpeculativeInputSequence.Int64 != want {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("Token wait cursor does not identify its admitted Turn", nil)
		}
	}
	leaseStatus, err := q.LockTokenWaitRunLease(ctx, db.LockTokenWaitRunLeaseParams{
		ID:                 pgvalue.UUID(request.RunLeaseID),
		RunID:              locators.RunID,
		AttemptNumber:      attemptNumber,
		ComputerID:         locator.ComputerID,
		LeaseSequence:      request.LeaseSequence,
		WorkerGroupID:      pgvalue.UUID(request.WorkerGroupID),
		WorkerHostID:       pgvalue.UUID(request.WorkerHostID),
		WorkerEpoch:        request.WorkerEpoch,
		ComputerInstanceID: locators.ComputerInstanceID,
		VMPlatformID:       runtime.VMPlatformID,
		RegionID:           locators.RegionID,
	})
	if err != nil || db.RunLeaseStatus(leaseStatus) != db.RunLeaseStatusRunning {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired run lease", err)
	}

	registered, err := q.RegisterTokenWait(ctx, db.RegisterTokenWaitParams{
		WaitID:                  pgvalue.UUID(request.WaitID),
		EnvironmentID:           locators.EnvironmentID,
		TimeoutAt:               request.TimeoutAt,
		IdleTimeoutMs:           request.IdleTimeoutMS,
		TokenID:                 pgvalue.UUID(request.TokenID),
		ExpectedRunningRevision: run.revision,
		RequestFingerprint:      request.RequestFingerprint,
		AttemptNumber:           attemptNumber,
		CurrentRunLeaseID:       pgvalue.UUID(request.RunLeaseID),
		Metadata:                metadata,
		Tags:                    tags,
		RunID:                   locators.RunID,
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("insert token wait", err)
	}
	if locators.SessionID.Valid {
		if lockedActor.DispatchHoldID.Valid || lockedActor.ActiveTurnID != request.TurnID || lockedActor.RunGeneration != request.RunGeneration.Int64 && request.TurnID.Valid {
			return WaitRegistrationResult{}, ErrWaitAuthority
		}
		if request.TurnID.Valid {
			_, err = q.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: lockedActor.ID, TurnID: request.TurnID, RunGeneration: request.RunGeneration, WaitID: registered.ID})
			if err != nil {
				return WaitRegistrationResult{}, tokenWaitAuthorityError("bind Token wait to active Turn", err)
			}
		} else if request.RunGeneration.Valid {
			return WaitRegistrationResult{}, ErrWaitAuthority
		}
	} else if request.TurnID.Valid || request.RunGeneration.Valid {
		return WaitRegistrationResult{}, ErrWaitAuthority
	}
	waitingRevision := registered.ExpectedRunRevision

	condition, err := q.LockTokenWaitCondition(ctx, db.LockTokenWaitConditionParams{
		EnvironmentID: locators.EnvironmentID,
		TokenID:       pgvalue.UUID(request.TokenID),
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitAuthorityError("lock token registration condition", err)
	}
	tokenStatus := db.TokenStatus(condition.Status)

	result := WaitRegistrationResult{
		WaitID: request.WaitID, RunRevision: waitingRevision,
		ConditionStatus: db.WaitStatusPending, SuspensionStatus: db.RunWaitStatusHot,
	}
	if tokenStatus != db.TokenStatusPending {
		resolution, err := tokenWaitTerminalResolution(tokenStatus, condition.Result)
		if err != nil {
			return WaitRegistrationResult{}, err
		}
		wait := tokenWaitLockedWait{
			id: request.WaitID, runID: runID,
			computerID: pgvalue.MustUUIDValue(locator.ComputerID), kind: db.WaitKindToken,
			conditionStatus: db.WaitStatusPending, suspensionStatus: db.RunWaitStatusHot,
			expectedRunRevision: waitingRevision, attemptNumber: attemptNumber,
			currentRunLeaseID: pgtype.UUID{Bytes: request.RunLeaseID, Valid: true},
		}
		run.revision = waitingRevision
		run.status = db.RunStatusWaiting
		if err := reconcileHotTokenWait(ctx, q, run, wait, resolution); err != nil {
			return WaitRegistrationResult{}, err
		}
		result.RunRevision = waitingRevision + 1
		result.ConditionStatus = resolution.conditionStatus
		result.SuspensionStatus = db.RunWaitStatusReleased
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
			RunLeaseID: pgvalue.UUID(request.RunLeaseID),
			TurnID:     request.TurnID, TurnRunGeneration: request.RunGeneration,
			WaitID:             pgvalue.UUID(request.WaitID),
			TokenID:            pgvalue.UUID(request.TokenID),
			RequestFingerprint: request.RequestFingerprint,
			Metadata:           metadata,
			Tags:               tags,
			LeaseSequence:      request.LeaseSequence,
			WorkerGroupID:      pgvalue.UUID(request.WorkerGroupID),
			WorkerHostID:       pgvalue.UUID(request.WorkerHostID),
			WorkerEpoch:        request.WorkerEpoch,
		},
	)
	if err == nil {
		if !replay.Matches {
			return WaitRegistrationResult{}, false, tokenWaitAuthorityError("token wait registration replay does not match", nil)
		}
		result := WaitRegistrationResult{
			WaitID:           pgvalue.MustUUIDValue(replay.WaitID),
			RunRevision:      replay.RunRevision.Int64,
			ConditionStatus:  db.WaitStatus(replay.ConditionStatus.String),
			SuspensionStatus: db.RunWaitStatus(replay.SuspensionStatus.String),
			Result:           json.RawMessage(replay.ConditionResult),
		}
		if replay.ConditionReasonCode.Valid {
			result.ReasonCode = replay.ConditionReasonCode.String
		}
		return result, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return WaitRegistrationResult{}, false, tokenWaitAuthorityError("load token wait registration replay", err)
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
		if run.actorID.Valid || cursor.Valid || attemptEntrypointKind != "task" ||
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
	computerID        uuid.UUID
	actorID           pgtype.UUID
	entrypointKind    string
	status            db.RunStatus
	revision          int64
	currentAttempt    int32
	currentRunLeaseID pgtype.UUID
	activeStartedAt   pgtype.Timestamptz
}

type tokenWaitLockedWait struct {
	id                  uuid.UUID
	runID               uuid.UUID
	computerID          uuid.UUID
	kind                db.WaitKind
	conditionStatus     db.WaitStatus
	suspensionStatus    db.RunWaitStatus
	expectedRunRevision int64
	attemptNumber       int32
	currentRunLeaseID   pgtype.UUID
	priorRunLeaseID     pgtype.UUID
	suspendCheckpointID pgtype.UUID
	timeoutAt           pgtype.Timestamptz
	timedOut            bool
}

type tokenWaitResolution struct {
	conditionStatus db.WaitStatus
	result          json.RawMessage
	reasonCode      *string
	conditionError  json.RawMessage
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

	if _, err := q.LockTokenWaitComputer(ctx, db.LockTokenWaitComputerParams{
		ComputerID: locator.ComputerID, EnvironmentID: pgvalue.UUID(environmentID),
	}); err != nil {
		return false, false, tokenWaitAuthorityError("lock Computer", err)
	}
	if _, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{
		ComputerID: locator.ComputerID, EnvironmentID: pgvalue.UUID(environmentID),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, false, tokenWaitAuthorityError("lock Computer Instance", err)
	}

	var lockedActorCurrentRunID pgtype.UUID
	if locator.SessionID.Valid {
		actor, err := q.LockTokenWaitActor(ctx, locator.SessionID)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock owning actor", err)
		}
		if actor.Status != "open" && actor.Status != "closing" {
			return false, false, tokenWaitAuthorityError("owning actor is not active", nil)
		}
		lockedActorCurrentRunID = actor.CurrentRunID
	}

	addressedRun, err := lockTokenWaitRun(ctx, q, environmentID, runID)
	if err != nil {
		return false, false, err
	}
	computerID := pgvalue.MustUUIDValue(locator.ComputerID)
	if addressedRun.computerID != computerID ||
		addressedRun.currentAttempt != locator.AttemptNumber {
		return false, false, tokenWaitAuthorityError("run locator changed", nil)
	}
	if locator.SessionID.Valid && (!lockedActorCurrentRunID.Valid || lockedActorCurrentRunID != locator.RunID) {
		return false, false, tokenWaitAuthorityError("Session current Run changed", nil)
	}

	attempt, err := q.LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID:         locator.RunID,
		AttemptNumber: locator.AttemptNumber,
		ComputerID:    locator.ComputerID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return false, false, tokenWaitAuthorityError("lock current run attempt", err)
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
	current, err := q.RunWaitTurnCurrent(ctx, pgvalue.UUID(waitID))
	if err != nil {
		return false, false, err
	}
	if !current {
		return false, false, tx.Commit(ctx)
	}
	if err := validateLockedTokenWait(addressedRun, wait); err != nil {
		return false, false, err
	}
	if wait.conditionStatus != db.WaitStatusPending {
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
			conditionStatus: db.WaitStatusFailed,
			reasonCode:      &reason,
			conditionError:  json.RawMessage(`{"code":"wait_timeout","retryable":false}`),
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
			db.TokenStatus(condition.Status),
			condition.Result,
		)
		if err != nil {
			return false, false, err
		}
	}

	switch wait.suspensionStatus {
	case db.RunWaitStatusHot:
		err = reconcileHotTokenWait(ctx, q, addressedRun, wait, resolution)
	case db.RunWaitStatusCheckpointing:
		err = reconcileCheckpointingTokenWait(ctx, q, wait, resolution)
	case db.RunWaitStatusResuming:
		_, err = q.ResolveResumingRunWait(ctx, db.ResolveResumingRunWaitParams{
			WaitID: pgvalue.UUID(wait.id), RunID: pgvalue.UUID(addressedRun.id), ExpectedRunRevision: addressedRun.revision,
			ConditionStatus: string(resolution.conditionStatus), ConditionResult: resolution.result,
			ConditionError: resolution.conditionError, ReasonCode: pgvalue.TextPtr(resolution.reasonCode),
		})
	case db.RunWaitStatusParked:
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
	return true, wait.suspensionStatus == db.RunWaitStatusCheckpointing, nil
}

func lockTokenWaitRun(ctx context.Context, q *db.Queries, environmentID, runID uuid.UUID) (tokenWaitLockedRun, error) {
	run, err := q.LockTokenWaitRun(ctx, db.LockTokenWaitRunParams{
		EnvironmentID: pgvalue.UUID(environmentID), RunID: pgvalue.UUID(runID),
	})
	if err != nil {
		return tokenWaitLockedRun{}, tokenWaitAuthorityError("lock Run", err)
	}
	return tokenWaitLockedRun{
		id: pgvalue.MustUUIDValue(run.ID), computerID: pgvalue.MustUUIDValue(run.ComputerID),
		actorID: run.SessionID, entrypointKind: run.EntrypointKind, status: db.RunStatus(run.Status),
		revision: run.Revision, currentAttempt: run.CurrentAttemptNumber,
		currentRunLeaseID: run.CurrentRunLeaseID, activeStartedAt: run.ActiveStartedAt,
	}, nil
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
		ComputerID:    locator.ComputerID,
		AttemptNumber: locator.AttemptNumber,
		TokenID:       pgvalue.UUID(tokenID),
	})
	if err != nil {
		return tokenWaitLockedWait{}, err
	}
	return tokenWaitLockedWait{
		id:                  pgvalue.MustUUIDValue(locked.ID),
		runID:               pgvalue.MustUUIDValue(locked.RunID),
		computerID:          pgvalue.MustUUIDValue(locked.ComputerID),
		kind:                locked.Kind,
		conditionStatus:     db.WaitStatus(locked.ConditionStatus),
		suspensionStatus:    db.RunWaitStatus(locked.SuspensionStatus),
		expectedRunRevision: locked.ExpectedRunRevision,
		attemptNumber:       locked.AttemptNumber,
		currentRunLeaseID:   locked.CurrentRunLeaseID,
		priorRunLeaseID:     locked.PriorRunLeaseID,
		suspendCheckpointID: locked.SuspendCheckpointID,
		timeoutAt:           locked.TimeoutAt,
		timedOut:            locked.TimedOut,
	}, nil
}

func validateLockedTokenWait(run tokenWaitLockedRun, wait tokenWaitLockedWait) error {
	if wait.kind != db.WaitKindToken ||
		wait.runID != run.id || wait.computerID != run.computerID ||
		wait.attemptNumber != run.currentAttempt || wait.expectedRunRevision != run.revision ||
		run.status != db.RunStatusWaiting {
		return tokenWaitAuthorityError("run and token wait fences do not match", nil)
	}
	switch wait.suspensionStatus {
	case db.RunWaitStatusHot, db.RunWaitStatusCheckpointing:
		if wait.conditionStatus != db.WaitStatusPending && wait.suspensionStatus != db.RunWaitStatusCheckpointing {
			return tokenWaitAuthorityError("terminal token wait is not awaiting checkpoint readiness", nil)
		}
		if !run.currentRunLeaseID.Valid || !wait.currentRunLeaseID.Valid ||
			run.currentRunLeaseID != wait.currentRunLeaseID || wait.priorRunLeaseID.Valid ||
			!run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("hot token wait lease fence does not match", nil)
		}
	case db.RunWaitStatusResuming:
		if !run.currentRunLeaseID.Valid || run.currentRunLeaseID != wait.currentRunLeaseID ||
			!wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid {
			return tokenWaitAuthorityError("restoring token wait provenance does not match", nil)
		}
	case db.RunWaitStatusParked:
		if wait.conditionStatus != db.WaitStatusPending {
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

func tokenWaitTerminalResolution(state db.TokenStatus, completionData []byte) (tokenWaitResolution, error) {
	switch state {
	case db.TokenStatusCompleted:
		result := json.RawMessage(completionData)
		if len(result) == 0 {
			result = json.RawMessage(`null`)
		}
		return tokenWaitResolution{conditionStatus: db.WaitStatusCompleted, result: result}, nil
	case db.TokenStatusCancelled:
		reason := "token_cancelled"
		return tokenWaitResolution{
			conditionStatus: db.WaitStatusCancelled,
			reasonCode:      &reason,
			conditionError:  json.RawMessage(`{"code":"token_cancelled","retryable":false}`),
		}, nil
	case db.TokenStatusExpired:
		reason := "token_expired"
		return tokenWaitResolution{
			conditionStatus: db.WaitStatusFailed,
			reasonCode:      &reason,
			conditionError:  json.RawMessage(`{"code":"token_expired","retryable":false}`),
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
		ConditionStatus:     string(resolution.conditionStatus),
		ConditionResult:     resolution.result,
		ReasonCode:          pgvalue.TextPtr(resolution.reasonCode),
		ConditionError:      resolution.conditionError,
		WaitID:              pgvalue.UUID(wait.id),
		RunID:               pgvalue.UUID(run.id),
		ExpectedRunRevision: run.revision,
		CurrentRunLeaseID:   run.currentRunLeaseID,
		AttemptNumber:       run.currentAttempt,
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
			ConditionStatus:     string(resolution.conditionStatus),
			ConditionResult:     resolution.result,
			ReasonCode:          pgvalue.TextPtr(resolution.reasonCode),
			ConditionError:      resolution.conditionError,
			WaitID:              pgvalue.UUID(wait.id),
			RunID:               pgvalue.UUID(wait.runID),
			ExpectedRunRevision: wait.expectedRunRevision,
			CurrentRunLeaseID:   wait.currentRunLeaseID,
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
		RunID:               pgvalue.UUID(run.id),
		ExpectedRunRevision: run.revision,
		AttemptNumber:       run.currentAttempt,
		ConditionStatus:     string(resolution.conditionStatus),
		ConditionResult:     resolution.result,
		ReasonCode:          pgvalue.TextPtr(resolution.reasonCode),
		ConditionError:      resolution.conditionError,
		WaitID:              pgvalue.UUID(wait.id),
		PriorRunLeaseID:     wait.priorRunLeaseID,
		SuspendCheckpointID: wait.suspendCheckpointID,
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
