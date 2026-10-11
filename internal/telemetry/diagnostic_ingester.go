package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

type DiagnosticWriter interface {
	WriteDiagnostics(context.Context, string, []StoredDiagnostic) ([]RejectedRow, error)
}

type DiagnosticIngester struct {
	pool   db.TxBeginner
	writer DiagnosticWriter
	config diagnostic.IngestConfig
	log    *slog.Logger
}

func NewDiagnosticIngester(pool db.TxBeginner, writer DiagnosticWriter, config diagnostic.IngestConfig, log *slog.Logger) (*DiagnosticIngester, error) {
	if pool == nil || writer == nil || config.Validate() != nil {
		return nil, ErrDiagnosticInvalid
	}
	if log == nil {
		log = slog.Default()
	}
	return &DiagnosticIngester{pool: pool, writer: writer, config: config, log: log}, nil
}

func (i *DiagnosticIngester) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var jobs sync.WaitGroup
	jobs.Go(func() {
		for ctx.Err() == nil {
			call, stop := context.WithTimeout(ctx, i.config.OperationTimeout)
			_, expiryErr := ExpireDiagnostics(call, i.pool, i.config.GCBatchRecords)
			stop()
			if expiryErr != nil {
				i.log.Warn("diagnostic retention pass failed")
			}
			if sleep(ctx, i.config.PollEvery) != nil {
				return
			}
		}
	})
	defer func() { cancel(); jobs.Wait() }()
	for ctx.Err() == nil {
		started := time.Now()
		for _, kind := range []string{"session", "computer_preparation", "computer_command"} {
			if err := i.ExportOnce(ctx, kind); err != nil {
				// Sink error strings can include customer payloads. Keep those out of
				// platform logs; pending counts and oldest acceptance identify the backlog.
				if errors.Is(err, ErrDiagnosticExportSize) {
					i.log.Error("diagnostic export byte bound is smaller than a pending record", "source_kind", kind)
				} else {
					i.log.Warn("diagnostic export pass failed", "source_kind", kind)
				}
			}
		}
		if err := sleep(ctx, time.Until(started.Add(i.config.PollEvery))); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (i *DiagnosticIngester) ExportOnce(ctx context.Context, kind string) error {
	ctx, cancel := context.WithTimeout(ctx, i.config.OperationTimeout)
	defer cancel()
	claim, err := ClaimDiagnostics(ctx, i.pool, kind, i.config.Batch)
	if err != nil || len(claim.IDs) == 0 {
		return err
	}
	rejected, sendErr := i.writer.WriteDiagnostics(ctx, kind, claim.Records)
	if sendErr != nil {
		return errors.Join(sendErr, RetryDiagnostics(ctx, i.pool, claim.Token, claim.IDs, i.config.RetryAfter))
	}
	failed := make(map[int]bool, len(rejected))
	for _, row := range rejected {
		if row.Index < 0 || row.Index >= len(claim.IDs) || failed[row.Index] {
			return errors.Join(ErrDiagnosticInvalid, RetryDiagnostics(ctx, i.pool, claim.Token, claim.IDs, i.config.RetryAfter))
		}
		failed[row.Index] = true
	}
	goodIDs, badIDs := make([]int64, 0, len(claim.IDs)), make([]int64, 0, len(failed))
	for index, id := range claim.IDs {
		if failed[index] {
			badIDs = append(badIDs, id)
		} else {
			goodIDs = append(goodIDs, id)
		}
	}
	_, retireErr := RetireDiagnostics(ctx, i.pool, claim.Token, goodIDs)
	retryErr := RetryDiagnostics(ctx, i.pool, claim.Token, badIDs, i.config.RetryAfter)
	if len(badIDs) > 0 {
		i.log.Warn("diagnostic export isolated rejected records", "source_kind", kind, "records", len(badIDs))
	}
	return errors.Join(retireErr, retryErr)
}
