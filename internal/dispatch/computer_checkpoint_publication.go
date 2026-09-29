package dispatch

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// LockComputerCheckpointPublication validates a live registered physical source.
// The caller owns the transaction and must recheck this authority after blocking
// object writes, before commit. No representative Run supplies disk authority.
func LockComputerCheckpointPublication(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.CheckpointComputerObjectRequest) (db.ComputerInstance, db.ComputerCheckpoint, error) {
	source, err := lockComputerCheckpointSource(ctx, tx, worker, computerCheckpointFence{request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID})
	if err != nil {
		return db.ComputerInstance{}, db.ComputerCheckpoint{}, err
	}
	instance, cp := source.instance, source.checkpoint
	q := db.New(tx)
	if _, err = q.GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, WorkerGroupID: worker.GroupID, WorkerHostID: worker.HostID, WorkerEpoch: worker.Epoch, DesiredVersion: request.DesiredVersion, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds}); err != nil {
		return db.ComputerInstance{}, db.ComputerCheckpoint{}, err
	}
	if _, err = q.RequireRegisteredCheckpointManifest(ctx, db.RequireRegisteredCheckpointManifestParams{ID: cp.ID, Manifest: cp.Manifest}); err != nil {
		return db.ComputerInstance{}, db.ComputerCheckpoint{}, err
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return db.ComputerInstance{}, db.ComputerCheckpoint{}, err
	}
	if err = validateComputerCheckpointMembers(ctx, tx, instance, cp, len(members)); err != nil {
		return db.ComputerInstance{}, db.ComputerCheckpoint{}, err
	}
	return instance, cp, nil
}
