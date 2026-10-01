package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrCancellationNotFound  = errors.New("run not found")
	ErrCancellationConflict  = errors.New("run has another terminal outcome")
	ErrCancellationAuthority = errors.New("run cancellation authority is inconsistent")
)

const maxCancellationGraphSize = 1000

type CancellationRequest struct {
	IdempotencyKey string
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	RunID          uuid.UUID
}

type CancellationResult struct {
	Actor         *ActorCancellationReceipt
	RunID         uuid.UUID
	Changed       bool
	CancelledRuns int
}

type Canceler struct {
	db db.TxBeginner
}

type cancellationRun struct {
	id                       uuid.UUID
	parentRunID              pgtype.UUID
	parentOwnsLifecycle      pgtype.Bool
	environmentID            uuid.UUID
	computerID               uuid.UUID
	actorID                  pgtype.UUID
	status                   db.RunStatus
	currentAttemptNumber     int32
	currentRunLeaseID        pgtype.UUID
	revision                 int64
	instancePreparationCount int32
	depth                    int
}

type cancellationWait struct {
	id                  uuid.UUID
	runID               uuid.UUID
	computerID          uuid.UUID
	childRunID          pgtype.UUID
	conditionStatus     db.WaitStatus
	suspensionStatus    db.RunWaitStatus
	expectedRunRevision int64
	attemptNumber       int32
	currentRunLeaseID   pgtype.UUID
	priorRunLeaseID     pgtype.UUID
	suspendCheckpointID pgtype.UUID
}

type terminalChildWaitResolution struct {
	conditionStatus db.WaitStatus
	result          json.RawMessage
	reasonCode      *string
	conditionError  json.RawMessage
}

type termination struct {
	reasonCode     string
	errorCode      string
	errorMessage   string
	runStatus      db.RunStatus
	runLeaseStatus db.RunLeaseStatus
	attemptOutcome string
	waitCondition  db.WaitStatus
	waitSuspension db.RunWaitStatus
	eventKind      string
	eventMessage   string
}

var cancelledTermination = termination{
	reasonCode:     "run_cancelled",
	errorCode:      "run_cancelled",
	errorMessage:   "Run was cancelled",
	runStatus:      db.RunStatusCancelled,
	runLeaseStatus: db.RunLeaseStatusCancelled,
	attemptOutcome: "cancelled",
	waitCondition:  db.WaitStatusCancelled,
	waitSuspension: db.RunWaitStatusCancelled,
	eventKind:      "run.cancelled",
	eventMessage:   "Run cancelled",
}

var secretRevokedTermination = termination{
	reasonCode:     "secret_revoked",
	errorCode:      "secret_revoked",
	errorMessage:   "A Computer Secret used by this Run was revoked",
	runStatus:      db.RunStatusFailed,
	runLeaseStatus: db.RunLeaseStatusFailed,
	attemptOutcome: "failed",
	waitCondition:  db.WaitStatusFailed,
	waitSuspension: db.RunWaitStatusFailed,
	eventKind:      "run.failed",
	eventMessage:   "Run failed",
}

var runtimePreparationTermination = termination{
	reasonCode:     "runtime_preparation_failed",
	errorCode:      "runtime_preparation_failed",
	errorMessage:   "Run runtime preparation failed",
	runStatus:      db.RunStatusSystemFailed,
	runLeaseStatus: db.RunLeaseStatusFailed,
	attemptOutcome: "failed",
	waitCondition:  db.WaitStatusFailed,
	waitSuspension: db.RunWaitStatusFailed,
	eventKind:      "run.system_failed",
	eventMessage:   "Run runtime preparation failed",
}

func NewCanceler(database db.TxBeginner) (*Canceler, error) {
	if database == nil {
		return nil, errors.New("run cancellation database is required")
	}
	return &Canceler{db: database}, nil
}

type OwnedFinalizationRequest struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
}

type OwnedFinalization struct {
	tx           pgx.Tx
	currentRun   uuid.UUID
	descendants  []cancellationRun
	locked       map[uuid.UUID]cancellationRun
	waitsByChild map[uuid.UUID]cancellationWait
}

// LockOwnedFinalization acquires the global cancellation
// lock order before ordinary Run authority is locked. It lets terminal
// finalization re-lock its exact authority without later reaching from a
// Computer lock back to an unlocked descendant Run.
func LockOwnedFinalization(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedFinalizationRequest,
) (OwnedFinalization, error) {
	return lockOwnedFinalization(ctx, tx, request, nil)
}

// SessionRunSecrets addresses a Session's current Run and the Session's
// Computer, whose bindings that Run's current attempt resolved.
type SessionRunSecrets struct {
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
	ComputerID    uuid.UUID
}

// LockOwnedFinalizationWithSecrets reads the Session's current Run, locks its
// current attempt's Secret deliveries and then its owned finalization graph.
// A missing Run is pgx.ErrNoRows; Secret errors are
// secret.LockAttemptDelivery's.
func LockOwnedFinalizationWithSecrets(ctx context.Context, tx pgx.Tx, request SessionRunSecrets) (OwnedFinalization, error) {
	q := db.New(tx)
	current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), ID: pgvalue.UUID(request.RunID)})
	if err != nil {
		return OwnedFinalization{}, err
	}
	if _, err = secret.LockAttemptDelivery(ctx, q, current.ID, current.CurrentAttemptNumber, pgvalue.UUID(request.ComputerID)); err != nil {
		return OwnedFinalization{}, err
	}
	return LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(current.OrgID), ProjectID: pgvalue.MustUUIDValue(current.ProjectID), EnvironmentID: request.EnvironmentID, RunID: pgvalue.MustUUIDValue(current.ID)})
}

