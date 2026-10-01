package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// maxTurnOutputBytes bounds a worker's canonical Turn output.
const maxTurnOutputBytes = 1 << 20

var (
	// ErrOutputTooLarge reports Turn output larger than the maximum size.
	ErrOutputTooLarge = errors.New("actor output exceeds the maximum size")
	// ErrStaleTurnCommit reports a Turn commit whose execution, Turn or input
	// cursor no longer admits it, or whose replay differs.
	ErrStaleTurnCommit = errors.New("actor turn commit is stale")
)

// LeaseLocators reads the unlocked locators of a live Run lease.
type LeaseLocators interface {
	GetLiveRunLeaseLocators(context.Context, db.GetLiveRunLeaseLocatorsParams) (db.GetLiveRunLeaseLocatorsRow, error)
}

// TurnOutput is a worker's output on a Turn of its live Actor execution,
// with canonical data. A MessageDeliveryID of uuid.Nil writes outside a
// message callback; an empty idempotency key defaults to the correlation ID.
type TurnOutput struct {
	TurnID            uuid.UUID
	RunGeneration     int64
	MessageDeliveryID uuid.UUID
	CorrelationID     uuid.UUID
	IdempotencyKey    string
	Data              json.RawMessage
}

// AppendTurnOutputFromRun appends a worker's Turn output, as
// AppendTurnOutput does, from its live Actor execution. Oversized data is
// ErrOutputTooLarge. It reads the lease's locators from locators, outside
// the transaction,
// then in one transaction locates the lease again and requires the same
// environment and Session, locks the attempt's Secret deliveries and the
// execution, requires the execution to be the running, entered,
// non-finalizing top-level Actor execution of its open or closing Session's
// current Run, appends the output and locks the live execution again. A lost
// or changed execution is ErrStaleOutput; stale worker claims are
// workergroup.ErrStaleClaims. A rejection commits with its receipt and is
// returned as an *OperationError.
func AppendTurnOutputFromRun(ctx context.Context, txb db.TxBeginner, locators LeaseLocators, fence run.ExecutionFence, request TurnOutput) (Output, error) {
	if len(request.Data) > maxTurnOutputBytes {
		return Output{}, ErrOutputTooLarge
	}
	discovered, err := locators.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: fence.LeaseID, LeaseSequence: fence.LeaseSequence,
		WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID, WorkerEpoch: fence.WorkerEpoch,
	})
	if err != nil || !discovered.SessionID.Valid {
		return Output{}, staleOutput(err)
	}
	environmentID, err := pgvalue.UUIDValue(discovered.EnvironmentID)
	if err != nil {
		return Output{}, ErrStaleOutput
	}
	actorID, err := pgvalue.UUIDValue(discovered.SessionID)
	if err != nil {
		return Output{}, ErrStaleOutput
	}
	var output Output
	var rejected error
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		locator, err := run.LocateLiveExecution(ctx, tx, fence)
		if err != nil ||
			locator.EnvironmentID() != discovered.EnvironmentID ||
			locator.SessionID() != discovered.SessionID {
			return staleOutput(err)
		}
		secrets, err := locator.LockSecrets(ctx)
		if err != nil {
			return fmt.Errorf("lock actor output secret authority: %w", err)
		}
		authority, err := secrets.LockExecution(ctx)
		if err != nil || !authority.Session().ID.Valid {
			return staleOutput(err)
		}

		if authority.Run().ParentRunID.Valid ||
			authority.Run().EntrypointKind != "actor" ||
			authority.Run().SessionID != authority.Session().ID ||
			authority.Session().CurrentRunID != authority.Run().ID ||
			(authority.Session().Status != "open" && authority.Session().Status != "closing") ||
			authority.Run().Status != db.RunStatusRunning ||
			authority.Lease().Status != db.RunLeaseStatusRunning ||
			!authority.Run().ActiveStartedAt.Valid ||
			!authority.Attempt().EntrypointEnteredAt.Valid ||
			authority.Attempt().TerminalAt.Valid ||
			authority.Lease().FinalizationOperationID.Valid {
			return ErrStaleOutput
		}
		key := request.IdempotencyKey
		if key == "" {
			key = request.CorrelationID.String()
		}
		receipt, err := AppendTurnOutput(ctx, tx, run.TurnScope{
			EnvironmentID: environmentID, SessionID: actorID, TurnID: request.TurnID,
			RunID: pgvalue.MustUUIDValue(authority.Run().ID), AttemptNumber: authority.Attempt().Number, RunGeneration: request.RunGeneration, MessageDeliveryID: request.MessageDeliveryID,
		}, key, request.Data)
		if err != nil {
			return err
		}
		if receipt.Code != "" {
			rejected = &OperationError{Code: receipt.Code}
			return nil
		}
		output = Output{event: receipt.Event, deploymentID: authority.Run().DeploymentID}
		if _, err = run.LockLiveExecution(ctx, tx, fence); err != nil {
			return staleOutput(err)
		}
		return nil
	})
	if err == nil {
		err = rejected
	}
	return output, err
}

