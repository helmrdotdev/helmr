package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type computerSaveOperation int

const (
	computerSaveBegin computerSaveOperation = iota
	computerSaveWrite
	computerSaveAdopt
	computerSaveAbandon
)

func computerSaveReceiptParams(worker workergroup.HostPrincipal, request workerapi.ComputerSaveBeginRequest) (db.GetWorkerComputerSaveParams, error) {
	if err := validateComputerSaveRequest(request); err != nil {
		return db.GetWorkerComputerSaveParams{}, err
	}
	environment, _ := ids.Parse(request.EnvironmentID)
	instance, _ := ids.Parse(request.ComputerInstanceID)
	save, _ := ids.Parse(request.SaveID)
	return db.GetWorkerComputerSaveParams{
		EnvironmentID: pgvalue.UUID(environment), ComputerInstanceID: pgvalue.UUID(instance),
		SaveID: pgvalue.UUID(save), Sequence: pgtype.Int8{Int64: request.Sequence, Valid: true},
		WorkerHostID: pgvalue.UUID(worker.HostID), WorkerGroupID: pgvalue.UUID(worker.GroupID),
		WorkerEpoch: worker.Epoch, WriterGeneration: request.WriterGeneration,
	}, nil
}

func validateComputerSaveRequest(request workerapi.ComputerSaveBeginRequest) error {
	for _, raw := range []string{request.EnvironmentID, request.ComputerInstanceID, request.SaveID} {
		if _, err := ids.Parse(raw); err != nil {
			return errors.New("save, environment and instance IDs must be canonical UUIDv7")
		}
	}
	if request.Sequence <= 0 || request.WriterGeneration <= 0 {
		return errors.New("positive save sequence and writer generation required")
	}
	return nil
}

// applyComputerSave owns the save mutation within the caller transaction.
func applyComputerSave(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request workerapi.ComputerSaveBeginRequest, operation computerSaveOperation, apply func(pgx.Tx, *db.Queries, db.ComputerInstance) error) (workerapi.ComputerSaveBeginResponse, error) {
	var result workerapi.ComputerSaveBeginResponse
	params, err := computerSaveReceiptParams(worker, request)
	if err != nil {
		return result, err
	}
	q := db.New(tx)
	locator, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: params.ComputerInstanceID, EnvironmentID: params.EnvironmentID})
	if err != nil {
		return result, err
	}
	bindings, err := q.LockComputerSecretsForAdmission(ctx, locator.ComputerID)
	if err != nil {
		return result, err
	}
	for _, binding := range bindings {
		if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
			return result, fmt.Errorf("%w: %s", pgx.ErrNoRows, "computer secrets revoked")
		}
	}
	if _, err = workergroup.LockHost(ctx, q, worker); err != nil {
		return result, err
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: params.EnvironmentID, ID: locator.ComputerID})
	if err != nil {
		return result, err
	}
	instance, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: params.ComputerInstanceID, OrgID: locator.OrgID, WorkerHostID: params.WorkerHostID, WorkerGroupID: params.WorkerGroupID, WorkerEpoch: params.WorkerEpoch})
	if err != nil {
		return result, err
	}
	if instance.EnvironmentID != params.EnvironmentID || instance.ComputerID != c.ID || instance.WriterGeneration != params.WriterGeneration || instance.WriterGeneration != c.WriterGeneration ||
		c.Status != "active" || c.DesiredState != "active" || instance.DesiredState != "ready" || instance.ObservedState != "ready" || instance.ObservedDesiredVersion != instance.DesiredVersion || instance.MountState != "mounted" || instance.ReclaimedAt.Valid ||
		(instance.AdmissionState != "open" && instance.AdmissionState != "draining") {
		return result, fmt.Errorf("%w: %s", pgx.ErrNoRows, "computer save writer is no longer active")
	}
	pending := instance.SaveDiskVersionID == params.SaveID && instance.SaveSequence == request.Sequence
	predecessor := instance.SaveBaseDiskVersionID
	if operation == computerSaveBegin && !pending {
		row, err := q.BeginComputerInstanceSave(ctx, db.BeginComputerInstanceSaveParams{Sequence: request.Sequence, SaveID: params.SaveID, PredecessorID: c.HeadDiskVersionID, ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, WorkerHostID: instance.WorkerHostID, WorkerEpoch: instance.WorkerEpoch, WriterGeneration: instance.WriterGeneration, WriterTokenHash: instance.WriterTokenHash, DesiredVersion: instance.DesiredVersion})
		if err != nil {
			return result, err
		}
		predecessor = row.SaveBaseDiskVersionID
	} else if !pending {
		return result, fmt.Errorf("%w: %s", pgx.ErrNoRows, "save operation is not pending")
	}
	if operation != computerSaveBegin {
		_, err := q.GetWorkerComputerSave(ctx, params)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return result, err
		}
		if (err == nil) != (operation == computerSaveAdopt) {
			return result, fmt.Errorf("%w: %s", pgx.ErrNoRows, "save publication state differs from operation")
		}
	}
	if apply != nil {
		if err = apply(tx, q, instance); err != nil {
			return result, err
		}
	}
	// Locks prevent credential changes, but elapsed time can still expire a writer.
	var authorized bool
	err = tx.QueryRow(ctx, `SELECT w.current_epoch=$3 AND w.status IN ('active','draining')
 AND g.status IN ('active','paused','draining') AND clock_timestamp()<$4
 FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, params.WorkerHostID, params.WorkerGroupID, worker.Epoch, instance.WriterExpiresAt).Scan(&authorized)
	if err != nil {
		return result, err
	}
	if !authorized {
		return result, fmt.Errorf("%w: %s", pgx.ErrNoRows, "computer save writer expired or revoked")
	}
	return workerapi.ComputerSaveBeginResponse{ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration, PredecessorID: pgvalue.UUIDString(predecessor), DesiredVersion: instance.DesiredVersion, SaveID: request.SaveID, Sequence: request.Sequence}, nil
}