// LockOwnedFinalizationWithInstanceFence fences Worker dispatch before acquiring
// Computer, Instance and member locks for the owned Run graph.
func LockOwnedFinalizationWithInstanceFence(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedFinalizationRequest,
	beforeInstance func() error,
) (OwnedFinalization, error) {
	if beforeInstance == nil {
		return OwnedFinalization{}, errors.New("owned run finalization Runtime fence is required")
	}
	return lockOwnedFinalization(ctx, tx, request, beforeInstance)
}

func lockOwnedFinalization(
	ctx context.Context,
	tx pgx.Tx,
	request OwnedFinalizationRequest,
	beforeInstance func() error,
) (OwnedFinalization, error) {
	if tx == nil || request.OrgID == uuid.Nil() || request.ProjectID == uuid.Nil() ||
		request.EnvironmentID == uuid.Nil() || request.RunID == uuid.Nil() {
		return OwnedFinalization{}, errors.New("owned run finalization graph authority is required")
	}
	scope := CancellationRequest{
		OrgID: request.OrgID, ProjectID: request.ProjectID,
		EnvironmentID: request.EnvironmentID,
	}
	lineage, err := cancellationLineage(ctx, tx, request.RunID)
	if err != nil {
		return OwnedFinalization{}, err
	}
	descendantIDs, err := discoverOwnedCancellationRuns(
		ctx, tx, scope, request.RunID,
	)
	if err != nil {
		return OwnedFinalization{}, err
	}
	lockOrder := append(slices.Clone(lineage), descendantIDs[1:]...)
	if len(lockOrder) > maxCancellationGraphSize {
		return OwnedFinalization{}, cancellationAuthority(
			"owned run finalization graph exceeds the transaction bound",
			nil,
		)
	}
	if err := lockCancellationComputers(ctx, tx, lockOrder, beforeInstance); err != nil {
		return OwnedFinalization{}, err
	}
	slices.SortFunc(lockOrder, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if err := lockCancellationActors(ctx, tx, scope, lockOrder); err != nil {
		return OwnedFinalization{}, err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lockOrder))
	for _, id := range lockOrder {
		run, err := lockCancellationRun(ctx, tx, scope, id)
		if err != nil {
			return OwnedFinalization{}, cancellationAuthority(
				"lock owned run finalization graph",
				err,
			)
		}
		locked[id] = run
	}
	reloaded, err := discoverOwnedCancellationRuns(
		ctx, tx, scope, request.RunID,
	)
	if err != nil {
		return OwnedFinalization{}, err
	}
	if !slices.Equal(descendantIDs, reloaded) {
		return OwnedFinalization{}, cancellationAuthority(
			"owned run finalization graph changed during lock acquisition",
			nil,
		)
	}
	descendants := make([]cancellationRun, 0, len(descendantIDs))
	for depth, id := range descendantIDs {
		run, found := locked[id]
		if !found {
			return OwnedFinalization{}, cancellationAuthority(
				"owned run finalization descendant was not locked",
				nil,
			)
		}
		run.depth = depth
		descendants = append(descendants, run)
	}
	waitsByChild, err := lockCancellationResources(
		ctx, tx, lockOrder, descendants,
	)
	if err != nil {
		return OwnedFinalization{}, err
	}
	return OwnedFinalization{
		tx: tx, currentRun: request.RunID, descendants: descendants,
		locked: locked, waitsByChild: waitsByChild,
	}, nil
}

// CancelDescendants terminalizes all still-active owned descendants without
// resolving the current Run's boundary Wait. The current Run is terminalized
// by the caller in the same transaction.
func (g OwnedFinalization) CancelDescendants(ctx context.Context) (int, error) {
	if g.tx == nil || g.currentRun == uuid.Nil() || len(g.descendants) == 0 ||
		g.descendants[0].id != g.currentRun {
		return 0, errors.New("owned run finalization graph is invalid")
	}
	runs := slices.Clone(g.descendants[1:])
	slices.SortFunc(runs, func(left, right cancellationRun) int {
		if left.depth != right.depth {
			return right.depth - left.depth
		}
		return slices.Compare(left.id[:], right.id[:])
	})
	cancelled := 0
	for _, run := range runs {
		if runStatusTerminal(run.status) {
			continue
		}
		if err := cancelLockedRun(ctx, g.tx, run); err != nil {
			return 0, err
		}
		cancelled++
	}
	return cancelled, nil
}

