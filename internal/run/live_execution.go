package run

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// A live operation that delivers its attempt's Secrets runs a staged
// prologue in its transaction: LocateLiveExecution, then the caller's own
// checks on the located lease, then LiveLocator.LockSecrets, then
// LiveSecrets.LockExecution. No stage rewrites another stage's error, so each
// operation keeps its own outcome per phase.

// LiveLocator is the unlocked location of a live Run lease that
// LocateLiveExecution read in the caller's transaction.
type LiveLocator struct {
	tx      pgx.Tx
	fence   ExecutionFence
	located db.GetLiveRunLeaseLocatorsRow
}

// LocateLiveExecution reads, without locking, where the fenced live lease
// runs. A lease the fence does not address is pgx.ErrNoRows.
func LocateLiveExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (LiveLocator, error) {
	located, err := db.New(tx).GetLiveRunLeaseLocators(ctx, fence.liveLocators())
	if err != nil {
		return LiveLocator{}, err
	}
	return LiveLocator{tx: tx, fence: fence, located: located}, nil
}

// EnvironmentID is the located Run's environment.
func (l LiveLocator) EnvironmentID() pgtype.UUID { return l.located.EnvironmentID }

// RunID is the located Run.
func (l LiveLocator) RunID() pgtype.UUID { return l.located.RunID }

// AttemptNumber is the located lease's attempt.
func (l LiveLocator) AttemptNumber() int32 { return l.located.AttemptNumber }

// SessionID is the located Run's Session; it is invalid for a Task Run.
func (l LiveLocator) SessionID() pgtype.UUID { return l.located.SessionID }

// LiveSecrets is a located live lease whose attempt's Secret deliveries are
// locked.
type LiveSecrets struct {
	locator LiveLocator
	secrets []secret.DeliveryEnvelope
}

// LockSecrets locks the located attempt's Secret deliveries. Its errors are
// secret.LockAttemptDelivery's.
func (l LiveLocator) LockSecrets(ctx context.Context) (LiveSecrets, error) {
	secrets, err := secret.LockAttemptDelivery(ctx, db.New(l.tx), l.located.RunID, l.located.AttemptNumber, l.located.ComputerID)
	if err != nil {
		return LiveSecrets{}, err
	}
	return LiveSecrets{locator: l, secrets: secrets}, nil
}

// LockExecution locks the fenced execution as LockLiveExecution does, with
// the Secret deliveries already locked.
func (s LiveSecrets) LockExecution(ctx context.Context) (Execution, error) {
	execution, err := LockLiveExecution(ctx, s.locator.tx, s.locator.fence)
	if err != nil {
		return Execution{}, err
	}
	execution.secrets = s.secrets
	return execution, nil
}

// lockOwnedFinalization locks the located Run's owned finalization graph,
// fencing the execution host before the graph's Instances.
func (l LiveLocator) lockOwnedFinalization(ctx context.Context) (OwnedFinalization, error) {
	loc := l.located
	return LockOwnedFinalizationWithInstanceFence(ctx, l.tx, OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID), RunID: pgvalue.MustUUIDValue(loc.RunID)}, func() error {
		return workergroup.LockExecutionHost(ctx, db.New(l.tx), l.fence.host(loc.RegionID, false))
	})
}

func (f ExecutionFence) liveLocators() db.GetLiveRunLeaseLocatorsParams {
	return db.GetLiveRunLeaseLocatorsParams{ID: f.LeaseID, LeaseSequence: f.LeaseSequence, WorkerGroupID: f.WorkerGroupID, WorkerHostID: f.WorkerHostID, WorkerEpoch: f.WorkerEpoch}
}
