package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
)

type HistoricalReader struct {
	client historicalClient
}

type historicalClient interface {
	Select(ctx context.Context, dest any, query string, args ...any) error
}

func NewHistoricalReader(client historicalClient) *HistoricalReader {
	return &HistoricalReader{client: client}
}

type EventRecord struct {
	OrgID          uuid.UUID  `json:"org_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	EnvironmentID  uuid.UUID  `json:"environment_id"`
	SubjectKind    string     `json:"subject_kind"`
	SubjectID      uuid.UUID  `json:"subject_id"`
	EventKind      string     `json:"event_kind"`
	Seq            uint64     `json:"seq"`
	RunID          *uuid.UUID `json:"run_id,omitempty"`
	DeploymentID   *uuid.UUID `json:"deployment_id,omitempty"`
	RunLeaseID     *uuid.UUID `json:"run_lease_id,omitempty"`
	AttemptNumber  *int32     `json:"attempt_number,omitempty"`
	TraceID        string     `json:"trace_id"`
	SpanID         string     `json:"span_id"`
	ParentSpanID   string     `json:"parent_span_id"`
	Traceparent    string     `json:"traceparent"`
	Category       string     `json:"category"`
	Severity       string     `json:"severity"`
	Source         string     `json:"source"`
	Message        string     `json:"message"`
	Body           string     `json:"body"`
	IdempotencyKey string     `json:"idempotency_key"`
	RetentionClass string     `json:"retention_class"`
	RedactionClass string     `json:"redaction_class"`
	ObservedAt     time.Time  `json:"observed_at"`
}

type RunLogRecord struct {
	OrgID          uuid.UUID `json:"org_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	EnvironmentID  uuid.UUID `json:"environment_id"`
	RunID          uuid.UUID `json:"run_id"`
	RunLeaseID     uuid.UUID `json:"run_lease_id"`
	AttemptNumber  int32     `json:"attempt_number"`
	StreamName     string    `json:"stream_name"`
	Seq            uint64    `json:"seq"`
	ObservedSeq    uint64    `json:"observed_seq"`
	Content        string    `json:"content"`
	SizeBytes      uint64    `json:"size_bytes"`
	IdempotencyKey string    `json:"idempotency_key"`
	RetentionClass string    `json:"retention_class"`
	RedactionClass string    `json:"redaction_class"`
	Source         string    `json:"source"`
	ObservedAt     time.Time `json:"observed_at"`
}