// FailCurrentForSecretRevocation terminalizes the graph root with an explicit
// Secret revocation error after cancelling its owned descendants. The caller
// must lock and validate the Computer's complete Secret set before acquiring
// this graph.
func (g OwnedFinalization) FailCurrentForSecretRevocation(
	ctx context.Context,
) (int, error) {
	cancelled, err := g.CancelDescendants(ctx)
	if err != nil {
		return 0, err
	}
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return cancelled, nil
	}
	if err := terminateLockedRun(
		ctx,
		g.tx,
		target,
		secretRevokedTermination,
	); err != nil {
		return 0, err
	}
	if target.parentRunID.Valid && target.parentOwnsLifecycle.Valid &&
		target.parentOwnsLifecycle.Bool {
		parentID := uuid.UUID(target.parentRunID.Bytes)
		parent, found := g.locked[parentID]
		if found && !runStatusTerminal(parent.status) {
			wait, found := g.waitsByChild[target.id]
			if !found {
				return 0, cancellationAuthority(
					"secret-revoked child wait boundary is inconsistent",
					nil,
				)
			}
			result, err := marshalChildFailureResult(
				target.id,
				"secret_revoked",
				"A Computer Secret used by the child Run was revoked",
			)
			if err != nil {
				return 0, err
			}
			if err := resolveChildResult(
				ctx,
				g.tx,
				parent,
				wait,
				result,
			); err != nil {
				return 0, err
			}
		}
	}
	return cancelled + 1, nil
}

