package controlplane

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Physical writer renewal does not renew member executions or reopen admission.
func renewComputerInstance(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request workerapi.ComputerInstanceRenewRequest) (db.ComputerInstance, error) {
	instanceID, err := ids.Parse(request.ComputerInstanceID)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	environmentID, err := ids.Parse(request.EnvironmentID)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if request.WriterGeneration <= 0 {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	target, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: pgvalue.UUID(instanceID), EnvironmentID: pgvalue.UUID(environmentID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	bindings, err := q.LockComputerSecretsForAdmission(ctx, target.ComputerID)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHost(ctx, q, worker)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !locked.Continues() {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: target.ID, OrgID: target.OrgID, WorkerHostID: locked.Host.ID, WorkerGroupID: locked.Group.ID, WorkerEpoch: worker.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.WriterGeneration != request.WriterGeneration || i.ComputerID != c.ID || i.EnvironmentID != c.EnvironmentID {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	// A close intent is an observation, never a renewed write grant.
	if i.DesiredState == "closed" {
		return i, nil
	}
	for _, b := range bindings {
		if b.SecretStatus != "active" || !b.CurrentVersionID.Valid {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
	}
	if c.Status != "active" || c.DesiredState != "active" || c.WriterGeneration != i.WriterGeneration {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return q.RenewComputerInstanceWriter(ctx, db.RenewComputerInstanceWriterParams{ID: i.ID, WorkerHostID: locked.Host.ID, WorkerEpoch: worker.Epoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, TtlSeconds: int64(run.LeaseTTL / time.Second)})
}
