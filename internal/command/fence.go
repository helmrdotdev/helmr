package command

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// lockCommandInstance locks the Computer and the bound Instance of the
// Command at target.
func lockCommandInstance(ctx context.Context, tx pgx.Tx, target db.GetComputerCommandTargetRow, commandID pgtype.UUID) (computer.CommandInstance, error) {
	return computer.LockCommandInstance(ctx, tx, computer.CommandRef{
		EnvironmentID: pgvalue.MustUUIDValue(target.EnvironmentID), ComputerID: pgvalue.MustUUIDValue(target.ComputerID), CommandID: pgvalue.MustUUIDValue(commandID),
	})
}

// host is the worker host epoch the principal acts for.
func host(worker workergroup.HostPrincipal) computer.Host {
	return computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch}
}

// producerStillAuthorized rechecks, after the operation's last lock and
// write, that the Instance's worker host epoch is still admitted and its
// writer has not expired.
func producerStillAuthorized(ctx context.Context, q *db.Queries, worker workergroup.HostPrincipal, i db.ComputerInstance) error {
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{WorkerHostID: i.WorkerHostID, WorkerGroupID: i.WorkerGroupID, WorkerEpoch: worker.Epoch, ExpiresAt: i.WriterExpiresAt})
	if err != nil {
		return err
	}
	if !authorized.Valid || !authorized.Bool {
		return pgx.ErrNoRows
	}
	return nil
}
