package dispatch

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"uuid"
)

// RecordComputerInstanceFailure records a physical failure under supply →
// Computer → Instance locks. The caller commits or rolls back the transaction;
// logical failure settlement and physical cleanup remain independent owners.
func RecordComputerInstanceFailure(ctx context.Context, tx pgx.Tx, workerGroupID uuid.UUID, params db.MarkComputerInstanceFailedParams) (db.ComputerInstance, error) {
	q := db.New(tx)
	workerFatal := params.ReasonCode.String == workerapi.RuntimeFailureWorkerInvalid
	target, err := q.GetWorkerComputerInstanceTarget(ctx, db.GetWorkerComputerInstanceTargetParams{ID: params.ID, WorkerHostID: params.WorkerHostID, WorkerEpoch: params.WorkerEpoch, WorkerGroupID: pgvalue.UUID(workerGroupID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(workerGroupID))
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused && group.Status != db.WorkerGroupStatusDraining {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	pool, err := q.LockWorkerPool(ctx, db.LockWorkerPoolParams{WorkerGroupID: group.ID, WorkerPoolID: target.WorkerPoolID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if pool.Status != "active" && pool.Status != "draining" {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	worker, err := q.LockWorkerHostForActivation(ctx, db.LockWorkerHostForActivationParams{WorkerHostID: params.WorkerHostID, WorkerGroupID: group.ID, WorkerPoolID: pool.ID, WorkerEpoch: pgtype.Int8{Int64: params.WorkerEpoch, Valid: true}})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if worker.Status != db.WorkerHostStatusActive && worker.Status != db.WorkerHostStatusDraining {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: params.ID, OrgID: target.OrgID, WorkerHostID: params.WorkerHostID, WorkerGroupID: group.ID, WorkerEpoch: params.WorkerEpoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.ComputerID != c.ID || i.EnvironmentID != c.EnvironmentID || i.WriterGeneration != c.WriterGeneration {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	if params.ReasonCode.String == workerapi.RuntimeFailureComputerSource {
		if i.ObservedState != "allocated" {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		// Only the retained committed head can condemn published Computer data.
		// A private restore failure cannot relabel the independent committed head.
		_, err = tx.Exec(ctx, `UPDATE computers c SET status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',
   recovery_id=CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_id,$2) ELSE $2 END,
   recovery_disk_version_id=head_disk_version_id,recovery_reason='computer_source_unavailable',
   recovery_started_at=CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_started_at,clock_timestamp()) ELSE clock_timestamp() END,
   recovery_completed_at=NULL,recovery_failure=coalesce(recovery_failure,jsonb_build_object('code','computer_source_unavailable','message','Published Computer source is unavailable','details',$3::jsonb)),
   revision=revision+1,updated_at=clock_timestamp()
   FROM computer_instances i,computer_disk_versions v WHERE i.id=$1 AND c.id=i.computer_id
   AND c.status NOT IN ('deleting','deleted') AND c.desired_state<>'deleted'
   AND c.head_disk_version_id=i.retained_source_disk_version_id AND v.id=c.head_disk_version_id AND v.computer_id=c.id AND v.status='committed'`, i.ID, pgvalue.UUID(uuid.NewV7()), params.Error)
		if err != nil {
			return db.ComputerInstance{}, err
		}
	}
	row, err := q.MarkComputerInstanceFailed(ctx, params)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if workerFatal && worker.Status == db.WorkerHostStatusActive {
		if _, err = q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{ID: worker.ID, WorkerGroupID: group.ID, ExpectedEpoch: worker.CurrentEpoch, ExpectedClaimVersion: worker.ClaimVersion}); err != nil {
			return db.ComputerInstance{}, err
		}
	}
	return row, nil
}
