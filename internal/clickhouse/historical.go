package clickhouse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

type Reader struct {
	client historicalClient
}

type historicalClient interface {
	Select(ctx context.Context, dest any, query string, args ...any) error
}

func NewReader(client historicalClient) *Reader {
	return &Reader{client: client}
}

func (r *Reader) ListEvents(ctx context.Context, q telemetry.EventQuery) (telemetry.EventPage, error) {
	sql := `SELECT seq, run_id, deployment_id, run_lease_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, event_kind, message, body, redaction_class, observed_at
FROM helmr_telemetry.events FINAL
WHERE org_id = @org_id
  AND subject_kind = @subject_kind
  AND subject_id = @subject_id
  AND (@all_severities OR severity IN @severities)
  AND seq > @after
ORDER BY seq ASC
LIMIT @row_limit`
	var rows []eventRow
	if err := r.client.Select(ctx, &rows, sql,
		Named("org_id", q.OrgID),
		Named("subject_kind", q.SubjectType),
		Named("subject_id", q.SubjectID),
		Named("all_severities", len(q.Severities) == 0),
		Named("severities", q.Severities),
		Named("after", uint64(q.AfterSeq)),
		Named("row_limit", uint32(q.Limit)),
	); err != nil {
		return telemetry.EventPage{}, fmt.Errorf("%w: %v", telemetry.ErrHistoricalUnavailable, err)
	}
	events := make([]api.RunEvent, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		events = append(events, row.event())
		last = int64(row.Seq)
	}
	return telemetry.EventPage{Events: events, LastSeq: last, Historical: len(events)}, nil
}

func (r *Reader) ListRunLogChunks(ctx context.Context, q telemetry.RunLogChunkQuery) (telemetry.RunLogChunkPage, error) {
	sql := `SELECT run_id, run_lease_id, attempt_number, stream_name, seq, observed_seq, content, size_bytes, observed_at
FROM helmr_telemetry.run_logs FINAL
WHERE org_id = @org_id
  AND run_id = @run_id
  AND (
    @all_levels
    OR (
      stream_name = 'structured'
      AND JSONExtractString(base64Decode(content), 'level') IN @levels
    )
  )
  AND seq > @after
ORDER BY seq ASC
LIMIT @row_limit`
	var rows []runLogRow
	if err := r.client.Select(ctx, &rows, sql,
		Named("org_id", q.OrgID),
		Named("run_id", q.RunID),
		Named("all_levels", len(q.Levels) == 0),
		Named("levels", q.Levels),
		Named("after", uint64(q.AfterSeq)),
		Named("row_limit", uint32(q.Limit)),
	); err != nil {
		return telemetry.RunLogChunkPage{}, fmt.Errorf("%w: %v", telemetry.ErrHistoricalUnavailable, err)
	}
	chunks := make([]api.RunLogChunk, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		chunks = append(chunks, row.chunk())
		last = int64(row.Seq)
	}
	return telemetry.RunLogChunkPage{Chunks: chunks, LastSeq: last, Historical: len(chunks)}, nil
}

func (r *Reader) ListTerminalOutput(ctx context.Context, q telemetry.TerminalOutputQuery) (telemetry.TerminalOutputPage, error) {
	sql := `SELECT stream_name, offset_start, offset_end, content, observed_at, ingested_at
FROM helmr_telemetry.terminal_outputs FINAL
WHERE org_id = @org_id
  AND project_id = @project_id
  AND environment_id = @environment_id
  AND workspace_id = @workspace_id
  AND resource_kind = @resource_kind
  AND resource_id = @resource_id
  AND stream_name = @stream_name
  AND offset_end > @after
ORDER BY offset_start ASC
LIMIT @row_limit`
	var rows []terminalOutputHistoryRow
	if err := r.client.Select(ctx, &rows, sql,
		Named("org_id", q.OrgID),
		Named("project_id", q.ProjectID),
		Named("environment_id", q.EnvironmentID),
		Named("workspace_id", q.WorkspaceID),
		Named("resource_kind", q.ResourceKind),
		Named("resource_id", q.ResourceID),
		Named("stream_name", q.StreamName),
		Named("after", uint64(q.AfterOffset)),
		Named("row_limit", uint32(q.Limit)),
	); err != nil {
		return telemetry.TerminalOutputPage{}, fmt.Errorf("%w: %v", telemetry.ErrHistoricalUnavailable, err)
	}
	chunks := make([]telemetry.TerminalOutputChunk, 0, len(rows))
	last := q.AfterOffset
	for _, row := range rows {
		chunks = append(chunks, row.chunk(q.ResourceKind, q.ResourceID))
		last = int64(row.OffsetEnd)
	}
	return telemetry.TerminalOutputPage{Chunks: chunks, LastOffset: last, Historical: len(chunks)}, nil
}

func (r *Reader) GetRunLogSnapshot(ctx context.Context, q telemetry.RunLogSnapshotQuery) (telemetry.RunLogSnapshot, error) {
	var snapshot telemetry.RunLogSnapshot
	cursor := int64(0)
	const pageLimit = int32(1000)
	for {
		page, err := r.ListRunLogChunks(ctx, telemetry.RunLogChunkQuery{
			OrgID:    q.OrgID,
			RunID:    q.RunID,
			AfterSeq: cursor,
			Limit:    pageLimit,
		})
		if err != nil {
			return telemetry.RunLogSnapshot{}, err
		}
		for _, chunk := range page.Chunks {
			data, _ := base64.StdEncoding.DecodeString(chunk.ContentBase64)
			switch chunk.Stream {
			case "stdout":
				snapshot.StdoutBytes += int64(len(data))
				snapshot.Stdout = appendTail(snapshot.Stdout, data, q.StdoutLimit)
			case "stderr":
				snapshot.StderrBytes += int64(len(data))
				snapshot.Stderr = appendTail(snapshot.Stderr, data, q.StderrLimit)
			}
			if seq, err := telemetry.ParseCursor(chunk.ID); err == nil && seq > snapshot.Cursor {
				snapshot.Cursor = seq
			}
			if chunk.At.After(snapshot.UpdatedAt) {
				snapshot.UpdatedAt = chunk.At
			}
		}
		if len(page.Chunks) < int(pageLimit) || page.LastSeq <= cursor {
			break
		}
		cursor = page.LastSeq
	}
	snapshot.Truncated = isTailTruncated(snapshot.StdoutBytes, q.StdoutLimit) || isTailTruncated(snapshot.StderrBytes, q.StderrLimit)
	return snapshot, nil
}

