package run

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrWaitNotFound reports a polled wait the lease's attempt does not have.
	ErrWaitNotFound = errors.New("run wait is not found")
	// ErrWaitFenceStale reports a polled wait that is not on the lease's
	// Computer or not registered under the lease or its predecessor.
	ErrWaitFenceStale = errors.New("run wait fence is stale")
	// ErrWaitTurnRevoked reports a polled wait whose Turn no longer holds it.
	ErrWaitTurnRevoked = errors.New("run wait turn authority was revoked")
)

// WaitPollStore reads a polled wait and its Session and Turn authority.
type WaitPollStore interface {
	GetRunWait(context.Context, db.GetRunWaitParams) (db.RunWait, error)
	RunWaitSessionStopped(context.Context, pgtype.UUID) (bool, error)
	RunWaitTurnCurrent(context.Context, pgtype.UUID) (bool, error)
}

// WaitPollScope is the located live lease a wait is polled under.
type WaitPollScope struct {
	RunID         pgtype.UUID
	AttemptNumber int32
	ComputerID    pgtype.UUID
	LeaseID       pgtype.UUID
}

// PollWait reads, without a transaction, the wait a worker polls under its
// located lease. A released wait whose Session was stopped returns stopped
// without the Turn check; otherwise the wait's Turn must still hold it.
func PollWait(ctx context.Context, store WaitPollStore, scope WaitPollScope, waitID pgtype.UUID) (wait db.RunWait, stopped bool, err error) {
	wait, err = store.GetRunWait(ctx, db.GetRunWaitParams{
		RunID: scope.RunID, AttemptNumber: scope.AttemptNumber, ID: waitID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.RunWait{}, false, ErrWaitNotFound
	}
	if err != nil {
		return db.RunWait{}, false, err
	}
	if wait.AttemptNumber != scope.AttemptNumber ||
		wait.ComputerID != scope.ComputerID ||
		(wait.CurrentRunLeaseID != scope.LeaseID && wait.PriorRunLeaseID != scope.LeaseID) {
		return db.RunWait{}, false, ErrWaitFenceStale
	}
	stopped, err = store.RunWaitSessionStopped(ctx, wait.ID)
	if err != nil {
		return db.RunWait{}, false, err
	}
	if stopped && wait.SuspensionStatus == db.RunWaitStatusReleased {
		return wait, true, nil
	}
	current, err := store.RunWaitTurnCurrent(ctx, wait.ID)
	if err != nil {
		return db.RunWait{}, false, err
	}
	if !current {
		return db.RunWait{}, false, ErrWaitTurnRevoked
	}
	return wait, false, nil
}
