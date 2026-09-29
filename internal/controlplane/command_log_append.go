package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errInvalidCommandLog = errors.New("invalid command log identity, content or observation")

// appendCommandLog fences a physical producer independently of Run lifetimes.
// The caller commits the transaction only after this operation succeeds.
func appendCommandLog(ctx context.Context, tx pgx.Tx, worker workerActor, request workerapi.CommandLogAppendRequest) error {
	orgID, orgErr := ids.Parse(request.OrgID)
	commandID, execErr := ids.Parse(request.CommandID)
	instanceID, instanceErr := ids.Parse(request.ComputerInstanceID)
	if orgErr != nil || execErr != nil || instanceErr != nil || request.WriterGeneration <= 0 ||
		(request.Stream != workerapi.LogStreamStdout && request.Stream != workerapi.LogStreamStderr) ||
		request.ObservedSeq > uint64(1<<63-1) || request.ObservedAt.IsZero() ||
		request.ObservedAt.Year() < 1970 || request.ObservedAt.Year() > 2299 {
		return errInvalidCommandLog
	}
	if err := telemetry.ValidateRunLog(request.Content); err != nil {
		return errInvalidCommandLog
	}
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{
		OrgID: pgvalue.UUID(orgID), CommandID: pgvalue.UUID(commandID),
	})
	if err != nil {
		return err
	}
	// Serialize credential revocation before taking physical and member locks.
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(worker.WorkerGroupID))
	if err != nil {
		return err
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(worker.WorkerHostID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
	})
	if err != nil {
		return err
	}
	if err = worker.checkLockedClaims(host, group); err != nil {
		return err
	}
	if _, err = q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID}); err != nil {
		return err
	}
	if _, err = q.LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID}); err != nil {
		return err
	}
	authority, err := q.LockComputerCommandWorkerAuthority(ctx, db.LockComputerCommandWorkerAuthorityParams{
		OrgID: pgvalue.UUID(orgID), CommandID: pgvalue.UUID(commandID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
		WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch,
	})
	if err != nil {
		return err
	}
	instance := authority.ComputerInstance
	if instance.ID != pgvalue.UUID(instanceID) || instance.WorkerGroupID != pgvalue.UUID(worker.WorkerGroupID) ||
		instance.ReclaimedAt.Valid || instance.DesiredState != db.RuntimeDesiredStateReady ||
		instance.WriterGeneration != request.WriterGeneration ||
		(authority.ComputerCommand.Status != "running" && authority.ComputerCommand.Status != "stopping") {
		return pgx.ErrNoRows
	}
	_, err = q.InsertCommandLogChunk(ctx, db.InsertCommandLogChunkParams{
		OrgID: target.OrgID, ProjectID: target.ProjectID,
		EnvironmentID: authority.ComputerCommand.EnvironmentID, CommandID: authority.ComputerCommand.ID,
		StreamName: string(request.Stream), ObservedSeq: int64(request.ObservedSeq), Content: request.Content,
		ObservedAt: pgtype.Timestamptz{Time: request.ObservedAt.UTC().Truncate(time.Millisecond), Valid: true},
	})
	if err != nil {
		return err
	}
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{
		WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
		WorkerEpoch: worker.WorkerEpoch, ExpiresAt: instance.WriterExpiresAt,
	})
	if err != nil {
		return err
	}
	if !authorized.Valid || !authorized.Bool {
		return pgx.ErrNoRows
	}
	return nil
}
