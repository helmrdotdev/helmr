package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrStaleOutput reports a worker's Actor execution that its receipt no
	// longer locates, or that is no longer the live, entered execution of the
	// Session's current Run.
	ErrStaleOutput = errors.New("actor output append source authority is stale")
	// ErrStaleExecution reports a worker receipt that no longer addresses the
	// live execution an operation locks.
	ErrStaleExecution = errors.New("run lease claim is stale")
	// errTurnReference reports a worker Turn operation whose Turn is not a
	// canonical UUIDv7.
	errTurnReference = errors.New("turn_id must be a canonical UUIDv7")
)

// TurnWork addresses a worker's Turn work on its live Actor execution: the
// Turn and the Run generation it executes the Turn in. A Turn the worker did
// not address canonically is uuid.Nil, which the operation rejects after it
// locks the execution.
type TurnWork struct {
	TurnID        uuid.UUID
	RunGeneration int64
}

// Output is an output event a worker's Actor execution appended, with the
// deployment of the Run that produced it.
type Output struct {
	event        db.SessionEvent
	deploymentID pgtype.UUID
}

// Event is the appended Session event.
func (o Output) Event() db.SessionEvent {
	event := o.event
	event.Data = slices.Clone(event.Data)
	return event
}

// DeploymentID is the deployment of the producing Run.
func (o Output) DeploymentID() pgtype.UUID { return o.deploymentID }

// lockExecution runs the staged live prologue for a worker's Actor
// execution: it locates the lease, locks the attempt's Secret deliveries and
// then the execution. Secret locks precede Computer, Instance and logical
// scope locks. Stop remains observable so admitted callback acknowledgements
// can settle their durable work. A lease the receipt does not locate is
// ErrStaleOutput; a lease that locks no execution is ErrStaleExecution; a
// Task execution, or one that is not an entered, non-finalizing Actor
// execution, is run.ErrTurnScope.
func lockExecution(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence) (run.Execution, error) {
	locator, err := run.LocateLiveExecution(ctx, tx, fence)
	if err != nil {
		return run.Execution{}, staleOutput(err)
	}
	if !locator.SessionID().Valid {
		return run.Execution{}, run.ErrTurnScope
	}
	secrets, err := locator.LockSecrets(ctx)
	if err != nil {
		return run.Execution{}, err
	}
	a, err := secrets.LockExecution(ctx)
	if err != nil {
		return run.Execution{}, staleExecution(err)
	}
	if !a.Session().ID.Valid || a.Run().EntrypointKind != "actor" || !a.Attempt().EntrypointEnteredAt.Valid || a.Lease().FinalizationOperationID.Valid {
		return run.Execution{}, run.ErrTurnScope
	}
	return a, nil
}

// staleOutput reports a lost execution as ErrStaleOutput joined with its
// cause. Stale worker claims are returned unchanged.
func staleOutput(err error) error {
	if errors.Is(err, workergroup.ErrStaleClaims) {
		return err
	}
	if err == nil {
		return ErrStaleOutput
	}
	return errors.Join(ErrStaleOutput, err)
}

// staleExecution reports a receipt that addresses no execution as
// ErrStaleExecution.
func staleExecution(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleExecution
	}
	return err
}

// turnScope is the producer scope of the worker's Turn work on the locked
// execution.
func turnScope(authority run.Execution, work TurnWork) (run.TurnScope, error) {
	if work.TurnID == uuid.Nil() {
		return run.TurnScope{}, errTurnReference
	}
	if work.RunGeneration <= 0 {
		return run.TurnScope{}, run.ErrTurnScope
	}
	return run.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(authority.Session().EnvironmentID), SessionID: pgvalue.MustUUIDValue(authority.Session().ID), TurnID: work.TurnID, RunID: pgvalue.MustUUIDValue(authority.Run().ID), AttemptNumber: authority.Attempt().Number, RunGeneration: work.RunGeneration}, nil
}

// running reports whether the locked execution's Run and lease are running.
func running(authority run.Execution) bool {
	return authority.Run().Status == db.RunStatusRunning && authority.Lease().Status == db.RunLeaseStatusRunning
}