// ChargeRuntimePreparationFailure records one infrastructure delivery failure.
// Exhaustion terminalizes the exact Run and its owned graph without consuming
// the user execution RetryPolicy.
func (g OwnedFinalization) ChargeRuntimePreparationFailure(
	ctx context.Context,
) (bool, error) {
	if g.tx == nil || g.currentRun == uuid.Nil() || len(g.descendants) == 0 ||
		g.descendants[0].id != g.currentRun {
		return false, errors.New("runtime preparation failure authority is invalid")
	}
	target := g.descendants[0]
	if target.status != db.RunStatusQueued || target.currentRunLeaseID.Valid {
		return false, cancellationAuthority("runtime preparation target is not queued", nil)
	}
	var preparationPending, sourceFailure bool
	var failure []byte
	if err := g.tx.QueryRow(ctx, `SELECT preparation_attempt_count>0,recovery_failure IS NOT NULL,preparation_failure FROM computers WHERE id=$1`, pgvalue.UUID(target.computerID)).Scan(&preparationPending, &sourceFailure, &failure); err != nil {
		return false, err
	}
	if len(failure) > 0 {
		return true, g.failCurrentForComputerPreparation(ctx)
	}
	if sourceFailure {
		return true, g.failCurrentForComputerSource(ctx)
	}
	// A physical attempt is already charged to the Computer. Its failure fact
	// may still await settlement; no Run may charge or reset that budget.
	if preparationPending {
		return false, nil
	}
	if target.instancePreparationCount < 0 || target.instancePreparationCount > 7 {
		return false, cancellationAuthority("runtime preparation count is invalid", nil)
	}
	queries := db.New(g.tx)
	if target.instancePreparationCount < 7 {
		if _, err := queries.ChargeRunInstancePreparationFailure(
			ctx,
			db.ChargeRunInstancePreparationFailureParams{
				ID:            pgvalue.UUID(target.id),
				AttemptNumber: target.currentAttemptNumber,
				ExpectedCount: target.instancePreparationCount,
			},
		); err != nil {
			return false, cancellationAuthority("charge runtime preparation failure", err)
		}
		return false, nil
	}
	if _, err := queries.ExhaustRunInstancePreparation(
		ctx,
		db.ExhaustRunInstancePreparationParams{
			ID:            pgvalue.UUID(target.id),
			AttemptNumber: target.currentAttemptNumber,
		},
	); err != nil {
		return false, cancellationAuthority("exhaust runtime preparation", err)
	}
	if err := g.failCurrentForRuntimePreparation(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (g OwnedFinalization) failCurrentForRuntimePreparation(ctx context.Context) error {
	return g.failCurrentPreparation(ctx, runtimePreparationTermination)
}

// FailComputerPreparation settles unstarted work or members of an invalidated
// restore checkpoint. The caller holds the normal Run graph; physical failure
// and checkpoint invalidation must already be durable.
func (g OwnedFinalization) FailComputerPreparation(ctx context.Context) (bool, error) {
	if g.tx == nil || len(g.descendants) == 0 {
		return false, errors.New("missing Run authority")
	}
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return false, nil
	}
	var failed, sourceFailed, parked bool
	if err := g.tx.QueryRow(ctx, `SELECT c.preparation_failure IS NOT NULL,c.recovery_failure IS NOT NULL,EXISTS(
 SELECT 1 FROM run_waits w JOIN computer_checkpoints cp ON cp.id=w.suspend_checkpoint_id
 JOIN computer_checkpoint_runs m ON m.checkpoint_id=cp.id AND m.run_wait_id=w.id AND m.run_id=w.run_id AND m.attempt_number=w.attempt_number
 WHERE w.run_id=$2 AND w.attempt_number=$3 AND w.computer_id=c.id
 AND w.suspension_status IN ('parked','resume_pending','resuming')
 AND cp.status='invalid' AND cp.invalidation_reason_code='computer_preparation_exhausted')
 FROM computers c WHERE c.id=$1`, pgvalue.UUID(target.computerID), pgvalue.UUID(target.id), target.currentAttemptNumber).Scan(&failed, &sourceFailed, &parked); err != nil {
		return false, err
	}
	if !failed && !sourceFailed {
		return false, nil
	}
	unstarted := (target.status == db.RunStatusQueued || target.status == db.RunStatusRetryDelayed) && !target.currentRunLeaseID.Valid
	if !unstarted && !(failed && target.status == db.RunStatusWaiting && parked) {
		return false, nil
	}
	if sourceFailed && unstarted {
		return true, g.failCurrentForComputerSource(ctx)
	}
	return true, g.failCurrentForComputerPreparation(ctx)
}
func (g OwnedFinalization) failCurrentForComputerPreparation(ctx context.Context) error {
	failure := runtimePreparationTermination
	failure.reasonCode = "computer_preparation_exhausted"
	failure.errorCode = failure.reasonCode
	failure.errorMessage = "Computer preparation limit reached"
	return g.failCurrentPreparation(ctx, failure)
}

func (g OwnedFinalization) failCurrentForComputerSource(ctx context.Context) error {
	failure := runtimePreparationTermination
	failure.reasonCode = "computer_source_unavailable"
	failure.errorCode = failure.reasonCode
	failure.errorMessage = "Published Computer source is unavailable"
	return g.failCurrentPreparation(ctx, failure)
}
func (g OwnedFinalization) failCurrentPreparation(ctx context.Context, failure termination) error {
	if _, err := g.CancelDescendants(ctx); err != nil {
		return err
	}
	target := g.descendants[0]
	if runStatusTerminal(target.status) {
		return nil
	}
	if err := terminateLockedRun(
		ctx,
		g.tx,
		target,
		failure,
	); err != nil {
		return err
	}
	if !target.parentRunID.Valid || !target.parentOwnsLifecycle.Valid ||
		!target.parentOwnsLifecycle.Bool {
		return nil
	}
	parentID := uuid.UUID(target.parentRunID.Bytes)
	parent, found := g.locked[parentID]
	if !found || runStatusTerminal(parent.status) {
		return nil
	}
	wait, found := g.waitsByChild[target.id]
	if !found {
		return cancellationAuthority("runtime preparation child wait is missing", nil)
	}
	result, err := marshalChildFailureResult(
		target.id,
		failure.errorCode,
		failure.errorMessage,
	)
	if err != nil {
		return err
	}
	return resolveChildResult(ctx, g.tx, parent, wait, result)
}

// Cancel cancels the addressed Run in its own transaction. An Actor Run's
// cancellation locks the Run's owned graph, re-locks its Session and acquires
// the idempotency claim last; a committed rejection is returned with its
// result as a CancellationRejectionError. A Task Run's cancellation locks its
// lineage's Computers, Sessions and Runs in order and cancels its owned
// descendants, resolving a parent-owned boundary's wait.
func (c *Canceler) Cancel(
	ctx context.Context,
	request CancellationRequest,
) (CancellationResult, error) {
	if request.OrgID == uuid.Nil() || request.ProjectID == uuid.Nil() ||
		request.EnvironmentID == uuid.Nil() ||
		request.RunID == uuid.Nil() {
		return CancellationResult{}, errors.New("run cancellation scope and ID are required")
	}
	var result CancellationResult
	var rejection string
	err := db.RunTx(ctx, c.db, func(tx pgx.Tx) error {
		var err error
		result, rejection, err = cancelInTx(ctx, tx, request)
		return err
	})
	if err != nil {
		return CancellationResult{}, err
	}
	if rejection != "" {
		return result, &CancellationRejectionError{Code: rejection}
	}
	return result, nil
}

// cancelInTx is Cancel's transaction. An accepted Actor cancellation that the
// Session rejects commits and reports the rejection code.
func cancelInTx(ctx context.Context, tx pgx.Tx, request CancellationRequest) (CancellationResult, string, error) {
	targetID, err := findCancellationTarget(ctx, tx, request)
	if errors.Is(err, pgx.ErrNoRows) {
		return CancellationResult{}, "", ErrCancellationNotFound
	}
	if err != nil {
		return CancellationResult{}, "", cancellationAuthority("resolve target run", err)
	}
	targetRun, err := db.New(tx).GetRun(ctx, db.GetRunParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), ID: pgvalue.UUID(targetID)})
	if err != nil {
		return CancellationResult{}, "", err
	}
	if targetRun.SessionID.Valid {
		// Acquire the owned graph before Session admission/claim mutation. A
		// no-worker stop can then retire it without expanding the lock set.
		graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{
			OrgID: request.OrgID, ProjectID: request.ProjectID,
			EnvironmentID: request.EnvironmentID, RunID: targetID,
		})
		if err != nil {
			return CancellationResult{}, "", err
		}
		receipt, err := acceptActorRunCancellation(ctx, tx, request, targetRun, graph)
		if err != nil {
			return CancellationResult{}, "", err
		}
		return CancellationResult{RunID: targetID, Actor: &receipt, Changed: receipt.Status == "accepted"}, receipt.Code, nil
	}
	lineage, err := cancellationLineage(ctx, tx, targetID)
	if err != nil {
		return CancellationResult{}, "", err
	}
	descendants, err := discoverOwnedCancellationRuns(ctx, tx, request, targetID)
	if err != nil {
		return CancellationResult{}, "", err
	}
	lockOrder := append(slices.Clone(lineage), descendants[1:]...)
	if len(lockOrder) > maxCancellationGraphSize {
		return CancellationResult{}, "", cancellationAuthority(
			"run cancellation graph exceeds the transaction bound",
			nil,
		)
	}
	if err := lockCancellationComputers(ctx, tx, lockOrder, nil); err != nil {
		return CancellationResult{}, "", err
	}
	slices.SortFunc(lockOrder, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if err := lockCancellationActors(ctx, tx, request, lockOrder); err != nil {
		return CancellationResult{}, "", err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lockOrder))
	for _, id := range lockOrder {
		run, err := lockCancellationRun(ctx, tx, request, id)
		if err != nil {
			return CancellationResult{}, "", cancellationAuthority("lock run graph", err)
		}
		locked[id] = run
	}
	target, ok := locked[targetID]
	if !ok {
		return CancellationResult{}, "", cancellationAuthority("target run was not locked", nil)
	}
	result := CancellationResult{RunID: target.id}
	if runStatusTerminal(target.status) {
		if target.status != db.RunStatusCancelled {
			return CancellationResult{}, "", ErrCancellationConflict
		}
		return result, "", nil
	}

	reloaded, err := discoverOwnedCancellationRuns(ctx, tx, request, targetID)
	if err != nil {
		return CancellationResult{}, "", err
	}
	if !slices.Equal(descendants, reloaded) {
		return CancellationResult{}, "", cancellationAuthority(
			"run cancellation graph changed during lock acquisition",
			nil,
		)
	}
	cancelled := make(map[uuid.UUID]struct{}, len(descendants))
	runs := make([]cancellationRun, 0, len(descendants))
	for depth, id := range descendants {
		run, found := locked[id]
		if !found {
			return CancellationResult{}, "", cancellationAuthority(
				"parent-owned run was not locked",
				nil,
			)
		}
		run.depth = depth
		cancelled[id] = struct{}{}
		runs = append(runs, run)
	}
	waitsByChild, err := lockCancellationResources(ctx, tx, lockOrder, runs)
	if err != nil {
		return CancellationResult{}, "", err
	}
	var boundaryParent cancellationRun
	var boundaryWait cancellationWait
	resolveBoundaryParent := false
	if target.parentRunID.Valid && target.parentOwnsLifecycle.Valid &&
		target.parentOwnsLifecycle.Bool {
		parentID := uuid.UUID(target.parentRunID.Bytes)
		if _, parentCancelled := cancelled[parentID]; !parentCancelled {
			var found bool
			boundaryParent, found = locked[parentID]
			if !found {
				return CancellationResult{}, "", cancellationAuthority(
					"parent-owned run parent was not locked",
					nil,
				)
			}
			boundaryWait, found = waitsByChild[target.id]
			if !found {
				return CancellationResult{}, "", cancellationAuthority(
					"parent-owned run wait was not locked",
					nil,
				)
			}
			if err := validateCancellationBoundary(boundaryParent, target, boundaryWait); err != nil {
				return CancellationResult{}, "", err
			}
			resolveBoundaryParent = true
		}
	}
	slices.SortFunc(runs, func(left, right cancellationRun) int {
		if left.depth != right.depth {
			return right.depth - left.depth
		}
		return slices.Compare(left.id[:], right.id[:])
	})
	for _, run := range runs {
		if err := cancelLockedRun(ctx, tx, run); err != nil {
			return CancellationResult{}, "", err
		}
		if resolveBoundaryParent && run.id == target.id {
			if err := resolveCancelledChildWait(
				ctx,
				tx,
				boundaryParent,
				run,
				boundaryWait,
			); err != nil {
				return CancellationResult{}, "", err
			}
		}
	}
	result.Changed = true
	result.CancelledRuns = len(runs)
	return result, "", nil
}

