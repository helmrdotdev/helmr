package telemetry

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type ClickHouseWriter struct {
	client batchClient
}

type batchClient interface {
	PrepareBatch(ctx context.Context, query string) (driver.Batch, error)
}

func NewClickHouseWriter(client batchClient) *ClickHouseWriter {
	return &ClickHouseWriter{client: client}
}

func (w *ClickHouseWriter) WriteEvents(ctx context.Context, rows []EventRecord) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := w.client.PrepareBatch(ctx, `INSERT INTO helmr_telemetry.events (
    worker_group_id, org_id, project_id, environment_id, subject_kind, subject_id, event_kind, seq,
    run_id, deployment_id, attempt_id, run_lease_id, attempt_number, trace_id, span_id,
    parent_span_id, traceparent, category, severity, source, message, body, idempotency_key,
    retention_class, redaction_class, observed_at
)`)
	if err != nil {
		return err
	}
	defer batch.Close()
	for idx, row := range rows {
		if err := batch.Append(
			row.WorkerGroupID,
			row.OrgID,
			row.ProjectID,
			row.EnvironmentID,
			row.SubjectKind,
			row.SubjectID,
			row.EventKind,
			row.Seq,
			row.RunID,
			row.DeploymentID,
			row.AttemptID,
			row.RunLeaseID,
			row.AttemptNumber,
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
			return fmt.Errorf("append event row %d: %w", idx, err)
		}
	}
	return batch.Send()
}

func (w *ClickHouseWriter) WriteRunLogs(ctx context.Context, rows []RunLogRecord) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := w.client.PrepareBatch(ctx, `INSERT INTO helmr_telemetry.run_logs (
    worker_group_id, org_id, project_id, environment_id, run_id, attempt_id, run_lease_id,
    attempt_number, stream_name, seq, observed_seq, content, size_bytes, idempotency_key,
    retention_class, redaction_class, source, observed_at
)`)
	if err != nil {
		return err
	}
	defer batch.Close()
	for idx, row := range rows {
		if err := batch.Append(
			row.WorkerGroupID,
			row.OrgID,
			row.ProjectID,
			row.EnvironmentID,
			row.RunID,
			row.AttemptID,
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
			return fmt.Errorf("append run log row %d: %w", idx, err)
		}
	}
	return batch.Send()
}

func (w *ClickHouseWriter) WriteTerminalOutput(ctx context.Context, rows []TerminalOutputRecord) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := w.client.PrepareBatch(ctx, `INSERT INTO helmr_telemetry.terminal_outputs (
    worker_group_id, org_id, project_id, environment_id, workspace_id, resource_kind, resource_id,
    stream_name, offset_start, offset_end, content, size_bytes, idempotency_key,
    retention_class, redaction_class, observed_at
)`)
	if err != nil {
		return err
	}
	defer batch.Close()
	for idx, row := range rows {
		if err := batch.Append(
			row.WorkerGroupID,
			row.OrgID,
			row.ProjectID,
			row.EnvironmentID,
			row.WorkspaceID,
			row.ResourceKind,
			row.ResourceID,
			row.StreamName,
			row.OffsetStart,
			row.OffsetEnd,
			row.Content,
			row.SizeBytes,
			row.IdempotencyKey,
			row.RetentionClass,
			row.RedactionClass,
			row.ObservedAt,
		); err != nil {
			return fmt.Errorf("append terminal output row %d: %w", idx, err)
		}
	}
	return batch.Send()
}