// DeclareMessageReadyFromRun locks the worker's live Actor execution and
// declares its Turn ready for messages, as DeclareMessageReady does, while
// the Run and lease are running.
func DeclareMessageReadyFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, work TurnWork) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		a, err := lockExecution(ctx, tx, fence)
		if err != nil {
			return err
		}
		scope, err := turnScope(a, work)
		if err != nil {
			return err
		}
		if !running(a) {
			return run.ErrTurnScope
		}
		_, err = DeclareMessageReady(ctx, tx, scope, pgvalue.MustUUIDValue(a.Lease().ID))
		return err
	})
}

// BeginSettlementFromRun locks the worker's live Actor execution and begins
// its Turn's settlement, as BeginSettlement does.
func BeginSettlementFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, work TurnWork) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		a, err := lockExecution(ctx, tx, fence)
		if err != nil {
			return err
		}
		scope, err := turnScope(a, work)
		if err != nil {
			return err
		}
		_, err = BeginSettlement(ctx, tx, scope)
		return err
	})
}

// ClaimMessageFromRun locks the worker's live Actor execution and claims the
// Turn's next message for the delivery, as ClaimMessage does. With no
// message to deliver it returns a message whose ID is invalid.
func ClaimMessageFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, work TurnWork, deliveryID uuid.UUID) (db.SessionMessage, error) {
	var message db.SessionMessage
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		a, err := lockExecution(ctx, tx, fence)
		if err != nil {
			return err
		}
		scope, err := turnScope(a, work)
		if err != nil {
			return err
		}
		message, err = ClaimMessage(ctx, tx, scope, pgvalue.MustUUIDValue(a.Lease().ID), deliveryID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return message, err
}

// CompleteMessageFromRun locks the worker's live Actor execution and records
// the delivered message's outcome, as CompleteMessage does.
func CompleteMessageFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, work TurnWork, messageID, deliveryID uuid.UUID, outcome MessageOutcome) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		a, err := lockExecution(ctx, tx, fence)
		if err != nil {
			return err
		}
		scope, err := turnScope(a, work)
		if err != nil {
			return err
		}
		_, err = CompleteMessage(ctx, tx, scope, pgvalue.MustUUIDValue(a.Lease().ID), messageID, deliveryID, outcome)
		return err
	})
}

// AppendSessionOutputFromRun locks the worker's live Actor execution and,
// while its Run and lease are running, appends Session output outside a
// Turn, as AppendSessionOutput does. An empty idempotency key defaults to
// the correlation ID.
func AppendSessionOutputFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, runGeneration int64, key, correlationID string, data json.RawMessage) (Output, error) {
	var output Output
	var rejection string
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		a, err := lockExecution(ctx, tx, fence)
		if err != nil {
			return err
		}
		if !running(a) {
			return run.ErrTurnScope
		}
		if key == "" {
			key = correlationID
		}
		receipt, err := AppendSessionOutput(ctx, tx, run.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(a.Session().EnvironmentID), SessionID: pgvalue.MustUUIDValue(a.Session().ID), RunID: pgvalue.MustUUIDValue(a.Run().ID), AttemptNumber: a.Attempt().Number, RunGeneration: runGeneration}, key, data)
		if err != nil {
			return err
		}
		rejection = receipt.Code
		if rejection == "" {
			output = Output{event: receipt.Event, deploymentID: a.Run().DeploymentID}
		}
		return nil
	})
	if err == nil && rejection != "" {
		err = &OperationError{Code: rejection}
	}
	return output, err
}

// ReadControl reads, without a transaction or lock, the dispatch hold and
// active Turn of the Session whose current execution the worker's fence
// addresses in the Run generation. It is an advisory observation:
// finalization separately locks and proves the exact hold, and polling must
// not contend with shared worker dispatch locks. A fence that addresses no
// such execution is run.ErrTurnScope.
func ReadControl(ctx context.Context, q db.Querier, fence run.ExecutionFence, runGeneration int64) (db.ReadWorkerSessionControlRow, error) {
	state, err := q.ReadWorkerSessionControl(ctx, db.ReadWorkerSessionControlParams{
		RunLeaseID: fence.LeaseID, LeaseSequence: fence.LeaseSequence,
		WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID, WorkerEpoch: fence.WorkerEpoch, RunGeneration: runGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ReadWorkerSessionControlRow{}, run.ErrTurnScope
	}
	return state, err
}