func findCancellationTarget(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
) (uuid.UUID, error) {
	id, err := db.New(tx).FindCancellationTarget(ctx, db.FindCancellationTargetParams{
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		ID:            pgvalue.UUID(request.RunID),
	})
	return uuid.UUID(id.Bytes), err
}

func cancellationLineage(
	ctx context.Context,
	tx pgx.Tx,
	targetID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).ListCancellationLineage(ctx, db.ListCancellationLineageParams{
		TargetID: pgvalue.UUID(targetID),
		MaxDepth: maxCancellationGraphSize,
	})
	if err != nil {
		return nil, cancellationAuthority("load run lineage", err)
	}
	var ids []uuid.UUID
	for _, row := range rows {
		if row.Cycle {
			return nil, cancellationAuthority("run lineage contains a cycle", nil)
		}
		ids = append(ids, uuid.UUID(row.ID.Bytes))
		if len(ids) > maxCancellationGraphSize {
			return nil, cancellationAuthority("run lineage exceeds the transaction bound", nil)
		}
	}
	if len(ids) == 0 || ids[len(ids)-1] != targetID {
		return nil, cancellationAuthority("run lineage is incomplete", nil)
	}
	return ids, nil
}

func lockCancellationActors(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
	lineage []uuid.UUID,
) error {
	_, err := db.New(tx).LockCancellationActors(ctx, db.LockCancellationActorsParams{
		RunIDs:        pgUUIDs(lineage),
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
	})
	if err != nil {
		return cancellationAuthority("lock run lineage actors", err)
	}
	return nil
}

