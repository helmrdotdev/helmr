package dispatch

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// RecordComputerInstanceReclaim accepts an authenticated physical exclusion
// receipt. The caller validates its evidence and commits the transaction.
func RecordComputerInstanceReclaim(ctx context.Context, tx pgx.Tx, workerGroupID uuid.UUID, params db.ReclaimComputerInstanceParams) (db.ComputerInstance, error) {
	q := db.New(tx)
	target, err := q.GetWorkerComputerInstanceTarget(ctx, db.GetWorkerComputerInstanceTargetParams{ID: params.ID, WorkerHostID: params.WorkerHostID, WorkerEpoch: params.WorkerEpoch, WorkerGroupID: pgvalue.UUID(workerGroupID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if _, err = q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID}); err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: params.ID, OrgID: target.OrgID, WorkerHostID: params.WorkerHostID, WorkerEpoch: params.WorkerEpoch, WorkerGroupID: pgvalue.UUID(workerGroupID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.ReclaimedAt.Valid {
		// A lost cleanup acknowledgment may be replayed with its original fence.
		// Return the durable receipt without changing capacity or failure diagnosis.
		if i.DesiredState == "closed" && i.DesiredVersion == params.DesiredVersion &&
			i.ObservedVersion > 0 && i.ObservedVersion-1 == params.ExpectedObservedVersion &&
			(!params.RequireFailure || i.ObservedState == "failed" || i.ObservedState == "lost") {
			return i, nil
		}
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	params.WriterGeneration = i.WriterGeneration
	// Keep the original failure diagnosis even after successful physical cleanup.
	if i.ObservedState == "failed" || i.ObservedState == "lost" {
		params.ObservedState = i.ObservedState
		params.Reason = i.TerminalReasonCode
		params.Error = i.TerminalError
	} else {
		params.ObservedState = "closed"
		params.Error = nil
	}
	params.MountState = "unmounted"
	reclaimed, err := q.ReclaimComputerInstance(ctx, params)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	// Physical exclusion settles every process in this exact incarnation,
	// including members whose scoped exit proof was lost. Logical outcomes are
	// still owned by the Run and checkpoint reconciliation paths.
	if _, err := tx.Exec(ctx, `UPDATE run_leases SET process_reconciled_at=$3,updated_at=clock_timestamp()
 WHERE computer_instance_id=$1 AND writer_generation=$2 AND process_reconciled_at IS NULL`,
		reclaimed.ID, reclaimed.WriterGeneration, reclaimed.ReclaimedAt); err != nil {
		return db.ComputerInstance{}, err
	}
	return reclaimed, nil
}
