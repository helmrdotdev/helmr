package run

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TimerWait is a worker's request to register a timer wait on its live
// execution.
type TimerWait struct {
	Fence  ExecutionFence
	WaitID uuid.UUID
	// TurnID and RunGeneration are the Turn the worker names, parsed after
	// the execution is locked.
	TurnID        *string
	RunGeneration *int64
	// Cursor is an Actor execution's speculative input sequence.
	Cursor pgtype.Int8
	// Fingerprint identifies the registration request for its replay.
	Fingerprint string
	DueAt       time.Time
	IdleTimeout pgtype.Int8
	Metadata    json.RawMessage
	Tags        []string
}

// RegisterTimerWait registers, or replays, a timer wait in its own
// transaction. It runs the live prologue with the attempt's Secrets
// (LocateLiveExecution, LockSecrets, LockExecution), checks the wait's input
// cursor and Turn, replays a registration with the same fingerprint, and
// otherwise registers the wait and binds it to its Turn. A receipt that no
// longer addresses a running, entered execution, an existing different wait
// or a Run that is not running is ErrStale; cursor and Turn rejections are
// ErrWaitCursor, ErrTurnStopped, ErrTurnScope or the Turn's own validation
// error.
func RegisterTimerWait(ctx context.Context, txb db.TxBeginner, wait TimerWait) (db.RunWait, error) {
	var registered db.RunWait
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		authority, err := lockLiveWaitExecution(ctx, tx, wait.Fence)
		if err != nil {
			return err
		}
		turnID, generation, err := ParseWaitTurn(wait.TurnID, wait.RunGeneration)
		if err != nil {
			return err
		}
		if err := authority.ValidateWaitCursor(db.RunWait{TurnID: turnID, TurnRunGeneration: generation, TurnSessionID: authority.session.ID}, wait.Cursor); err != nil {
			return err
		}
		if turnID.Valid {
			if err := authority.ValidateTurnWork(ctx, tx, pgvalue.MustUUIDValue(turnID), generation.Int64); err != nil {
				return err
			}
		}
		r, attempt, lease := authority.run, authority.attempt, authority.lease
		registered, err = q.GetTimerRunWaitRegistrationReplay(ctx, db.GetTimerRunWaitRegistrationReplayParams{
			ID: pgvalue.UUID(wait.WaitID), EnvironmentID: r.EnvironmentID,
			RunID: r.ID, ComputerID: authority.Computer().ID,
			AttemptNumber:                  attempt.Number,
			RegistrationRequestFingerprint: pgvalue.Text(wait.Fingerprint),
			Metadata:                       wait.Metadata, Tags: wait.Tags, RunLeaseID: lease.ID,
		})
		if err == nil {
			return authority.ValidateWaitCursor(registered, wait.Cursor)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, existingErr := q.GetRunWait(ctx, db.GetRunWaitParams{
			RunID: r.ID, AttemptNumber: attempt.Number, ID: pgvalue.UUID(wait.WaitID),
		}); existingErr == nil || !errors.Is(existingErr, pgx.ErrNoRows) {
			return ErrStale
		}
		if r.Status != db.RunStatusRunning {
			return ErrStale
		}
		registered, err = q.RegisterTimerRunWait(ctx, db.RegisterTimerRunWaitParams{
			ID: pgvalue.UUID(wait.WaitID), EnvironmentID: r.EnvironmentID,
			DueAt: pgvalue.Timestamptz(wait.DueAt), IdleTimeoutMs: wait.IdleTimeout,
			RegistrationRequestFingerprint: pgvalue.Text(wait.Fingerprint),
			AttemptNumber:                  attempt.Number,
			CurrentRunLeaseID:              lease.ID,
			Metadata:                       wait.Metadata, Tags: wait.Tags,
			RunID:                   r.ID,
			ExpectedRunningRevision: r.Revision,
		})
		if err != nil {
			return stale(err)
		}
		if turnID.Valid {
			_, err = q.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: authority.session.ID, TurnID: turnID, RunGeneration: generation, WaitID: registered.ID})
		}
		return err
	})
	if err != nil {
		return db.RunWait{}, err
	}
	return registered, nil
}

// lockLiveWaitExecution runs the live prologue with Secrets for a wait
// registration and requires a running lease whose attempt entered its
// entrypoint and has not begun finalization; otherwise it is ErrStale.
func lockLiveWaitExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (Execution, error) {
	locator, err := LocateLiveExecution(ctx, tx, fence)
	if err != nil {
		return Execution{}, stale(err)
	}
	secrets, err := locator.LockSecrets(ctx)
	if err != nil {
		return Execution{}, err
	}
	execution, err := secrets.LockExecution(ctx)
	if err != nil {
		return Execution{}, stale(err)
	}
	if execution.lease.Status != db.RunLeaseStatusRunning || !execution.attempt.EntrypointEnteredAt.Valid || execution.lease.FinalizationOperationID.Valid {
		return Execution{}, ErrStale
	}
	return execution, nil
}