func lockCancellationRun(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
	id uuid.UUID,
) (cancellationRun, error) {
	row, err := db.New(tx).LockCancellationRun(ctx, db.LockCancellationRunParams{
		ID:            pgvalue.UUID(id),
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
	})
	if err != nil {
		return cancellationRun{}, err
	}
	return cancellationRun{
		id:                       uuid.UUID(row.ID.Bytes),
		parentRunID:              row.ParentRunID,
		parentOwnsLifecycle:      row.ParentOwnsLifecycle,
		environmentID:            uuid.UUID(row.EnvironmentID.Bytes),
		computerID:               uuid.UUID(row.ComputerID.Bytes),
		actorID:                  row.SessionID,
		status:                   row.Status,
		currentAttemptNumber:     row.CurrentAttemptNumber,
		currentRunLeaseID:        row.CurrentRunLeaseID,
		revision:                 row.Revision,
		instancePreparationCount: row.InstancePreparationCount,
	}, nil
}

func discoverOwnedCancellationRuns(
	ctx context.Context,
	tx pgx.Tx,
	request CancellationRequest,
	targetID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).ListOwnedCancellationRuns(ctx, db.ListOwnedCancellationRunsParams{
		TargetID:      pgvalue.UUID(targetID),
		OrgID:         pgvalue.UUID(request.OrgID),
		ProjectID:     pgvalue.UUID(request.ProjectID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		MaxDepth:      maxCancellationGraphSize + 1,
		LimitCount:    maxCancellationGraphSize + 1,
	})
	if err != nil {
		return nil, cancellationAuthority("discover parent-owned Runs", err)
	}
	var ids []uuid.UUID
	for _, row := range rows {
		if row.Cycle {
			return nil, cancellationAuthority("parent-owned run graph contains a cycle", nil)
		}
		ids = append(ids, uuid.UUID(row.ID.Bytes))
	}
	if len(ids) == 0 || ids[0] != targetID {
		return nil, cancellationAuthority("parent-owned run graph is incomplete", nil)
	}
	if len(ids) > maxCancellationGraphSize {
		return nil, cancellationAuthority("run cancellation graph exceeds the transaction bound", nil)
	}
	return ids, nil
}

func lockCancellationComputers(ctx context.Context, tx pgx.Tx, runIDs []uuid.UUID, beforeInstance func() error) error {
	if beforeInstance != nil {
		if err := beforeInstance(); err != nil {
			return err
		}
	}
	if err := computer.LockRunComputers(ctx, tx, runIDs); err != nil {
		return cancellationAuthority("lock cancellation Computers and Instances", err)
	}
	return nil
}

func lockCancellationResources(ctx context.Context, tx pgx.Tx, runIDs []uuid.UUID, cancelRuns []cancellationRun) (map[uuid.UUID]cancellationWait, error) {
	cancelIDs := make([]uuid.UUID, 0, len(cancelRuns))
	for _, run := range cancelRuns {
		cancelIDs = append(cancelIDs, run.id)
	}
	if err := lockCancellationAttempts(ctx, tx, runIDs); err != nil {
		return nil, err
	}
	if _, err := lockCancellationRunLeases(ctx, tx, runIDs); err != nil {
		return nil, err
	}
	return lockCancellationWaits(ctx, tx, runIDs, cancelIDs)
}

func lockCancellationAttempts(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) error {
	_, err := db.New(tx).LockCancellationAttempts(ctx, pgUUIDs(runIDs))
	if err != nil {
		return cancellationAuthority("lock cancellation attempts", err)
	}
	return nil
}

func lockCancellationRunLeases(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := db.New(tx).LockCancellationRunLeases(ctx, pgUUIDs(runIDs))
	if err != nil {
		return nil, cancellationAuthority("lock cancellation run leases", err)
	}
	return cancellationIDs(rows), nil
}

func lockCancellationWaits(
	ctx context.Context,
	tx pgx.Tx,
	runIDs []uuid.UUID,
	cancelIDs []uuid.UUID,
) (map[uuid.UUID]cancellationWait, error) {
	rows, err := db.New(tx).LockCancellationWaits(ctx, db.LockCancellationWaitsParams{
		RunIDs:    pgUUIDs(runIDs),
		CancelIDs: pgUUIDs(cancelIDs),
	})
	if err != nil {
		return nil, cancellationAuthority("lock cancellation waits", err)
	}
	waitsByChild := make(map[uuid.UUID]cancellationWait)
	for _, row := range rows {
		wait := cancellationWait{
			id:                  uuid.UUID(row.ID.Bytes),
			runID:               uuid.UUID(row.RunID.Bytes),
			computerID:          uuid.UUID(row.ComputerID.Bytes),
			childRunID:          row.ChildRunID,
			conditionStatus:     row.ConditionStatus,
			suspensionStatus:    row.SuspensionStatus,
			expectedRunRevision: row.ExpectedRunRevision,
			attemptNumber:       row.AttemptNumber,
			currentRunLeaseID:   row.CurrentRunLeaseID,
			priorRunLeaseID:     row.PriorRunLeaseID,
			suspendCheckpointID: row.SuspendCheckpointID,
		}
		if wait.childRunID.Valid {
			childID := uuid.UUID(wait.childRunID.Bytes)
			if _, duplicate := waitsByChild[childID]; duplicate {
				return nil, cancellationAuthority(
					"multiple active parent waits name one child run",
					nil,
				)
			}
			waitsByChild[childID] = wait
		}
	}
	return waitsByChild, nil
}

