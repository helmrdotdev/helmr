package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultIngestBatchSize     = int32(250)
	defaultIngestLeaseDuration = 30 * time.Second
	defaultIngestIdleEvery     = 250 * time.Millisecond
	defaultIngestRetryAfter    = 2 * time.Second
	defaultOutboxRetainFor     = 24 * time.Hour
)

type Ingestor struct {
	log           *slog.Logger
	db            db.Querier
	writer        IngestWriter
	batchSize     int32
	leaseDuration time.Duration
	idleEvery     time.Duration
	retryAfter    time.Duration
	outboxRetain  time.Duration
}

func NewIngestor(log *slog.Logger, queries db.Querier, writer IngestWriter) (*Ingestor, error) {
	if queries == nil {
		return nil, fmt.Errorf("telemetry ingester database is required")
	}
	if writer == nil {
		return nil, fmt.Errorf("telemetry ingester writer is required")
	}
	if log == nil {
		log = slog.Default()
	}
	ingester := &Ingestor{
		log:           log,
		db:            queries,
		writer:        writer,
		batchSize:     defaultIngestBatchSize,
		leaseDuration: defaultIngestLeaseDuration,
		idleEvery:     defaultIngestIdleEvery,
		retryAfter:    defaultIngestRetryAfter,
		outboxRetain:  defaultOutboxRetainFor,
	}
	if ingester.batchSize <= 0 {
		return nil, fmt.Errorf("telemetry ingester batch size must be positive")
	}
	return ingester, nil
}

func (i *Ingestor) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hadError := false
		eventCount, err := i.ingestEvents(ctx)
		if err != nil {
			hadError = true
			i.log.Warn("ingest event telemetry failed", "error", err)
		}
		logCount, err := i.ingestRunLogs(ctx)
		if err != nil {
			hadError = true
			i.log.Warn("ingest run log telemetry failed", "error", err)
		}
		if _, err := i.db.PruneTelemetryOutboxWritten(ctx, pgvalue.Interval(i.outboxRetain)); err != nil {
			i.log.Warn("prune telemetry outbox failed", "error", err)
		}
		if hadError {
			if err := sleep(ctx, i.retryAfter); err != nil {
				return err
			}
			continue
		}
		if eventCount == 0 && logCount == 0 {
			if err := sleep(ctx, i.idleEvery); err != nil {
				return err
			}
		}
	}
}

func (i *Ingestor) ingestEvents(ctx context.Context) (int, error) {
	rows, err := i.db.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      i.batchSize,
		LeaseDuration: pgvalue.Interval(i.leaseDuration),
	})
	if err != nil || len(rows) == 0 {
		return len(rows), err
	}
	ids := make([]int64, 0, len(rows))
	candidates := make([]eventIngestCandidate, 0, len(rows))
	var firstErr error
	for _, row := range rows {
		candidates = append(candidates, eventIngestCandidate{
			outboxID:   row.OutboxID,
			retryCount: row.RetryCount,
			createdAt:  pgvalue.Time(row.CreatedAt),
			record:     eventRecord(row),
		})
	}
	if len(candidates) > 0 {
		successes, err := i.writeEventCandidates(ctx, candidates)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, candidate := range successes {
			ids = append(ids, candidate.outboxID)
		}
	}
	if len(ids) == 0 {
		return len(rows), firstErr
	}
	if err := i.db.MarkTelemetryOutboxWritten(ctx, ids); err != nil {
		return len(rows), err
	}
	return len(rows), firstErr
}

func (i *Ingestor) ingestRunLogs(ctx context.Context) (int, error) {
	rows, err := i.db.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      i.batchSize,
		LeaseDuration: pgvalue.Interval(i.leaseDuration),
	})
	if err != nil || len(rows) == 0 {
		return len(rows), err
	}
	ids := make([]int64, 0, len(rows))
	candidates := make([]runLogIngestCandidate, 0, len(rows))
	var firstErr error
	for _, row := range rows {
		candidates = append(candidates, runLogIngestCandidate{
			outboxID:   row.OutboxID,
			retryCount: row.RetryCount,
			createdAt:  pgvalue.Time(row.CreatedAt),
			record:     runLogRecord(row),
		})
	}
	if len(candidates) > 0 {
		successes, err := i.writeRunLogCandidates(ctx, candidates)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, candidate := range successes {
			ids = append(ids, candidate.outboxID)
		}
	}
	if len(ids) == 0 {
		return len(rows), firstErr
	}
	if err := i.db.MarkTelemetryOutboxWritten(ctx, ids); err != nil {
		return len(rows), err
	}
	return len(rows), firstErr
}

func (i *Ingestor) writeEventCandidates(ctx context.Context, candidates []eventIngestCandidate) ([]eventIngestCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	records := make([]EventRecord, 0, len(candidates))
	for _, candidate := range candidates {
		records = append(records, candidate.record)
	}
	if err := i.writer.WriteEvents(ctx, records); err == nil {
		return candidates, nil
	}
	successes := make([]eventIngestCandidate, 0, len(candidates))
	var firstErr error
	for _, candidate := range candidates {
		if err := i.writer.WriteEvents(ctx, []EventRecord{candidate.record}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			i.log.Warn("ingest event row failed",
				"outbox_id", candidate.outboxID,
				"retry_count", candidate.retryCount,
				"pending_age", time.Since(candidate.createdAt).Truncate(time.Millisecond),
				"error", err,
			)
			_ = i.markFailed(ctx, []int64{candidate.outboxID}, err)
			continue
		}
		successes = append(successes, candidate)
	}
	return successes, firstErr
}

