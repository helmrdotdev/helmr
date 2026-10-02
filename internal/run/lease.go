package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The worker lease operations below each own their transaction and return a
// result for the caller to project. Stale credential claims are
// workergroup.ErrStaleClaims; a receipt that no longer addresses the live
// execution is ErrStale.

// ErrStale reports that a worker's Run lease receipt no longer addresses the
// execution it names, or that the execution is not in the state the
// operation requires.
var ErrStale = errors.New("run lease authority is stale")

// ErrTaskCompletionReplayDiffers reports that a Run lease already recorded a
// different task completion. It is ErrStale.
var ErrTaskCompletionReplayDiffers = fmt.Errorf("%w: task completion replay differs", ErrStale)

// ErrLogChunkDiffers reports that a log stream's observed sequence already
// holds a different chunk.
var ErrLogChunkDiffers = errors.New("run log chunk sequence already contains different content")

// leaseDiscoveryLimit bounds the leases one discovery returns.
const leaseDiscoveryLimit int32 = 64

// LeaseDiscoveryStore reads the Run leases assigned to a worker host.
type LeaseDiscoveryStore interface {
	DiscoverWorkerRunLeaseWork(context.Context, db.DiscoverWorkerRunLeaseWorkParams) ([]db.DiscoverWorkerRunLeaseWorkRow, error)
}

// LeaseWork is a Run lease a worker host should claim.
type LeaseWork struct {
	LeaseID       uuid.UUID
	LeaseSequence int64
}

// DiscoverLeases reads, without a transaction, up to one page of the Run
// leases assigned to the worker host's epoch.
func DiscoverLeases(ctx context.Context, store LeaseDiscoveryStore, workerGroupID, workerHostID uuid.UUID, workerEpoch int64) ([]LeaseWork, error) {
	rows, err := store.DiscoverWorkerRunLeaseWork(ctx, db.DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: pgvalue.UUID(workerGroupID),
		RowLimit:      leaseDiscoveryLimit,
		WorkerHostID:  pgvalue.UUID(workerHostID),
		WorkerEpoch:   workerEpoch,
	})
	if err != nil {
		return nil, err
	}
	work := make([]LeaseWork, 0, len(rows))
	for _, row := range rows {
		work = append(work, LeaseWork{LeaseID: pgvalue.MustUUIDValue(row.ID), LeaseSequence: row.LeaseSequence})
	}
	return work, nil
}

// Claim is what a committed lease claim returns for the caller to project:
// the claimed execution's rows, its attempt's Secret deliveries, and for a
// restored execution the wait it resumes. It conveys no authority. Accessors
// return copies.
type Claim struct {
	execution  Execution
	resumeWait *db.RunWait
}

// Run is the claimed Run.
func (c Claim) Run() db.Run { return c.execution.Run() }

// Attempt is the claimed Attempt.
func (c Claim) Attempt() db.RunAttempt { return c.execution.Attempt() }

// Session is the claimed Run's Session; it is the zero value for a Task Run.
func (c Claim) Session() db.Session { return c.execution.Session() }

// Computer is the claimed Run's Computer.
func (c Claim) Computer() db.LockRunLeaseClaimComputerRow { return c.execution.Computer() }

// Instance is the Instance the claimed lease runs on.
func (c Claim) Instance() db.ComputerInstance { return c.execution.Instance() }

// Lease is the claimed Run lease.
func (c Claim) Lease() db.RunLease { return c.execution.Lease() }

// DeliverySecrets are the Secret deliveries of a fresh claim; a restored
// claim delivers none.
func (c Claim) DeliverySecrets() []secret.DeliveryEnvelope { return c.execution.DeliverySecrets() }

// ResumeWait is the wait a restored claim resumes; ok is false for a fresh
// claim.
func (c Claim) ResumeWait() (wait db.RunWait, ok bool) {
	if c.resumeWait == nil {
		return db.RunWait{}, false
	}
	return cloneWait(*c.resumeWait), true
}