func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	values := make([]pgtype.UUID, len(ids))
	for index, id := range ids {
		values[index] = pgvalue.UUID(id)
	}
	return values
}

func cancellationIDs(values []pgtype.UUID) []uuid.UUID {
	ids := make([]uuid.UUID, len(values))
	for index, value := range values {
		ids[index] = uuid.UUID(value.Bytes)
	}
	return ids
}

func validateCancellationBoundary(
	parent cancellationRun,
	child cancellationRun,
	wait cancellationWait,
) error {
	if wait.runID != parent.id ||
		!wait.childRunID.Valid ||
		uuid.UUID(wait.childRunID.Bytes) != child.id {
		return cancellationAuthority(
			"cancelled child wait relation does not match",
			nil,
		)
	}
	if wait.computerID != parent.computerID {
		return cancellationAuthority(
			"cancelled child wait computer does not match parent",
			nil,
		)
	}
	return nil
}

func cancelLockedRun(
	ctx context.Context,
	tx pgx.Tx,
	run cancellationRun,
) error {
	return terminateLockedRun(ctx, tx, run, cancelledTermination)
}

func terminateLockedRun(
	ctx context.Context,
	tx pgx.Tx,
	run cancellationRun,
	termination termination,
) error {
	if runStatusTerminal(run.status) {
		return nil
	}
	errorPayload, err := json.Marshal(map[string]any{
		"code":      termination.errorCode,
		"message":   termination.errorMessage,
		"retryable": false,
	})
	if err != nil {
		return err
	}
	failure, err := MarshalFailure(termination.errorCode, termination.errorMessage, nil)
	if err != nil {
		return err
	}
	queries := db.New(tx)
	if run.actorID.Valid {
		actor, err := queries.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: pgvalue.UUID(run.environmentID), ID: run.actorID})
		if err != nil {
			return err
		}
		if actor.CurrentRunID != pgvalue.UUID(run.id) {
			return cancellationAuthority("stale Actor termination", nil)
		}
		// Explicit cancellation already has a durable stop hold. Keep its
		// receipt address while retiring the execution; loss/failure establishes
		// a new recovery hold because the reason and execution certainty changed.
		if termination.runStatus != db.RunStatusCancelled || !actor.DispatchHoldID.Valid {
			if _, err := HoldSessionExecution(ctx, queries, actor, run.currentAttemptNumber, "recovery_required"); err != nil {
				return err
			}
		}
	}
	if err := retireRunAttempt(ctx, tx, run, termination, errorPayload); err != nil {
		return err
	}
	affected, err := queries.TerminalizeRun(
		ctx,
		db.TerminalizeRunParams{
			Status:           termination.runStatus,
			Failure:          failure,
			ID:               pgvalue.UUID(run.id),
			ExpectedRevision: run.revision,
		},
	)
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize run", err)
	}

	if err := queries.RecordRunTerminalEvent(
		ctx,
		db.RecordRunTerminalEventParams{
			RunLeaseID: run.currentRunLeaseID,
			Kind:       termination.eventKind,
			Message:    termination.eventMessage,
			ReasonCode: termination.reasonCode,
			RunID:      pgvalue.UUID(run.id),
		},
	); err != nil {
		return cancellationAuthority("record run terminal event", err)
	}
	return nil
}

// Retiring execution does not terminate the logical Run or release its Computer.
// Retry and final termination share the same fencing and continuation invalidation.
func retireRunExecution(ctx context.Context, tx pgx.Tx, run cancellationRun, termination termination, errorPayload []byte) error {
	queries := db.New(tx)

	if err := queries.TerminalizeRunSuspensions(
		ctx,
		db.TerminalizeRunSuspensionsParams{
			ConditionStatus:  termination.waitCondition,
			ErrorPayload:     errorPayload,
			ReasonCode:       termination.reasonCode,
			SuspensionStatus: termination.waitSuspension,
			RunID:            pgvalue.UUID(run.id),
		},
	); err != nil {
		return cancellationAuthority("terminalize run suspension", err)
	}
	if run.currentRunLeaseID.Valid {
		affected, err := queries.TerminalizeRunLease(
			ctx,
			db.TerminalizeRunLeaseParams{
				Status:       termination.runLeaseStatus,
				ReasonCode:   termination.reasonCode,
				ErrorPayload: errorPayload,
				ID:           run.currentRunLeaseID,
				RunID:        pgvalue.UUID(run.id),
			},
		)
		if err != nil || affected != 1 {
			return cancellationAuthority("terminalize current run lease", err)
		}
	}
	return nil
}

func retireRunAttempt(ctx context.Context, tx pgx.Tx, run cancellationRun, termination termination, errorPayload []byte) error {
	if err := retireRunExecution(ctx, tx, run, termination, errorPayload); err != nil {
		return err
	}
	queries := db.New(tx)
	affected, err := queries.TerminalizeRunAttempt(
		ctx,
		db.TerminalizeRunAttemptParams{
			Outcome:       termination.attemptOutcome,
			ReasonCode:    termination.reasonCode,
			ErrorPayload:  errorPayload,
			RunID:         pgvalue.UUID(run.id),
			AttemptNumber: run.currentAttemptNumber,
		},
	)
	if err != nil || affected != 1 {
		return cancellationAuthority("terminalize current run attempt", err)
	}
	return nil
}