type TerminalOutputRecord struct {
	OrgID          uuid.UUID `json:"org_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	EnvironmentID  uuid.UUID `json:"environment_id"`
	WorkspaceID    uuid.UUID `json:"workspace_id"`
	ResourceKind   string    `json:"resource_kind"`
	ResourceID     uuid.UUID `json:"resource_id"`
	StreamName     string    `json:"stream_name"`
	OffsetStart    uint64    `json:"offset_start"`
	OffsetEnd      uint64    `json:"offset_end"`
	Content        string    `json:"content"`
	SizeBytes      uint64    `json:"size_bytes"`
	IdempotencyKey string    `json:"idempotency_key"`
	RetentionClass string    `json:"retention_class"`
	RedactionClass string    `json:"redaction_class"`
	ObservedAt     time.Time `json:"observed_at"`
}

func (r *HistoricalReader) ListEvents(ctx context.Context, q EventQuery) (EventPage, error) {
	sql := `SELECT seq, run_id, deployment_id, run_lease_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, event_kind, message, body, redaction_class, observed_at
FROM helmr_telemetry.events FINAL
WHERE org_id = @org_id
  AND subject_kind = @subject_kind
  AND subject_id = @subject_id
  AND seq > @after
ORDER BY seq ASC
LIMIT @row_limit`
	var rows []eventRow
	if err := r.client.Select(ctx, &rows, sql,
		clickhouse.Named("org_id", q.OrgID),
		clickhouse.Named("subject_kind", q.SubjectType),
		clickhouse.Named("subject_id", q.SubjectID),
		clickhouse.Named("after", uint64(q.AfterSeq)),
		clickhouse.Named("row_limit", uint32(q.Limit)),
	); err != nil {
		return EventPage{}, fmt.Errorf("%w: %v", ErrHistoricalUnavailable, err)
	}
	events := make([]api.RunEvent, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		events = append(events, row.event())
		last = int64(row.Seq)
	}
	return EventPage{Events: events, LastSeq: last, Historical: len(events)}, nil
}

func (r *HistoricalReader) ListRunLogChunks(ctx context.Context, q RunLogChunkQuery) (RunLogChunkPage, error) {
	sql := `SELECT run_id, run_lease_id, attempt_number, stream_name, seq, observed_seq, content, size_bytes, observed_at
FROM helmr_telemetry.run_logs FINAL
WHERE org_id = @org_id
  AND run_id = @run_id
  AND seq > @after
ORDER BY seq ASC
LIMIT @row_limit`
	var rows []runLogRow
	if err := r.client.Select(ctx, &rows, sql,
		clickhouse.Named("org_id", q.OrgID),
		clickhouse.Named("run_id", q.RunID),
		clickhouse.Named("after", uint64(q.AfterSeq)),
		clickhouse.Named("row_limit", uint32(q.Limit)),
	); err != nil {
		return RunLogChunkPage{}, fmt.Errorf("%w: %v", ErrHistoricalUnavailable, err)
	}
	chunks := make([]api.RunLogChunk, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		chunks = append(chunks, row.chunk())
		last = int64(row.Seq)
	}
	return RunLogChunkPage{Chunks: chunks, LastSeq: last, Historical: len(chunks)}, nil
}

func (r *HistoricalReader) ListTerminalOutput(ctx context.Context, q TerminalOutputQuery) (TerminalOutputPage, error) {
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
		clickhouse.Named("org_id", q.OrgID),
		clickhouse.Named("project_id", q.ProjectID),
		clickhouse.Named("environment_id", q.EnvironmentID),
		clickhouse.Named("workspace_id", q.WorkspaceID),
		clickhouse.Named("resource_kind", q.ResourceKind),
		clickhouse.Named("resource_id", q.ResourceID),
		clickhouse.Named("stream_name", q.StreamName),
		clickhouse.Named("after", uint64(q.AfterOffset)),
		clickhouse.Named("row_limit", uint32(q.Limit)),
	); err != nil {
		return TerminalOutputPage{}, fmt.Errorf("%w: %v", ErrHistoricalUnavailable, err)
	}
	chunks := make([]TerminalOutputChunk, 0, len(rows))
	last := q.AfterOffset
	for _, row := range rows {
		chunks = append(chunks, row.chunk(q.ResourceKind, q.ResourceID))
		last = int64(row.OffsetEnd)
	}
	return TerminalOutputPage{Chunks: chunks, LastOffset: last, Historical: len(chunks)}, nil
}

func (r *HistoricalReader) GetRunLogSnapshot(ctx context.Context, q RunLogSnapshotQuery) (RunLogSnapshot, error) {
	var snapshot RunLogSnapshot
	cursor := int64(0)
	const pageLimit = int32(1000)
	for {
		page, err := r.ListRunLogChunks(ctx, RunLogChunkQuery{
			OrgID:    q.OrgID,
			RunID:    q.RunID,
			AfterSeq: cursor,
			Limit:    pageLimit,
		})
		if err != nil {
			return RunLogSnapshot{}, err
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
			if seq, err := ParseCursor(chunk.ID); err == nil && seq > snapshot.Cursor {
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
		ID:             Cursor(int64(r.Seq)),
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

func (r terminalOutputHistoryRow) chunk(resourceKind string, resourceID uuid.UUID) TerminalOutputChunk {
	content, err := base64.StdEncoding.DecodeString(r.Content)
	if err != nil {
		content = []byte(r.Content)
	}
	observed := r.ObservedAt.UTC()
	created := r.IngestedAt.UTC()
	if created.IsZero() {
		created = observed
	}
	return TerminalOutputChunk{
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
		ID:            Cursor(int64(r.Seq)),
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
