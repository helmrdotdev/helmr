package clickhouse

import (
	"context"
	"fmt"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

type Writer struct {
	client batchClient
}

type batchClient interface {
	PrepareBatch(ctx context.Context, query string) (driver.Batch, error)
}

var eventColumns = []ch.ColumnNameAndType{
	{Name: "org_id", Type: "UUID"}, {Name: "project_id", Type: "UUID"},
	{Name: "environment_id", Type: "UUID"}, {Name: "subject_kind", Type: "LowCardinality(String)"},
	{Name: "subject_id", Type: "UUID"}, {Name: "event_kind", Type: "String"},
	{Name: "seq", Type: "UInt64"}, {Name: "run_id", Type: "Nullable(UUID)"},
	{Name: "deployment_id", Type: "Nullable(UUID)"}, {Name: "run_lease_id", Type: "Nullable(UUID)"},
	{Name: "attempt_number", Type: "Nullable(Int32)"}, {Name: "trace_id", Type: "String"},
	{Name: "span_id", Type: "String"}, {Name: "parent_span_id", Type: "String"},
	{Name: "traceparent", Type: "String"}, {Name: "category", Type: "LowCardinality(String)"},
	{Name: "severity", Type: "LowCardinality(String)"}, {Name: "source", Type: "LowCardinality(String)"},
	{Name: "message", Type: "String"}, {Name: "body", Type: "String"},
	{Name: "idempotency_key", Type: "String"}, {Name: "retention_class", Type: "LowCardinality(String)"},
	{Name: "redaction_class", Type: "LowCardinality(String)"}, {Name: "observed_at", Type: "DateTime64(3, 'UTC')"},
}

var runLogColumns = []ch.ColumnNameAndType{
	{Name: "org_id", Type: "UUID"}, {Name: "project_id", Type: "UUID"},
	{Name: "environment_id", Type: "UUID"}, {Name: "run_id", Type: "UUID"},
	{Name: "run_lease_id", Type: "Nullable(UUID)"}, {Name: "attempt_number", Type: "Int32"},
	{Name: "stream_name", Type: "LowCardinality(String)"}, {Name: "seq", Type: "UInt64"},
	{Name: "observed_seq", Type: "UInt64"}, {Name: "content", Type: "String"},
	{Name: "size_bytes", Type: "UInt64"}, {Name: "idempotency_key", Type: "String"},
	{Name: "retention_class", Type: "LowCardinality(String)"}, {Name: "redaction_class", Type: "LowCardinality(String)"},
	{Name: "source", Type: "LowCardinality(String)"}, {Name: "observed_at", Type: "DateTime64(3, 'UTC')"},
}

func NewWriter(client batchClient) *Writer {
	return &Writer{client: client}
}

func (w *Writer) WriteEvents(ctx context.Context, rows []telemetry.EventRecord) ([]telemetry.RejectedRow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	var rejected []telemetry.RejectedRow
	valid := make([]int, len(rows))
	for idx := range valid {
		valid[idx] = idx
	}
	for len(valid) > 0 {
		batch, err := w.client.PrepareBatch(ch.Context(ctx, ch.WithColumnNamesAndTypes(eventColumns)), `INSERT INTO helmr_telemetry.events (
    org_id, project_id, environment_id, subject_kind, subject_id, event_kind, seq,
    run_id, deployment_id, run_lease_id, attempt_number, trace_id, span_id,
    parent_span_id, traceparent, category, severity, source, message, body, idempotency_key,
    retention_class, redaction_class, observed_at
)`)
		if err != nil {
			return rejected, err
		}
		rejectedAt := -1
		for position, idx := range valid {
			row := rows[idx]
			if err := batch.Append(
				row.OrgID,
				row.ProjectID,
				row.EnvironmentID,
				row.SubjectKind,
				row.SubjectID,
				row.EventKind,
				row.Seq,
				nullableValue(row.RunID),
				nullableValue(row.DeploymentID),
				nullableValue(row.RunLeaseID),
				nullableValue(row.AttemptNumber),
				row.TraceID,
				row.SpanID,
				row.ParentSpanID,
				row.Traceparent,
				row.Category,
				row.Severity,
				row.Source,
				row.Message,
				row.Body,
				row.IdempotencyKey,
				row.RetentionClass,
				row.RedactionClass,
				row.ObservedAt,
			); err != nil {
				rejected = append(rejected, telemetry.RejectedRow{
					Index: idx, Err: fmt.Errorf("append event row %d: %w", idx, err),
				})
				rejectedAt = position
				break
			}
		}
		if rejectedAt >= 0 {
			_ = batch.Close()
			valid = append(valid[:rejectedAt], valid[rejectedAt+1:]...)
			continue
		}
		err = batch.Send()
		_ = batch.Close()
		return rejected, err
	}
	return rejected, nil
}

func nullableValue[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

func (w *Writer) WriteRunLogs(ctx context.Context, rows []telemetry.RunLogRecord) ([]telemetry.RejectedRow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	var rejected []telemetry.RejectedRow
	valid := make([]int, len(rows))
	for idx := range valid {
		valid[idx] = idx
	}
	for len(valid) > 0 {
		batch, err := w.client.PrepareBatch(ch.Context(ctx, ch.WithColumnNamesAndTypes(runLogColumns)), `INSERT INTO helmr_telemetry.run_logs (
    org_id, project_id, environment_id, run_id, run_lease_id,
    attempt_number, stream_name, seq, observed_seq, content, size_bytes, idempotency_key,
    retention_class, redaction_class, source, observed_at
)`)
		if err != nil {
			return rejected, err
		}
		rejectedAt := -1
		for position, idx := range valid {
			row := rows[idx]
			if err := batch.Append(
				row.OrgID,
				row.ProjectID,
				row.EnvironmentID,
				row.RunID,
				row.RunLeaseID,
				row.AttemptNumber,
				row.StreamName,
				row.Seq,
				row.ObservedSeq,
				row.Content,
				row.SizeBytes,
				row.IdempotencyKey,
				row.RetentionClass,
				row.RedactionClass,
				row.Source,
				row.ObservedAt,
			); err != nil {
				rejected = append(rejected, telemetry.RejectedRow{
					Index: idx, Err: fmt.Errorf("append run log row %d: %w", idx, err),
				})
				rejectedAt = position
				break
			}
		}
		if rejectedAt >= 0 {
			_ = batch.Close()
			valid = append(valid[:rejectedAt], valid[rejectedAt+1:]...)
			continue
		}
		err = batch.Send()
		_ = batch.Close()
		return rejected, err
	}
	return rejected, nil
}
