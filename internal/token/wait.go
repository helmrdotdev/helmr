package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrWaitAuthority = errors.New("token wait reconciliation authority is inconsistent")

const maxWaitBatch = int32(1000)

type WaitBatch struct {
	Examined int
	Resolved int
	Deferred int
}

type WaitRegistration struct {
	TurnID                          pgtype.UUID
	RunGeneration                   pgtype.Int8
	TokenID                         uuid.UUID
	WaitID                          uuid.UUID
	RunLeaseID                      uuid.UUID
	LeaseSequence                   int64
	WorkerGroupID                   uuid.UUID
	WorkerHostID                    uuid.UUID
	WorkerEpoch                     int64
	RequestFingerprint              string
	SessionSpeculativeInputSequence pgtype.Int8
	TimeoutAt                       pgtype.Timestamptz
	IdleTimeoutMS                   pgtype.Int8
	Metadata                        json.RawMessage
	Tags                            []string
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
	db      db.TxDB
	queries *db.Queries
}

func NewWaitReconciler(database db.TxDB) (*WaitReconciler, error) {
	if database == nil {
		return nil, errors.New("token wait reconciliation database is required")
	}
	return &WaitReconciler{db: database, queries: db.New(database)}, nil
}

// Registrar registers Token waits for worker requests. It reconciles only a
// Token that is already terminal when its wait registers.
type Registrar struct {
	txb db.TxBeginner
}

// NewRegistrar returns a Registrar whose registrations each run in one
// transaction begun from txb.
func NewRegistrar(txb db.TxBeginner) (*Registrar, error) {
	if txb == nil {
		return nil, errors.New("token wait registration database is required")
	}
	return &Registrar{txb: txb}, nil
}

