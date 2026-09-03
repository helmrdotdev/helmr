package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	defaultOutboxGCEvery       = time.Second
	defaultOutboxGCBatchSize   = int32(2500)
)

type ingestStore interface {
	ClaimEventIngestBatch(context.Context, db.ClaimEventIngestBatchParams) ([]db.ClaimEventIngestBatchRow, error)
	ClaimRunLogIngestBatch(context.Context, db.ClaimRunLogIngestBatchParams) ([]db.ClaimRunLogIngestBatchRow, error)
	MarkTelemetryOutboxWritten(context.Context, db.MarkTelemetryOutboxWrittenParams) (int64, error)
	MarkTelemetryOutboxBatchFailed(context.Context, db.MarkTelemetryOutboxBatchFailedParams) (int64, error)
	PruneTelemetryOutboxWritten(context.Context, db.PruneTelemetryOutboxWrittenParams) (int64, error)
	GetTelemetryOutboxLifecycle(context.Context, pgtype.Interval) (db.GetTelemetryOutboxLifecycleRow, error)
}

type Ingestor struct {
	log           *slog.Logger
	db            ingestStore
	writer        IngestWriter
	batchSize     int32
	batchBytes    int64
	leaseDuration time.Duration
	idleEvery     time.Duration
	retryAfter    time.Duration
	outboxRetain  time.Duration
	gcEvery       time.Duration
	gcBatchSize   int32
}

func NewIngestor(log *slog.Logger, queries ingestStore, writer IngestWriter) (*Ingestor, error) {
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
		batchBytes:    MaxTelemetryBatchBytes,
		leaseDuration: defaultIngestLeaseDuration,
		idleEvery:     defaultIngestIdleEvery,
		retryAfter:    defaultIngestRetryAfter,
		outboxRetain:  defaultOutboxRetainFor,
		gcEvery:       defaultOutboxGCEvery,
		gcBatchSize:   defaultOutboxGCBatchSize,
	}
	if ingester.batchSize <= 0 {
		return nil, fmt.Errorf("telemetry ingester batch size must be positive")
	}
	return ingester, nil
}

func (i *Ingestor) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		i.runGC(runCtx)
	}()
	err := i.runIngest(runCtx)
	cancel()
	<-gcDone
	return err
}

func (i *Ingestor) runIngest(ctx context.Context) error {
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

func (i *Ingestor) runGC(ctx context.Context) {
	ticker := time.NewTicker(i.gcEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruned, err := i.db.PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{
				RetainFor: pgvalue.Interval(i.outboxRetain), RowLimit: i.gcBatchSize,
			})
			if err != nil {
				i.log.Warn("prune telemetry outbox failed", "error", err)
				continue
			}
			lifecycle, err := i.db.GetTelemetryOutboxLifecycle(ctx, pgvalue.Interval(i.outboxRetain))
			if err != nil {
				i.log.Warn("read telemetry outbox lifecycle failed", "error", err)
				continue
			}
			if pruned == 0 && !lifecycle.OldestRetryCreatedAt.Valid && !lifecycle.OldestGcWrittenAt.Valid {
				continue
			}
			attrs := []any{"pruned", pruned}
			if lifecycle.OldestRetryCreatedAt.Valid {
				attrs = append(attrs, "oldest_retry_age", time.Since(lifecycle.OldestRetryCreatedAt.Time).Truncate(time.Millisecond))
			}
			if lifecycle.OldestGcWrittenAt.Valid {
				attrs = append(attrs, "oldest_gc_eligible_age", time.Since(lifecycle.OldestGcWrittenAt.Time).Truncate(time.Millisecond))
			}
			i.log.Info("telemetry outbox lifecycle", attrs...)
		}
	}
}

func (i *Ingestor) ingestEvents(ctx context.Context) (int, error) {
	rows, err := i.db.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      i.batchSize,
		MaxBatchBytes: i.batchBytes,
		LeaseDuration: pgvalue.Interval(i.leaseDuration),
	})
	if err != nil || len(rows) == 0 {
		return len(rows), err
	}
	claims := make([]telemetryOutboxClaim, 0, len(rows))
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
		claims = append(claims, eventCandidateClaims(successes)...)
	}
	if len(claims) == 0 {
		return len(rows), firstErr
	}
	if err := i.markWritten(ctx, claims); err != nil {
		return len(rows), err
	}
	return len(rows), firstErr
}

func (i *Ingestor) ingestRunLogs(ctx context.Context) (int, error) {
	rows, err := i.db.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      i.batchSize,
		MaxBatchBytes: i.batchBytes,
		LeaseDuration: pgvalue.Interval(i.leaseDuration),
	})
	if err != nil || len(rows) == 0 {
		return len(rows), err
	}
	claims := make([]telemetryOutboxClaim, 0, len(rows))
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
		claims = append(claims, runLogCandidateClaims(successes)...)
	}
	if len(claims) == 0 {
		return len(rows), firstErr
	}
	if err := i.markWritten(ctx, claims); err != nil {
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
	result, sendErr := i.writer.WriteEvents(ctx, records)
	if sendErr != nil {
		return nil, errors.Join(sendErr, i.markFailed(ctx, eventCandidateClaims(candidates), sendErr))
	}
	rejected := rejectedIndexes(result)
	successes := make([]eventIngestCandidate, 0, len(candidates)-len(rejected))
	failedClaims := make([]telemetryOutboxClaim, 0, len(rejected))
	var rejectionErr error
	for idx, candidate := range candidates {
		cause, failed := rejected[idx]
		if !failed {
			successes = append(successes, candidate)
			continue
		}
		failedClaims = append(failedClaims, telemetryOutboxClaim{
			outboxID: candidate.outboxID, retryCount: candidate.retryCount,
		})
		rejectionErr = errors.Join(rejectionErr, cause)
		i.log.Warn("ingest event row rejected", "outbox_id", candidate.outboxID,
			"retry_count", candidate.retryCount,
			"pending_age", time.Since(candidate.createdAt).Truncate(time.Millisecond), "error", cause)
	}
	return successes, errors.Join(rejectionErr, i.markFailed(ctx, failedClaims, rejectionErr))
}