func (i *Ingestor) writeRunLogCandidates(ctx context.Context, candidates []runLogIngestCandidate) ([]runLogIngestCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	records := make([]RunLogRecord, 0, len(candidates))
	for _, candidate := range candidates {
		records = append(records, candidate.record)
	}
	if err := i.writer.WriteRunLogs(ctx, records); err == nil {
		return candidates, nil
	}
	successes := make([]runLogIngestCandidate, 0, len(candidates))
	var firstErr error
	for _, candidate := range candidates {
		if err := i.writer.WriteRunLogs(ctx, []RunLogRecord{candidate.record}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			i.log.Warn("ingest run log row failed",
				"outbox_id", candidate.outboxID,
				"retry_count", candidate.retryCount,
				"pending_age", time.Since(candidate.createdAt).Truncate(time.Millisecond),
				"error", err,
			)
			_ = i.markFailed(ctx, []int64{candidate.outboxID}, err)
			continue
		}
		successes = append(successes, candidate)
	}
	return successes, firstErr
}

func (i *Ingestor) markFailed(ctx context.Context, ids []int64, cause error) error {
	if len(ids) == 0 {
		return nil
	}
	return i.db.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids:         ids,
		RetryAfter:  pgvalue.Interval(i.retryAfter),
		IngestError: truncateError(cause),
	})
}

type eventIngestCandidate struct {
	outboxID   int64
	retryCount int32
	createdAt  time.Time
	record     EventRecord
}

type runLogIngestCandidate struct {
	outboxID   int64
	retryCount int32
	createdAt  time.Time
	record     RunLogRecord
}

func eventRecord(row db.ClaimEventIngestBatchRow) EventRecord {
	body := json.RawMessage(row.Payload)
	if len(body) == 0 || !json.Valid(body) {
		body = json.RawMessage(`{}`)
	}
	return EventRecord{
		OrgID:          pgvalue.MustUUIDValue(row.OrgID),
		ProjectID:      pgvalue.MustUUIDValue(row.ProjectID),
		EnvironmentID:  pgvalue.MustUUIDValue(row.EnvironmentID),
		SubjectKind:    string(row.SubjectType),
		SubjectID:      pgvalue.MustUUIDValue(row.SubjectID),
		EventKind:      row.Kind,
		Seq:            uint64(row.Seq),
		RunID:          optionalUUID(row.RunID),
		RunLeaseID:     optionalUUID(row.RunLeaseID),
		AttemptNumber:  optionalInt32(row.AttemptNumber),
		TraceID:        pgvalue.TextValue(row.TraceID),
		SpanID:         pgvalue.TextValue(row.SpanID),
		ParentSpanID:   pgvalue.TextValue(row.ParentSpanID),
		Traceparent:    pgvalue.TextValue(row.Traceparent),
		Category:       row.Category,
		Severity:       row.Severity,
		Source:         row.Source,
		Message:        row.Message,
		Body:           string(body),
		IdempotencyKey: row.IdempotencyKey,
		RetentionClass: "standard",
		RedactionClass: row.RedactionClass,
		ObservedAt:     observedAt(row.OccurredAt, row.CreatedAt),
	}
}

func runLogRecord(row db.ClaimRunLogIngestBatchRow) RunLogRecord {
	return RunLogRecord{
		OrgID:          pgvalue.MustUUIDValue(row.OrgID),
		ProjectID:      pgvalue.MustUUIDValue(row.ProjectID),
		EnvironmentID:  pgvalue.MustUUIDValue(row.EnvironmentID),
		RunID:          pgvalue.MustUUIDValue(row.RunID),
		RunLeaseID:     pgvalue.MustUUIDValue(row.RunLeaseID),
		AttemptNumber:  int4Value(row.AttemptNumber),
		StreamName:     string(row.Stream),
		Seq:            uint64(row.Seq),
		ObservedSeq:    uint64(pgvalue.Int8Value(row.ObservedSeq)),
		Content:        base64.StdEncoding.EncodeToString(row.Content),
		SizeBytes:      uint64(pgvalue.Int8Value(row.SizeBytes)),
		IdempotencyKey: row.IdempotencyKey,
		RetentionClass: "standard",
		RedactionClass: "standard",
		Source:         "worker",
		ObservedAt:     observedAt(pgtype.Timestamptz{}, row.CreatedAt),
	}
}

func optionalUUID(value pgtype.UUID) *uuid.UUID {
	if !value.Valid {
		return nil
	}
	id := pgvalue.MustUUIDValue(value)
	return &id
}

func optionalInt32(value pgtype.Int4) *int32 {
	if !value.Valid {
		return nil
	}
	return &value.Int32
}

func int4Value(value pgtype.Int4) int32 {
	if !value.Valid {
		return 0
	}
	return value.Int32
}

func observedAt(primary pgtype.Timestamptz, fallback pgtype.Timestamptz) time.Time {
	at := pgvalue.Time(primary)
	if at.IsZero() {
		at = pgvalue.Time(fallback)
	}
	if at.IsZero() {
		at = time.Unix(0, 0).UTC()
	}
	return at.UTC()
}

func truncateError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