// TurnCommit settles a Turn of a worker's live Actor execution with a
// completed result or a failed error, to the target input sequence. The
// fingerprint identifies the worker's normalized request for replay.
type TurnCommit struct {
	TurnID              uuid.UUID
	RunGeneration       int64
	Disposition         string
	Result              json.RawMessage
	Fingerprint         string
	TargetInputSequence int64
}

// CommitTurnFromRun settles the Turn and returns its terminal event. In one
// transaction it locates the lease, locks the attempt's Secret deliveries
// and the execution, requires the running, entered top-level Actor
// execution of its open or closing Session's current Run with a clear
// finalization scope, and then either replays a commit the Session already
// committed with the same fingerprint, or validates the Turn and the input
// cursor and settles the Turn as SettleTurn does before both the lease's and
// the Instance writer's expiry, locking the live execution again at the end.
// A commit that is stale, differs from its replay or whose Turn was stopped
// or rejected is ErrStaleTurnCommit, joined with the rejection's
// *OperationError; stale worker claims are workergroup.ErrStaleClaims.
func CommitTurnFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, commit TurnCommit) (pgtype.UUID, error) {
	var eventID pgtype.UUID
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		locator, err := run.LocateLiveExecution(ctx, tx, fence)
		if err != nil {
			return staleTurnCommit(err)
		}
		secrets, err := locator.LockSecrets(ctx)
		if err != nil {
			return fmt.Errorf("lock actor turn secret authority: %w", err)
		}
		authority, err := secrets.LockExecution(ctx)
		runRow, attempt, actor, lease, instance := authority.Run(), authority.Attempt(), authority.Session(), authority.Lease(), authority.Instance()
		if err != nil || !actor.ID.Valid {
			return staleTurnCommit(err)
		}

		if err := validateTurnCommitAuthority(ctx, q, authority); err != nil {
			return err
		}

		if actor.CommittedInputSequence == commit.TargetInputSequence {
			var replayed bool
			eventID, replayed, err = replayTurnCommit(ctx, q, commit, authority)
			if err != nil {
				return err
			}
			if replayed {
				return nil
			}
			return ErrStaleTurnCommit
		}
		scope := run.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(runRow.EnvironmentID), SessionID: pgvalue.MustUUIDValue(actor.ID), TurnID: commit.TurnID, RunID: pgvalue.MustUUIDValue(runRow.ID), AttemptNumber: attempt.Number, RunGeneration: commit.RunGeneration}
		input, err := run.ValidateTurn(ctx, tx, scope)
		if err != nil {
			return staleTurnCommit(err)
		}
		if input.Sequence != commit.TargetInputSequence {
			return ErrStaleTurnCommit
		}
		if actor.CommittedInputSequence+1 != commit.TargetInputSequence ||
			commit.TargetInputSequence >= actor.NextInputSequence {
			return ErrStaleTurnCommit
		}
		committedAt, err := q.GetTaskCompletionTime(ctx)
		if err != nil || !committedAt.Valid {
			if err == nil {
				err = errors.New("database actor turn commit time is unavailable")
			}
			return err
		}
		if !committedAt.Time.Before(lease.ExpiresAt.Time) ||
			!committedAt.Time.Before(instance.WriterExpiresAt.Time) {
			return ErrStaleTurnCommit
		}
		event, err := SettleTurn(ctx, tx, scope, commit.Disposition, commit.Result, commit.Fingerprint)
		if err != nil {
			return staleTurnCommit(err)
		}
		eventID = event.ID
		_, err = run.LockLiveExecution(ctx, tx, fence)
		if err != nil {
			return staleTurnCommit(err)
		}
		return nil
	})
	return eventID, err
}