func (i *Ingestor) writeRunLogCandidates(ctx context.Context, candidates []runLogIngestCandidate) ([]runLogIngestCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	records := make([]RunLogRecord, 0, len(candidates))
	for _, candidate := range candidates {
		records = append(records, candidate.record)
	}
	result, sendErr := i.writer.WriteRunLogs(ctx, records)
	if sendErr != nil {
		return nil, errors.Join(sendErr, i.markFailed(ctx, runLogCandidateClaims(candidates), sendErr))
	}
	rejected := rejectedIndexes(result)
	successes := make([]runLogIngestCandidate, 0, len(candidates)-len(rejected))
	failedClaims := make([]telemetryOutboxClaim, 0, len(rejected))
	var rejectionErr error
	for idx, candidate := range candidates {
		cause, failed := rejected[idx]
		if !failed {
			successes = append(successes, candidate)
			continue
		}
		failedClaims = append(failedClaims, telemetryOutboxClaim{
			outboxID: candidate.outboxID, retryCount: candidate.retryCount,
		})
		rejectionErr = errors.Join(rejectionErr, cause)
		i.log.Warn("ingest run log row rejected", "outbox_id", candidate.outboxID,
			"retry_count", candidate.retryCount,
			"pending_age", time.Since(candidate.createdAt).Truncate(time.Millisecond), "error", cause)
	}
	return successes, errors.Join(rejectionErr, i.markFailed(ctx, failedClaims, rejectionErr))
}

func (i *Ingestor) markFailed(ctx context.Context, claims []telemetryOutboxClaim, cause error) error {
	if len(claims) == 0 {
		return nil
	}
	ids, retryCounts, err := telemetryOutboxClaimArrays(claims)
	if err != nil {
		return err
	}
	updated, err := i.db.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids:                 ids,
		ExpectedRetryCounts: retryCounts,
		RetryAfter:          pgvalue.Interval(i.retryAfter),
		IngestError:         truncateError(cause),
	})
	if err != nil {
		return err
	}
	if updated != int64(len(ids)) {
		return fmt.Errorf("mark telemetry outbox failed rows: updated %d, want %d", updated, len(ids))
	}
	return nil
}

func (i *Ingestor) markWritten(ctx context.Context, claims []telemetryOutboxClaim) error {
	ids, retryCounts, err := telemetryOutboxClaimArrays(claims)
	if err != nil {
		return err
	}
	updated, err := i.db.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids: ids, ExpectedRetryCounts: retryCounts,
	})
	if err != nil {
		return err
	}
	if updated != int64(len(ids)) {
		return fmt.Errorf("mark telemetry outbox written rows: updated %d, want %d", updated, len(ids))
	}
	return nil
}

func rejectedIndexes(rows []RejectedRow) map[int]error {
	rejected := make(map[int]error, len(rows))
	for _, row := range rows {
		rejected[row.Index] = row.Err
	}
	return rejected
}

func eventCandidateClaims(candidates []eventIngestCandidate) []telemetryOutboxClaim {
	claims := make([]telemetryOutboxClaim, len(candidates))
	for idx := range candidates {
		claims[idx] = telemetryOutboxClaim{
			outboxID: candidates[idx].outboxID, retryCount: candidates[idx].retryCount,
		}
	}
	return claims
}

func runLogCandidateClaims(candidates []runLogIngestCandidate) []telemetryOutboxClaim {
	claims := make([]telemetryOutboxClaim, len(candidates))
	for idx := range candidates {
		claims[idx] = telemetryOutboxClaim{
			outboxID: candidates[idx].outboxID, retryCount: candidates[idx].retryCount,
		}
	}
	return claims
}

func telemetryOutboxClaimArrays(claims []telemetryOutboxClaim) ([]int64, []int32, error) {
	ids := make([]int64, len(claims))
	retryCounts := make([]int32, len(claims))
	for index, claim := range claims {
		ids[index] = claim.outboxID
		retryCounts[index] = claim.retryCount
	}
	if err := validateTelemetryOutboxClaimBatch(ids, retryCounts); err != nil {
		return nil, nil, err
	}
	return ids, retryCounts, nil
}

func validateTelemetryOutboxClaimBatch(ids []int64, retryCounts []int32) error {
	if len(ids) != len(retryCounts) {
		return fmt.Errorf("telemetry outbox claim cardinality mismatch: ids %d, retry counts %d", len(ids), len(retryCounts))
	}
	return nil
}

type telemetryOutboxClaim struct {
	outboxID   int64
	retryCount int32
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