// RegisterWait serializes the Run-to-Token race. The Wait is inserted before
// the Token is locked, so either registration observes a prior terminal Token
// or a concurrent terminalization publishes an intent after this transaction.
func (r *Registrar) RegisterWait(
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
	if request.SessionSpeculativeInputSequence.Valid && request.SessionSpeculativeInputSequence.Int64 < 0 {
		return WaitRegistrationResult{}, errors.New("token wait session speculative cursor is invalid")
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

	var result WaitRegistrationResult
	err := db.RunTx(ctx, r.txb, func(tx pgx.Tx) error {
		var err error
		result, err = registerTokenWait(ctx, tx, request, metadata, tags)
		return err
	})
	if err != nil {
		return WaitRegistrationResult{}, err
	}
	return result, nil
}

func registerTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	request WaitRegistration,
	metadata json.RawMessage,
	tags []string,
) (WaitRegistrationResult, error) {
	q := db.New(tx)
	// An exact existing registration is immutable and may outlive its run
	// lease. This read-only replay does not linearize creation; the mutable
	// path repeats it after locking the Run.
	if !request.TurnID.Valid {
		if replay, found, err := replayTokenWaitRegistration(ctx, q, request, metadata, tags); err != nil {
			return WaitRegistrationResult{}, err
		} else if found {
			return replay, nil
		}
	}
	stage, err := run.BeginTokenWaitRegistration(ctx, tx, run.TokenWaitFence{
		RunLeaseID: request.RunLeaseID, LeaseSequence: request.LeaseSequence,
		WorkerGroupID: request.WorkerGroupID, WorkerHostID: request.WorkerHostID, WorkerEpoch: request.WorkerEpoch,
	})
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitStageError(err)
	}
	locators, locator, lockedSession := stage.Lease(), stage.Owner(), stage.Session()
	runID := pgvalue.MustUUIDValue(locators.RunID)
	attemptNumber := locators.AttemptNumber
	lockedRun := tokenWaitRunFromRow(stage.Run())
	if replay, found, err := replayTokenWaitRegistration(ctx, q, request, metadata, tags); err != nil {
		return WaitRegistrationResult{}, err
	} else if found {
		return replay, nil
	}
	attempt, err := stage.LockAttempt(ctx)
	if err != nil {
		return WaitRegistrationResult{}, tokenWaitStageError(err)
	}
	if err := validateTokenWaitSessionCursor(
		request.SessionSpeculativeInputSequence, locator.SessionID, lockedSession.CurrentRunID,
		lockedSession.CommittedInputSequence, lockedSession.NextInputSequence,
		lockedRun, attempt.Attempt().EntrypointKind, attempt.Attempt().SessionInputStartSequence,
	); err != nil {
		return WaitRegistrationResult{}, err
	}
	if lockedRun.entrypointKind == "actor" {
		want := lockedSession.CommittedInputSequence
		if request.TurnID.Valid {
			want++
		}
		if request.SessionSpeculativeInputSequence.Int64 != want {
			return WaitRegistrationResult{}, tokenWaitAuthorityError("Token wait cursor does not identify its admitted Turn", nil)
		}
	}
	if err := attempt.LockLease(ctx); err != nil {
		return WaitRegistrationResult{}, tokenWaitStageError(err)
	}

	registered, err := q.RegisterTokenWait(ctx, db.RegisterTokenWaitParams{
		WaitID:                  pgvalue.UUID(request.WaitID),
		EnvironmentID:           locators.EnvironmentID,
		TimeoutAt:               request.TimeoutAt,
		IdleTimeoutMs:           request.IdleTimeoutMS,
		TokenID:                 pgvalue.UUID(request.TokenID),
		ExpectedRunningRevision: lockedRun.revision,
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
		if lockedSession.DispatchHoldID.Valid || lockedSession.ActiveTurnID != request.TurnID || lockedSession.RunGeneration != request.RunGeneration.Int64 && request.TurnID.Valid {
			return WaitRegistrationResult{}, ErrWaitAuthority
		}
		if request.TurnID.Valid {
			_, err = q.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: lockedSession.ID, TurnID: request.TurnID, RunGeneration: request.RunGeneration, WaitID: registered.ID})
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
		lockedRun.revision = waitingRevision
		lockedRun.status = db.RunStatusWaiting
		if err := reconcileHotTokenWait(ctx, q, lockedRun, wait, resolution); err != nil {
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

func validateTokenWaitSessionCursor(
	cursor pgtype.Int8,
	ownerSessionID pgtype.UUID,
	sessionCurrentRunID pgtype.UUID,
	sessionCommittedInputSequence int64,
	sessionNextInputSequence int64,
	run tokenWaitLockedRun,
	attemptEntrypointKind string,
	attemptSessionInputStartSequence pgtype.Int8,
) error {
	switch run.entrypointKind {
	case "task":
		if run.sessionID.Valid || cursor.Valid || attemptEntrypointKind != "task" ||
			attemptSessionInputStartSequence.Valid {
			return tokenWaitAuthorityError("task token wait carries actor authority", nil)
		}
	case "actor":
		if !run.sessionID.Valid || run.sessionID != ownerSessionID || !sessionCurrentRunID.Valid ||
			uuid.UUID(sessionCurrentRunID.Bytes) != run.id || attemptEntrypointKind != "actor" ||
			!attemptSessionInputStartSequence.Valid || !cursor.Valid ||
			attemptSessionInputStartSequence.Int64 > sessionCommittedInputSequence ||
			cursor.Int64 < sessionCommittedInputSequence ||
			cursor.Int64 > sessionCommittedInputSequence+1 ||
			cursor.Int64 >= sessionNextInputSequence {
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
	sessionID         pgtype.UUID
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
) (bool, bool, error) {
	var resolved, deferred bool
	err := db.RunTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		resolved, deferred, err = reconcileTokenWait(ctx, tx, environmentID, tokenID, waitID, runID, timeout)
		return err
	})
	if err != nil {
		return false, false, err
	}
	return resolved, deferred, nil
}

// reconcileTokenWait resolves one Token wait in tx. A wait that is gone,
// converged, no longer current or not yet due commits without resolving.
func reconcileTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	waitID uuid.UUID,
	runID uuid.UUID,
	timeout bool,
) (resolved bool, deferred bool, err error) {
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
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}

	if err := computer.LockResidence(ctx, tx, environmentID, pgvalue.MustUUIDValue(locator.ComputerID)); err != nil {
		return false, false, tokenWaitAuthorityError("lock Computer residence", err)
	}

	var lockedSessionCurrentRunID pgtype.UUID
	if locator.SessionID.Valid {
		session, err := q.LockTokenWaitSession(ctx, locator.SessionID)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock owning actor", err)
		}
		if session.Status != "open" && session.Status != "closing" {
			return false, false, tokenWaitAuthorityError("owning actor is not active", nil)
		}
		lockedSessionCurrentRunID = session.CurrentRunID
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
	if locator.SessionID.Valid && (!lockedSessionCurrentRunID.Valid || lockedSessionCurrentRunID != locator.RunID) {
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
		return false, false, nil
	}
	if err := validateLockedTokenWait(addressedRun, wait); err != nil {
		return false, false, err
	}
	if wait.conditionStatus != db.WaitStatusPending {
		return false, true, nil
	}

	var resolution tokenWaitResolution
	if timeout {
		if !wait.timeoutAt.Valid || !wait.timedOut {
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
	return true, wait.suspensionStatus == db.RunWaitStatusCheckpointing, nil
}

func lockTokenWaitRun(ctx context.Context, q *db.Queries, environmentID, runID uuid.UUID) (tokenWaitLockedRun, error) {
	locked, err := q.LockTokenWaitRun(ctx, db.LockTokenWaitRunParams{
		EnvironmentID: pgvalue.UUID(environmentID), RunID: pgvalue.UUID(runID),
	})
	if err != nil {
		return tokenWaitLockedRun{}, tokenWaitAuthorityError("lock Run", err)
	}
	return tokenWaitRunFromRow(locked), nil
}

func tokenWaitRunFromRow(locked db.Run) tokenWaitLockedRun {
	return tokenWaitLockedRun{
		id: pgvalue.MustUUIDValue(locked.ID), computerID: pgvalue.MustUUIDValue(locked.ComputerID),
		sessionID: locked.SessionID, entrypointKind: locked.EntrypointKind, status: db.RunStatus(locked.Status),
		revision: locked.Revision, currentAttempt: locked.CurrentAttemptNumber,
		currentRunLeaseID: locked.CurrentRunLeaseID, activeStartedAt: locked.ActiveStartedAt,
	}
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

// tokenWaitStageError reports a rejected or failed registration stage as a
// Token wait authority failure.
func tokenWaitStageError(cause error) error {
	return fmt.Errorf("%w: %w", ErrWaitAuthority, cause)
}

func tokenWaitAuthorityError(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrWaitAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrWaitAuthority, operation, cause)
}
