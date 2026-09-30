package command

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Log streams a Command's output is recorded on.
const (
	LogStdout = "stdout"
	LogStderr = "stderr"
)

// LogChunk is one observed record of a Command's output stream, reported by
// the worker host that runs it on the Instance incarnation and writer
// generation it acts for. The caller has already bounded Content.
type LogChunk struct {
	OrgID            uuid.UUID
	CommandID        uuid.UUID
	InstanceID       uuid.UUID
	WriterGeneration int64
	Stream           string
	ObservedSeq      uint64
	ObservedAt       time.Time
	Content          []byte
}

func (c LogChunk) valid() bool {
	return c.WriterGeneration > 0 && (c.Stream == LogStdout || c.Stream == LogStderr) &&
		c.ObservedSeq <= uint64(1<<63-1) && !c.ObservedAt.IsZero() &&
		c.ObservedAt.Year() >= 1970 && c.ObservedAt.Year() <= 2299
}

// AppendLog records a log chunk while its producer still runs the Command,
// fencing the physical producer independently of Run lifetimes. Replaying a
// recorded sequence with the same content is accepted; different content is
// ErrChanged.
func AppendLog(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, chunk LogChunk) error {
	return changed(db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		return appendLog(ctx, tx, worker, chunk)
	}))
}

func appendLog(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, chunk LogChunk) error {
	if !chunk.valid() {
		return ErrInvalidLog
	}
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{
		OrgID: pgvalue.UUID(chunk.OrgID), CommandID: pgvalue.UUID(chunk.CommandID),
	})
	if err != nil {
		return err
	}
	// Serialize credential revocation before taking physical and member locks.
	if _, err = workergroup.LockHost(ctx, q, worker); err != nil {
		return err
	}
	// Lock the Command's bound Instance; the worker authority below decides
	// whether it still admits this producer.
	if _, err = lockCommandInstance(ctx, tx, target, pgvalue.UUID(chunk.CommandID)); err != nil {
		return err
	}
	authority, err := q.LockComputerCommandWorkerAuthority(ctx, db.LockComputerCommandWorkerAuthorityParams{
		OrgID: pgvalue.UUID(chunk.OrgID), CommandID: pgvalue.UUID(chunk.CommandID), WorkerGroupID: pgvalue.UUID(worker.GroupID),
		WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch,
	})
	if err != nil {
		return err
	}
	instance := authority.ComputerInstance
	if instance.ID != pgvalue.UUID(chunk.InstanceID) || instance.WorkerGroupID != pgvalue.UUID(worker.GroupID) ||
		instance.ReclaimedAt.Valid || instance.DesiredState != db.RuntimeDesiredStateReady ||
		instance.WriterGeneration != chunk.WriterGeneration ||
		(authority.ComputerCommand.Status != "running" && authority.ComputerCommand.Status != "stopping") {
		return pgx.ErrNoRows
	}
	_, err = q.InsertCommandLogChunk(ctx, db.InsertCommandLogChunkParams{
		OrgID: target.OrgID, ProjectID: target.ProjectID,
		EnvironmentID: authority.ComputerCommand.EnvironmentID, CommandID: authority.ComputerCommand.ID,
		StreamName: chunk.Stream, ObservedSeq: int64(chunk.ObservedSeq), Content: chunk.Content,
		ObservedAt: pgtype.Timestamptz{Time: chunk.ObservedAt.UTC().Truncate(time.Millisecond), Valid: true},
	})
	if err != nil {
		return err
	}
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{
		WorkerHostID: pgvalue.UUID(worker.HostID), WorkerGroupID: pgvalue.UUID(worker.GroupID),
		WorkerEpoch: worker.Epoch, ExpiresAt: instance.WriterExpiresAt,
	})
	if err != nil {
		return err
	}
	if !authorized.Valid || !authorized.Bool {
		return pgx.ErrNoRows
	}
	return nil
}
