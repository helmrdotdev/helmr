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
	GetRunWaitPoll(context.Context, db.GetRunWaitPollParams) (db.GetRunWaitPollRow, error)
}

// WaitPollScope is the located live lease a wait is polled under.
type WaitPollScope struct {
	RunID         pgtype.UUID
	AttemptNumber int32
	ComputerID    pgtype.UUID
	LeaseID       pgtype.UUID
}

// PollWait reads the wait and its Session/Turn authority in one snapshot
// under the located lease. A released wait whose Session was stopped returns stopped
// without the Turn check; otherwise the wait's Turn must still hold it.
func PollWait(ctx context.Context, store WaitPollStore, scope WaitPollScope, waitID pgtype.UUID) (wait db.RunWait, stopped bool, err error) {
	poll, err := store.GetRunWaitPoll(ctx, db.GetRunWaitPollParams{
		RunID: scope.RunID, AttemptNumber: scope.AttemptNumber, ID: waitID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.RunWait{}, false, ErrWaitNotFound
	}
	if err != nil {
		return db.RunWait{}, false, err
	}
	wait = poll.RunWait
	if wait.AttemptNumber != scope.AttemptNumber ||
		wait.ComputerID != scope.ComputerID ||
		(wait.CurrentRunLeaseID != scope.LeaseID && wait.PriorRunLeaseID != scope.LeaseID) {
		return db.RunWait{}, false, ErrWaitFenceStale
	}
	stopped = poll.Stopped
	if stopped && wait.SuspensionStatus == db.RunWaitStatusReleased {
		return wait, true, nil
	}
	if !poll.Current {
		return db.RunWait{}, false, ErrWaitTurnRevoked
	}
	return wait, false, nil
}
