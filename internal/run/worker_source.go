package run

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// SourceReceipt is the lease receipt a worker presents for the source Run it
// acts from: the authenticated worker and the lease identity exactly as the
// worker sent it, not yet parsed.
type SourceReceipt struct {
	Worker        workergroup.HostPrincipal
	LeaseID       string
	LeaseSequence int64
}

// Fence is the execution fence of the receipt's lease. A malformed receipt,
// whose lease ID is not a canonical UUIDv7 or whose lease sequence is not
// positive, is ErrStaleSource.
func (r SourceReceipt) Fence() (ExecutionFence, error) {
	leaseID, err := ids.Parse(r.LeaseID)
	if err != nil || r.LeaseSequence <= 0 {
		return ExecutionFence{}, fmt.Errorf("%w: invalid receipt", ErrStaleSource)
	}
	return ExecutionFence{
		LeaseID: pgvalue.UUID(leaseID), LeaseSequence: r.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(r.Worker.GroupID), WorkerHostID: pgvalue.UUID(r.Worker.HostID), WorkerEpoch: r.Worker.Epoch,
		GroupClaimVersion: r.Worker.GroupClaimVersion, HostClaimVersion: r.Worker.HostClaimVersion,
	}, nil
}

// LockReceiptSource checks a worker's source receipt and then locks the live
// source Run it addresses in the caller's transaction, as LockLiveSource
// does. A malformed receipt is ErrStaleSource.
func LockReceiptSource(ctx context.Context, tx pgx.Tx, receipt SourceReceipt) (LiveSource, error) {
	fence, err := receipt.Fence()
	if err != nil {
		return LiveSource{}, err
	}
	return LockLiveSource(ctx, tx, fence)
}

// LockWorkerSource locks, in its own transaction, the live source Run that a
// worker's receipt addresses, as LockReceiptSource does. The receipt is
// checked inside the transaction, so a database that cannot begin one is
// reported before a malformed receipt.
func LockWorkerSource(ctx context.Context, txb db.TxBeginner, receipt SourceReceipt) (LiveSource, error) {
	var source LiveSource
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		source, err = LockReceiptSource(ctx, tx, receipt)
		return err
	})
	return source, err
}
