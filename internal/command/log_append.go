package command

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
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
	EnvironmentID    uuid.UUID
	CommandID        uuid.UUID
	InstanceID       uuid.UUID
	WriterGeneration int64
	Stream           string
	ObservedSeq      uint64
	ObservedAt       time.Time
	Kind             string
	ThroughSequence  uint64
	DroppedBytes     int64
	Complete         bool
	Content          []byte
}

func (c LogChunk) valid() bool {
	return c.WriterGeneration > 0 && (c.Stream == LogStdout || c.Stream == LogStderr) &&
		c.ObservedSeq <= uint64(1<<63-1) && !c.ObservedAt.IsZero() &&
		c.ObservedAt.Year() >= 1970 && c.ObservedAt.Year() <= 2299
}

// AppendLog records a log chunk while its producer still runs the Command,
// fencing the physical producer by its Computer lease. Replaying a
// recorded sequence with the same content is accepted; different content is
// ErrChanged.
func AppendLog(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, chunk LogChunk, bounds diagnostic.Bounds) (telemetry.DiagnosticReceipt, error) {
	var result telemetry.DiagnosticReceipt
	record := diagnostic.Record{Stream: chunk.Stream, Kind: chunk.Kind, Sequence: int64(chunk.ObservedSeq), ThroughSequence: int64(chunk.ThroughSequence), ObservedAtUnixNano: chunk.ObservedAt.UnixNano(), Data: chunk.Content, DroppedBytes: chunk.DroppedBytes, Complete: chunk.Complete}
	if !chunk.valid() || chunk.ThroughSequence > uint64(1<<63-1) || record.Validate(bounds.ChunkBytes) != nil {
		return result, ErrInvalidLog
	}
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		admission, err := telemetry.BeginDiagnosticAdmission(ctx, tx, telemetry.DiagnosticSource{EnvironmentID: chunk.EnvironmentID, Kind: "computer_command", ID: chunk.CommandID, ProducerEpoch: 1}, bounds)
		if err != nil {
			return err
		}
		result, err = appendLog(ctx, tx, worker, chunk, record, &admission)
		return err
	})
	if err != nil {
		return telemetry.DiagnosticReceipt{}, changed(err)
	}
	return result, nil
}

func appendLog(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, chunk LogChunk, record diagnostic.Record, admission *telemetry.DiagnosticAdmission) (telemetry.DiagnosticReceipt, error) {
	var receipt telemetry.DiagnosticReceipt
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{
		EnvironmentID: pgvalue.UUID(chunk.EnvironmentID), CommandID: pgvalue.UUID(chunk.CommandID),
	})
	if err != nil {
		return receipt, err
	}
	if err = lockHost(ctx, tx, worker); err != nil {
		return receipt, err
	}
	lease, err := lockCommandLease(ctx, tx, target, chunk.CommandID)
	if err != nil {
		return receipt, err
	}
	if !lease.matches(worker, chunk.InstanceID, chunk.WriterGeneration) {
		return receipt, pgx.ErrNoRows
	}
	process, err := lockCommand(ctx, tx, lease.EnvironmentID, lease.ComputerID, chunk.CommandID)
	if err != nil {
		return receipt, err
	}
	if !process.ComputerLeaseEpoch.Valid || process.ComputerLeaseEpoch.Int64 != lease.Epoch || (!process.TerminalAt.Valid && process.Status != "running" && process.Status != "stopping") {
		return receipt, pgx.ErrNoRows
	}
	if process.ProcessReconciledAt.Valid || process.OutputFenced {
		return receipt, pgx.ErrNoRows
	}
	if process.TerminalAt.Valid {
		final, complete, gapped := process.StdoutFinalThrough, process.StdoutFinalComplete, process.StdoutFinalGapped
		if chunk.Stream == "stderr" {
			final, complete, gapped = process.StderrFinalThrough, process.StderrFinalComplete, process.StderrFinalGapped
		}
		if !final.Valid || record.ThroughSequence > final.Int64 || (record.Kind == "end" && (record.ThroughSequence != final.Int64 || record.Complete != complete.Bool)) || (record.Kind == "gap" && !gapped.Bool) {
			return receipt, pgx.ErrNoRows
		}
	}
	if err = producerStillAuthorized(ctx, tx, worker, lease, false); err != nil {
		return receipt, err
	}

	receipt, err = admission.Append(ctx, record)
	if err != nil {
		return receipt, err
	}
	return receipt, producerStillAuthorized(ctx, tx, worker, lease, false)
}
