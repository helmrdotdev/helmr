package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// InputWait registers a worker's Actor input wait: its Session's next input
// after the cursor, with the wait's identity, normalized annotations,
// deadlines and the fingerprint of the worker's normalized request.
type InputWait struct {
	WaitID             uuid.UUID
	SessionID          uuid.UUID
	AfterInputSequence int64
	Fingerprint        string
	Metadata           []byte
	Tags               []string
	TimeoutAt          pgtype.Timestamptz
	IdleTimeout        pgtype.Int8
}

// RegisterInputWait registers the Actor input wait of the worker's live
// execution, or replays its registration, in one transaction. It locates the
// lease, locks the attempt's Secret deliveries and then the running, entered,
// non-finalizing execution, which must be the Actor execution of the
// addressed Session. A replay with the same fingerprint validates the wait
// cursor; a new wait requires the cursor at the Session's committed input
// with no active Turn or hold, registers the wait, and then locks the
// Session's Turn at the next input sequence: it completes the wait with that
// Turn, fails it when the closing Session has committed its close sequence,
// or leaves it pending. A stale execution or a wait already registered with
// a different request is ErrStaleExecution; stale cursors, stopped Turns and
// Turn scopes are run's errors, inconsistent Session input is ErrAuthority,
// and stale worker claims are workergroup.ErrStaleClaims.
func RegisterInputWait(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, wait InputWait) (db.RunWait, error) {
	var registered db.RunWait
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		authority, err := lockWaitExecution(ctx, tx, fence)
		if err != nil {
			return err
		}
		if authority.Run().EntrypointKind != "actor" || authority.Run().SessionID != pgvalue.UUID(wait.SessionID) {
			return ErrStaleExecution
		}
		cursor := pgtype.Int8{Int64: wait.AfterInputSequence, Valid: true}
		replayParams := db.GetSessionInputRunWaitRegistrationReplayParams{
			ID: pgvalue.UUID(wait.WaitID), EnvironmentID: authority.Run().EnvironmentID, RunID: authority.Run().ID,
			ComputerID: authority.Computer().ID, SessionID: authority.Session().ID,
			AfterInputSequence:             cursor,
			AttemptNumber:                  authority.Attempt().Number,
			RegistrationRequestFingerprint: pgvalue.Text(wait.Fingerprint), Metadata: wait.Metadata, Tags: wait.Tags,
			RunLeaseID: authority.Lease().ID,
		}
		registered, err = q.GetSessionInputRunWaitRegistrationReplay(ctx, replayParams)
		if err == nil {
			return authority.ValidateWaitCursor(registered, cursor)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, existingErr := q.GetRunWait(ctx, db.GetRunWaitParams{
			RunID: authority.Run().ID, AttemptNumber: authority.Attempt().Number, ID: pgvalue.UUID(wait.WaitID),
		}); existingErr == nil || !errors.Is(existingErr, pgx.ErrNoRows) {
			return ErrStaleExecution
		}
		if err := authority.ValidateWaitCursor(db.RunWait{Kind: db.WaitKindSessionInput}, cursor); err != nil {
			return err
		}
		if authority.Run().Status != db.RunStatusRunning || authority.Session().ActiveTurnID.Valid || authority.Session().DispatchHoldID.Valid || wait.AfterInputSequence != authority.Session().CommittedInputSequence {
			return ErrStaleExecution
		}
		registered, err = q.RegisterSessionInputRunWait(ctx, db.RegisterSessionInputRunWaitParams{
			ID: pgvalue.UUID(wait.WaitID), EnvironmentID: authority.Run().EnvironmentID, TimeoutAt: wait.TimeoutAt,
			IdleTimeoutMs: wait.IdleTimeout, SessionID: authority.Session().ID, AfterInputSequence: cursor,
			RegistrationRequestFingerprint: pgvalue.Text(wait.Fingerprint), AttemptNumber: authority.Attempt().Number,
			CurrentRunLeaseID: authority.Lease().ID,
			Metadata:          wait.Metadata, Tags: wait.Tags,
			RunID: authority.Run().ID, ExpectedRunningRevision: authority.Run().Revision,
		})
		if err != nil {
			return staleExecution(err)
		}
		record, err := q.GetSessionTurnAtSequenceForUpdate(ctx, db.GetSessionTurnAtSequenceForUpdateParams{
			EnvironmentID: authority.Run().EnvironmentID, SessionID: authority.Session().ID,
			Sequence: wait.AfterInputSequence + 1,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if authority.Session().Status == "closing" && authority.Session().CloseSequence.Valid &&
				wait.AfterInputSequence >= authority.Session().CloseSequence.Int64 {
				registered, err = FailWait(ctx, tx, registered, "session_closed")
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		registered, err = CompleteWait(ctx, tx, registered, record)
		return err
	})
	return registered, err
}

// lockWaitExecution runs the staged live prologue for a worker's wait: it
// locates the lease, locks the attempt's Secret deliveries and then the
// execution, which must be running, entered and not finalizing. A lease that
// addresses no such execution is ErrStaleExecution.
func lockWaitExecution(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence) (run.Execution, error) {
	locator, err := run.LocateLiveExecution(ctx, tx, fence)
	if err != nil {
		return run.Execution{}, staleExecution(err)
	}
	secrets, err := locator.LockSecrets(ctx)
	if err != nil {
		return run.Execution{}, err
	}
	a, err := secrets.LockExecution(ctx)
	if err != nil {
		return run.Execution{}, staleExecution(err)
	}
	if a.Lease().Status != db.RunLeaseStatusRunning || !a.Attempt().EntrypointEnteredAt.Valid || a.Lease().FinalizationOperationID.Valid {
		return run.Execution{}, ErrStaleExecution
	}
	return a, nil
}