func resolveCancelledChildWait(ctx context.Context, tx pgx.Tx, parent, child cancellationRun, wait cancellationWait) error {
	if wait.conditionStatus != db.WaitStatusPending || parent.status != db.RunStatusWaiting ||
		parent.currentAttemptNumber != wait.attemptNumber || parent.revision != wait.expectedRunRevision {
		return cancellationAuthority("cancelled child wait fence does not match", nil)
	}
	return resolveCancelledChildResult(ctx, tx, parent, child, wait)
}

func resolveCancelledChildResult(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	child cancellationRun,
	wait cancellationWait,
) error {
	result, err := marshalChildFailureResult(
		child.id,
		"child_run_cancelled",
		"Child Run was cancelled",
	)
	if err != nil {
		return err
	}
	return resolveChildResult(ctx, tx, parent, wait, result)
}

func resolveChildResult(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	wait cancellationWait,
	result json.RawMessage,
) error {
	return resolveTerminalChildWait(
		ctx,
		tx,
		parent,
		wait,
		terminalChildWaitResolution{
			conditionStatus: db.WaitStatusCompleted,
			result:          result,
		},
	)
}

func resolveTerminalChildWait(
	ctx context.Context,
	tx pgx.Tx,
	parent cancellationRun,
	wait cancellationWait,
	resolution terminalChildWaitResolution,
) error {
	queries := db.New(tx)
	switch wait.suspensionStatus {
	case db.RunWaitStatusHot:
		if !wait.currentRunLeaseID.Valid ||
			!parent.currentRunLeaseID.Valid ||
			wait.currentRunLeaseID != parent.currentRunLeaseID {
			return cancellationAuthority("hot terminal child wait lease does not match", nil)
		}
		if _, err := queries.ResolveHotTerminalChildWait(
			ctx,
			db.ResolveHotTerminalChildWaitParams{
				ConditionStatus:     string(resolution.conditionStatus),
				ConditionResult:     resolution.result,
				ConditionError:      resolution.conditionError,
				ReasonCode:          pgvalue.TextPtr(resolution.reasonCode),
				WaitID:              pgvalue.UUID(wait.id),
				RunID:               pgvalue.UUID(parent.id),
				ExpectedRunRevision: parent.revision,
				AttemptNumber:       parent.currentAttemptNumber,
				CurrentRunLeaseID:   parent.currentRunLeaseID,
			},
		); err != nil {
			return cancellationAuthority("resolve hot terminal child wait", err)
		}
	case db.RunWaitStatusCheckpointing:
		if _, err := queries.ResolveCheckpointingTerminalChildWait(
			ctx,
			db.ResolveCheckpointingTerminalChildWaitParams{
				ConditionStatus: string(resolution.conditionStatus),
				ConditionResult: resolution.result,
				ConditionError:  resolution.conditionError,
				ReasonCode:      pgvalue.TextPtr(resolution.reasonCode),
				WaitID:          pgvalue.UUID(wait.id),
				RunID:           pgvalue.UUID(parent.id),
			},
		); err != nil {
			return cancellationAuthority("resolve checkpointing terminal child wait", err)
		}
	case db.RunWaitStatusResuming:
		if _, err := queries.ResolveResumingRunWait(ctx, db.ResolveResumingRunWaitParams{
			WaitID: pgvalue.UUID(wait.id), RunID: pgvalue.UUID(parent.id), ExpectedRunRevision: parent.revision,
			ConditionStatus: string(resolution.conditionStatus), ConditionResult: resolution.result,
			ConditionError: resolution.conditionError, ReasonCode: pgvalue.TextPtr(resolution.reasonCode),
		}); err != nil {
			return cancellationAuthority("resolve restoring terminal child wait", err)
		}
	case db.RunWaitStatusParked:
		if !wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			parent.currentRunLeaseID.Valid {
			return cancellationAuthority("parked terminal child wait fence does not match", nil)
		}
		_, err := queries.ResolveParkedTerminalChildWait(
			ctx,
			db.ResolveParkedTerminalChildWaitParams{
				ConditionStatus:     string(resolution.conditionStatus),
				ConditionResult:     resolution.result,
				ConditionError:      resolution.conditionError,
				ReasonCode:          pgvalue.TextPtr(resolution.reasonCode),
				WaitID:              pgvalue.UUID(wait.id),
				RunID:               pgvalue.UUID(parent.id),
				ExpectedRunRevision: parent.revision,
				AttemptNumber:       parent.currentAttemptNumber,
			},
		)
		if err != nil {
			return cancellationAuthority("resolve parked terminal child wait", err)
		}
	default:
		return cancellationAuthority("terminal child wait suspension is ineligible", nil)
	}
	return nil
}

func runStatusTerminal(status db.RunStatus) bool {
	switch status {
	case db.RunStatusSucceeded, db.RunStatusFailed, db.RunStatusCancelled,
		db.RunStatusExpired, db.RunStatusSystemFailed:
		return true
	default:
		return false
	}
}

func cancellationAuthority(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrCancellationAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrCancellationAuthority, operation, cause)
}