func cloneWait(w db.RunWait) db.RunWait {
	w.ChildRequest = bytes.Clone(w.ChildRequest)
	w.ConditionResult = bytes.Clone(w.ConditionResult)
	w.ConditionError = bytes.Clone(w.ConditionError)
	w.Metadata = bytes.Clone(w.Metadata)
	w.Tags = slices.Clone(w.Tags)
	w.SuspensionError = bytes.Clone(w.SuspensionError)
	return w
}

// ClaimLease claims the fenced Run lease in its own transaction. An unlocked
// probe of the lease status chooses the branch: a running lease attaches to
// its restored execution and resuming wait without delivering Secrets;
// otherwise the claim locks the attempt's Secrets first and marks the lease
// starting.
func ClaimLease(ctx context.Context, txb db.TxBeginner, fence ExecutionFence) (Claim, error) {
	var claim Claim
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var restored bool
		if err := tx.QueryRow(ctx, `SELECT status='running' FROM run_leases WHERE id=$1`, fence.LeaseID).Scan(&restored); err != nil {
			return stale(err)
		}
		var err error
		if restored {
			var wait db.RunWait
			claim.execution, wait, err = ClaimRestoredExecution(ctx, tx, fence)
			claim.resumeWait = &wait
		} else {
			claim.execution, err = ClaimExecution(ctx, tx, fence)
		}
		return stale(err)
	})
	if err != nil {
		return Claim{}, err
	}
	return claim, nil
}

// StartLease records the logical start of the fenced claimed lease in its own
// transaction.
func StartLease(ctx context.Context, txb db.TxBeginner, fence ExecutionFence) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		_, err := StartExecution(ctx, tx, fence)
		return stale(err)
	})
}

// EnterEntrypoint records that the fenced started lease entered the Run's
// declared entrypoint, in its own transaction.
func EnterEntrypoint(ctx context.Context, txb db.TxBeginner, fence ExecutionFence, kind, declaredID string) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		return stale(EnterExecution(ctx, tx, fence, kind, declaredID))
	})
}

// Renewal is a renewed lease's grant.
type Renewal struct {
	ExpiresAt                 time.Time
	BaseComputerDiskVersionID pgtype.UUID
}

// RenewLease extends the fenced live lease's grant in its own transaction and
// reads the renewed grant there.
func RenewLease(ctx context.Context, txb db.TxBeginner, fence ExecutionFence, expectedExpiry time.Time) (Renewal, error) {
	var renewal Renewal
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		renewed, err := RenewExecution(ctx, tx, fence, expectedExpiry)
		if err != nil {
			return stale(err)
		}
		renewal = Renewal{ExpiresAt: renewed.Lease().ExpiresAt.Time, BaseComputerDiskVersionID: renewed.Attempt().BaseComputerDiskVersionID}
		return nil
	})
	if err != nil {
		return Renewal{}, err
	}
	return renewal, nil
}

// Finalization is a begun finalization's grant.
type Finalization struct {
	ExpiresAt time.Time
	StartedAt time.Time
}

// BeginFinalization begins the fenced member's finalization in its own
// transaction and records its Program quiescence. It runs the staged live
// prologue: the located lease must belong to the requested Run attempt
// before its Secrets and then its execution are locked. Missing or superseded
// execution authority is ErrStale; stale claims and backend failures retain
// their own classification.
func BeginFinalization(ctx context.Context, txb db.TxBeginner, request ExecutionFinalization) (Finalization, error) {
	var finalization Finalization
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		locator, err := LocateLiveExecution(ctx, tx, request.Fence)
		if err != nil {
			return stale(err)
		}
		if locator.RunID() != request.RunID || locator.AttemptNumber() != request.AttemptNumber {
			return ErrStale
		}
		if _, err := locator.LockSecrets(ctx); err != nil {
			return fmt.Errorf("lock run finalization secret authority: %w", err)
		}
		begun, err := BeginExecutionFinalization(ctx, tx, request)
		if err != nil {
			return stale(err)
		}
		lease := begun.Lease()
		// Guest emits ProgramQuiesced only after the scoped cgroup is empty and
		// output is drained. Persist that authenticated physical proof separately
		// from logical finalization; no other member's process is reconciled.
		if _, err := tx.Exec(ctx, `UPDATE run_leases SET process_reconciled_at=COALESCE(process_reconciled_at,clock_timestamp())
 WHERE id=$1 AND computer_instance_id=$2 AND writer_generation=$3`,
			lease.ID, lease.ComputerInstanceID, lease.WriterGeneration); err != nil {
			return fmt.Errorf("record Program quiescence: %w", err)
		}
		finalization = Finalization{ExpiresAt: lease.ExpiresAt.Time, StartedAt: lease.FinalizationStartedAt.Time}
		return nil
	})
	return finalization, err
}