type eventRow struct {
	Seq            uint64     `ch:"seq"`
	RunID          *uuid.UUID `ch:"run_id"`
	DeploymentID   *uuid.UUID `ch:"deployment_id"`
	RunLeaseID     *uuid.UUID `ch:"run_lease_id"`
	AttemptNumber  *int32     `ch:"attempt_number"`
	TraceID        string     `ch:"trace_id"`
	SpanID         string     `ch:"span_id"`
	Traceparent    string     `ch:"traceparent"`
	Category       string     `ch:"category"`
	Severity       string     `ch:"severity"`
	Source         string     `ch:"source"`
	EventKind      string     `ch:"event_kind"`
	Message        string     `ch:"message"`
	Body           string     `ch:"body"`
	RedactionClass string     `ch:"redaction_class"`
	ObservedAt     time.Time  `ch:"observed_at"`
}

func (r eventRow) event() api.RunEvent {
	var runID, deploymentID *string
	if r.RunID != nil {
		value := r.RunID.String()
		runID = &value
	}
	if r.DeploymentID != nil {
		value := r.DeploymentID.String()
		deploymentID = &value
	}
	at := r.ObservedAt.UTC()
	attrs := json.RawMessage(r.Body)
	if len(attrs) == 0 || !json.Valid(attrs) {
		attrs = json.RawMessage(`{}`)
	}
	if r.RedactionClass == "sensitive" {
		attrs = json.RawMessage(`{"redacted":true}`)
	}
	return api.RunEvent{
		ID:             telemetry.Cursor(int64(r.Seq)),
		RunID:          runID,
		DeploymentID:   deploymentID,
		AttemptNumber:  r.AttemptNumber,
		Trace:          api.TraceContext{TraceID: r.TraceID, SpanID: r.SpanID, Traceparent: r.Traceparent},
		Category:       r.Category,
		Severity:       r.Severity,
		Source:         r.Source,
		Kind:           r.EventKind,
		Message:        firstNonEmpty(r.Message, r.EventKind),
		At:             at,
		OccurredAt:     at,
		RedactionClass: r.RedactionClass,
		Attributes:     attrs,
	}
}

type runLogRow struct {
	RunID         uuid.UUID `ch:"run_id"`
	RunLeaseID    uuid.UUID `ch:"run_lease_id"`
	AttemptNumber int32     `ch:"attempt_number"`
	StreamName    string    `ch:"stream_name"`
	Seq           uint64    `ch:"seq"`
	ObservedSeq   uint64    `ch:"observed_seq"`
	Content       string    `ch:"content"`
	SizeBytes     uint64    `ch:"size_bytes"`
	ObservedAt    time.Time `ch:"observed_at"`
}

type terminalOutputHistoryRow struct {
	StreamName  string    `ch:"stream_name"`
	OffsetStart uint64    `ch:"offset_start"`
	OffsetEnd   uint64    `ch:"offset_end"`
	Content     string    `ch:"content"`
	ObservedAt  time.Time `ch:"observed_at"`
	IngestedAt  time.Time `ch:"ingested_at"`
}

func (r terminalOutputHistoryRow) chunk(resourceKind string, resourceID uuid.UUID) telemetry.TerminalOutputChunk {
	content, err := base64.StdEncoding.DecodeString(r.Content)
	if err != nil {
		content = []byte(r.Content)
	}
	observed := r.ObservedAt.UTC()
	created := r.IngestedAt.UTC()
	if created.IsZero() {
		created = observed
	}
	return telemetry.TerminalOutputChunk{
		ID:          fmt.Sprintf("terminal:%s:%s:%s:%d", resourceKind, resourceID.String(), r.StreamName, r.OffsetEnd),
		Stream:      r.StreamName,
		OffsetStart: int64(r.OffsetStart),
		OffsetEnd:   int64(r.OffsetEnd),
		Data:        content,
		ObservedAt:  observed,
		CreatedAt:   created,
	}
}

func (r runLogRow) chunk() api.RunLogChunk {
	contentBase64 := r.Content
	if _, err := base64.StdEncoding.DecodeString(r.Content); err != nil {
		contentBase64 = base64.StdEncoding.EncodeToString([]byte(r.Content))
	}
	return api.RunLogChunk{
		ID:            telemetry.Cursor(int64(r.Seq)),
		RunID:         r.RunID.String(),
		AttemptNumber: r.AttemptNumber,
		Stream:        r.StreamName,
		ContentBase64: contentBase64,
		Bytes:         int64(r.SizeBytes),
		ObservedSeq:   int64(r.ObservedSeq),
		At:            r.ObservedAt.UTC(),
	}
}

func appendTail(existing []byte, next []byte, limit int64) []byte {
	existing = append(existing, next...)
	if limit <= 0 || int64(len(existing)) <= limit {
		return existing
	}
	return existing[int64(len(existing))-limit:]
}

func isTailTruncated(total int64, limit int64) bool {
	if limit <= 0 {
		return false
	}
	return total > limit
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