func validateTurnCommitAuthority(ctx context.Context, store db.Querier, authority run.Execution) error {
	runRow, attempt, actor, lease, computerRow := authority.Run(), authority.Attempt(), authority.Session(), authority.Lease(), authority.Computer()
	if runRow.Status != db.RunStatusRunning || runRow.EntrypointKind != "actor" || !runRow.SessionID.Valid ||
		runRow.SessionID != actor.ID || runRow.ParentRunID.Valid ||
		runRow.ParentOwnsLifecycle.Valid || lease.Status != db.RunLeaseStatusRunning ||
		!runRow.ActiveStartedAt.Valid || !attempt.EntrypointEnteredAt.Valid ||
		attempt.TerminalAt.Valid || !attempt.SessionInputStartSequence.Valid ||
		!runRow.SessionInputStartSequence.Valid || !runRow.SessionInputHighWatermark.Valid ||
		!actor.CurrentRunID.Valid || actor.CurrentRunID != runRow.ID ||
		(actor.Status != "open" && actor.Status != "closing") ||
		!computerRow.HeadDiskVersionID.Valid ||
		lease.FinalizationOperationID.Valid ||
		lease.FinalizationStartedAt.Valid || lease.FinalizationRequestFingerprint.Valid {
		return ErrStaleTurnCommit
	}
	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{
		RunID: runRow.ID, AttemptNumber: attempt.Number, ComputerID: computerRow.ID,
	})
	if err != nil {
		return err
	}
	if !clear {
		return ErrStaleTurnCommit
	}
	return nil
}

// replayTurnCommit returns the terminal event of a Turn the execution
// already settled with the same disposition and fingerprint, or false.
func replayTurnCommit(ctx context.Context, store db.Querier, commit TurnCommit, authority run.Execution) (pgtype.UUID, bool, error) {
	runRow, attempt, actor := authority.Run(), authority.Attempt(), authority.Session()
	input, err := store.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: runRow.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(commit.TurnID)})
	if err != nil {
		return pgtype.UUID{}, false, staleTurnCommit(err)
	}
	if input.Status != commit.Disposition || input.TerminalRequestFingerprint.String != commit.Fingerprint || input.RunID != runRow.ID || input.AttemptNumber.Int32 != attempt.Number || input.RunGeneration.Int64 != commit.RunGeneration || !input.TerminalEventID.Valid {
		return pgtype.UUID{}, false, nil
	}
	return input.TerminalEventID, true, nil
}

// staleTurnCommit reports a rejected, stopped, inactive, out-of-scope or
// lost Turn commit as ErrStaleTurnCommit, joined with a rejection's
// *OperationError. Stale worker claims and other errors are returned
// unchanged.
func staleTurnCommit(err error) error {
	var operation *OperationError
	if errors.As(err, &operation) {
		return errors.Join(ErrStaleTurnCommit, err)
	}
	if errors.Is(err, workergroup.ErrStaleClaims) {
		return err
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrStaleExecution) ||
		errors.Is(err, ErrStaleCompletion) ||
		errors.Is(err, run.ErrTurnStopped) || errors.Is(err, run.ErrTurnNotActive) || errors.Is(err, run.ErrTurnScope) {
		return ErrStaleTurnCommit
	}
	return err
}