// TaskCompletionReplays reads the completion a Run lease recorded.
type TaskCompletionReplays interface {
	GetTaskCompletionReplay(context.Context, db.GetTaskCompletionReplayParams) (pgtype.Text, error)
}

// CompleteTask records the fenced Task member's completion in its own
// transaction. When that fails for any reason but stale claims, including an
// uncertain commit or a concurrent completion, a read outside the transaction
// accepts a recorded completion with the same fingerprint and reports a
// different one as ErrTaskCompletionReplayDiffers. A rejected admission is
// ErrTaskCompletionAdmission; a lease that no longer addresses the member is
// ErrStale.
func CompleteTask(ctx context.Context, txb db.TxBeginner, replays TaskCompletionReplays, completion TaskCompletion) error {
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		return CompleteTaskExecution(ctx, tx, completion)
	})
	// Stale claims re-authenticate before any replay lookup.
	if err == nil || errors.Is(err, workergroup.ErrStaleClaims) {
		return err
	}
	err = taskCompletionReplayAfterError(ctx, replays, completion, err)
	if err == nil || errors.Is(err, ErrTaskCompletionAdmission) || errors.Is(err, ErrStale) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.Join(ErrStale, err)
	}
	return err
}

// taskCompletionReplayAfterError resolves a failed completion against the
// recorded one.
func taskCompletionReplayAfterError(ctx context.Context, replays TaskCompletionReplays, completion TaskCompletion, operationErr error) error {
	replayed, replayErr := taskCompletionWasReplayed(ctx, replays, completion)
	if replayed {
		return nil
	}
	if errors.Is(replayErr, ErrTaskCompletionReplayDiffers) {
		return replayErr
	}
	if replayErr != nil {
		return errors.Join(operationErr, fmt.Errorf("check task completion replay: %w", replayErr))
	}
	return operationErr
}

func taskCompletionWasReplayed(ctx context.Context, replays TaskCompletionReplays, completion TaskCompletion) (bool, error) {
	fence := completion.Fence
	fingerprint, err := replays.GetTaskCompletionReplay(ctx, db.GetTaskCompletionReplayParams{
		RunLeaseID: fence.LeaseID, LeaseSequence: fence.LeaseSequence,
		WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fingerprint.Valid || fingerprint.String != completion.Fingerprint {
		return false, ErrTaskCompletionReplayDiffers
	}
	return true, nil
}

// AcknowledgeWaitResume releases one restored waiter after physical
// activation, in its own transaction. It neither opens the Instance to new
// work nor consumes another checkpoint. A receipt that does not address the
// restored waiter is pgx.ErrNoRows.
func AcknowledgeWaitResume(ctx context.Context, txb db.TxBeginner, fence ExecutionFence, waitID, checkpointID pgtype.UUID) (db.RunWait, error) {
	var wait db.RunWait
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		wait, err = acknowledgeWaitResume(ctx, tx, fence, waitID, checkpointID)
		return err
	})
	if err != nil {
		return db.RunWait{}, err
	}
	return wait, nil
}

// stale reports a receipt that addresses no execution as ErrStale.
func stale(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	return err
}
