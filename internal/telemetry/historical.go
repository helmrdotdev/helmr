package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
)

type HistoricalReader struct {
	client *clickhouse.Client
}

func NewHistoricalReader(client *clickhouse.Client) *HistoricalReader {
	return &HistoricalReader{client: client}
}

type EventRecord struct {
	CellID         string     `json:"cell_id"`
	OrgID          uuid.UUID  `json:"org_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	EnvironmentID  uuid.UUID  `json:"environment_id"`
	SubjectKind    string     `json:"subject_kind"`
	SubjectID      uuid.UUID  `json:"subject_id"`
	EventKind      string     `json:"event_kind"`
	Seq            uint64     `json:"seq"`
	RunID          *uuid.UUID `json:"run_id,omitempty"`
	DeploymentID   *uuid.UUID `json:"deployment_id,omitempty"`
	AttemptID      *uuid.UUID `json:"attempt_id,omitempty"`
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
	ObservedAt     string     `json:"observed_at"`
}

type RunLogRecord struct {
	CellID         string     `json:"cell_id"`
	OrgID          uuid.UUID  `json:"org_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	EnvironmentID  uuid.UUID  `json:"environment_id"`
	RunID          uuid.UUID  `json:"run_id"`
	AttemptID      *uuid.UUID `json:"attempt_id,omitempty"`
	RunLeaseID     uuid.UUID  `json:"run_lease_id"`
	AttemptNumber  int32      `json:"attempt_number"`
	StreamName     string     `json:"stream_name"`
	Seq            uint64     `json:"seq"`
	ObservedSeq    uint64     `json:"observed_seq"`
	Content        string     `json:"content"`
	SizeBytes      uint64     `json:"size_bytes"`
	IdempotencyKey string     `json:"idempotency_key"`
	RetentionClass string     `json:"retention_class"`
	RedactionClass string     `json:"redaction_class"`
	Source         string     `json:"source"`
	ObservedAt     string     `json:"observed_at"`
}

type TerminalOutputRecord struct {
	CellID         string    `json:"cell_id"`
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
	ObservedAt     string    `json:"observed_at"`
}

func (r *HistoricalReader) ListEvents(ctx context.Context, q EventQuery, watermark int64) ([]api.RunEvent, int64, error) {
	if watermark <= q.AfterSeq {
		return nil, q.AfterSeq, nil
	}
	sql := `SELECT seq, run_id, deployment_id, attempt_id, run_lease_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, event_kind, message, body, redaction_class, observed_at
FROM helmr_telemetry.events FINAL
WHERE org_id = {org_id:UUID}
  AND cell_id = {cell_id:String}
  AND subject_kind = {subject_kind:String}
  AND subject_id = {subject_id:UUID}
  AND seq > {after:UInt64}
  AND seq <= {watermark:UInt64}
ORDER BY seq ASC
LIMIT {row_limit:UInt32} FORMAT JSONEachRow`
	var rows []eventRow
	if err := r.client.SelectJSONEachRowParams(ctx, sql, map[string]string{
		"org_id":       q.OrgID.String(),
		"cell_id":      q.CellID,
		"subject_kind": q.SubjectType,
		"subject_id":   q.SubjectID.String(),
		"after":        strconv.FormatInt(q.AfterSeq, 10),
		"watermark":    strconv.FormatInt(watermark, 10),
		"row_limit":    strconv.FormatInt(int64(q.Limit), 10),
	}, &rows); err != nil {
		return nil, q.AfterSeq, err
	}
	events := make([]api.RunEvent, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		events = append(events, row.event())
		last = int64(row.Seq)
	}
	return events, last, nil
}

func (r *HistoricalReader) ListRunLogChunks(ctx context.Context, q RunLogChunkQuery, watermark int64) ([]api.RunLogChunk, int64, error) {
	if watermark <= q.AfterSeq {
		return nil, q.AfterSeq, nil
	}
	sql := `SELECT run_id, run_lease_id, attempt_id, attempt_number, stream_name, seq, observed_seq, content, size_bytes, observed_at
FROM helmr_telemetry.run_logs FINAL
WHERE org_id = {org_id:UUID}
  AND cell_id = {cell_id:String}
  AND run_id = {run_id:UUID}
  AND seq > {after:UInt64}
  AND seq <= {watermark:UInt64}
ORDER BY seq ASC
LIMIT {row_limit:UInt32} FORMAT JSONEachRow`
	var rows []runLogRow
	if err := r.client.SelectJSONEachRowParams(ctx, sql, map[string]string{
		"org_id":    q.OrgID.String(),
		"cell_id":   q.CellID,
		"run_id":    q.RunID.String(),
		"after":     strconv.FormatInt(q.AfterSeq, 10),
		"watermark": strconv.FormatInt(watermark, 10),
		"row_limit": strconv.FormatInt(int64(q.Limit), 10),
	}, &rows); err != nil {
		return nil, q.AfterSeq, err
	}
	chunks := make([]api.RunLogChunk, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		chunks = append(chunks, row.chunk())
		last = int64(row.Seq)
	}
	return chunks, last, nil
}

func (r *HistoricalReader) ListTerminalOutput(ctx context.Context, q TerminalOutputQuery, watermark int64) ([]TerminalOutputChunk, int64, error) {
	if watermark <= q.AfterOffset {
		return nil, q.AfterOffset, nil
	}
	sql := `SELECT stream_name, offset_start, offset_end, content, observed_at, ingested_at
FROM helmr_telemetry.terminal_output FINAL
WHERE org_id = {org_id:UUID}
  AND cell_id = {cell_id:String}
  AND project_id = {project_id:UUID}
  AND environment_id = {environment_id:UUID}
  AND workspace_id = {workspace_id:UUID}
  AND resource_kind = {resource_kind:String}
  AND resource_id = {resource_id:UUID}
  AND stream_name = {stream_name:String}
  AND offset_end > {after:UInt64}
  AND offset_end <= {watermark:UInt64}
ORDER BY offset_start ASC
LIMIT {row_limit:UInt32} FORMAT JSONEachRow`
	var rows []terminalOutputHistoryRow
	if err := r.client.SelectJSONEachRowParams(ctx, sql, map[string]string{
		"org_id":         q.OrgID.String(),
		"cell_id":        q.CellID,
		"project_id":     q.ProjectID.String(),
		"environment_id": q.EnvironmentID.String(),
		"workspace_id":   q.WorkspaceID.String(),
		"resource_kind":  q.ResourceKind,
		"resource_id":    q.ResourceID.String(),
		"stream_name":    q.StreamName,
		"after":          strconv.FormatInt(q.AfterOffset, 10),
		"watermark":      strconv.FormatInt(watermark, 10),
		"row_limit":      strconv.FormatInt(int64(q.Limit), 10),
	}, &rows); err != nil {
		return nil, q.AfterOffset, err
	}
	chunks := make([]TerminalOutputChunk, 0, len(rows))
	last := q.AfterOffset
	for _, row := range rows {
		chunks = append(chunks, row.chunk(q.ResourceKind, q.ResourceID))
		last = int64(row.OffsetEnd)
	}
	return chunks, last, nil
}

type eventRow struct {
	Seq            uint64     `json:"seq"`
	RunID          *uuid.UUID `json:"run_id"`
	DeploymentID   *uuid.UUID `json:"deployment_id"`
	AttemptID      *uuid.UUID `json:"attempt_id"`
	RunLeaseID     *uuid.UUID `json:"run_lease_id"`
	AttemptNumber  *int32     `json:"attempt_number"`
	TraceID        string     `json:"trace_id"`
	SpanID         string     `json:"span_id"`
	Traceparent    string     `json:"traceparent"`
	Category       string     `json:"category"`
	Severity       string     `json:"severity"`
	Source         string     `json:"source"`
	EventKind      string     `json:"event_kind"`
	Message        string     `json:"message"`
	Body           string     `json:"body"`
	RedactionClass string     `json:"redaction_class"`
	ObservedAt     string     `json:"observed_at"`
}

func (r eventRow) event() api.RunEvent {
	var runID, deploymentID, runLeaseID, attemptID *string
	if r.RunID != nil {
		value := r.RunID.String()
		runID = &value
	}
	if r.DeploymentID != nil {
		value := r.DeploymentID.String()
		deploymentID = &value
	}
	if r.RunLeaseID != nil {
		value := r.RunLeaseID.String()
		runLeaseID = &value
	}
	if r.AttemptID != nil {
		value := r.AttemptID.String()
		attemptID = &value
	}
	at := parseTime(r.ObservedAt)
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
		RunLeaseID:     runLeaseID,
		AttemptID:      attemptID,
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
	RunID         uuid.UUID  `json:"run_id"`
	RunLeaseID    uuid.UUID  `json:"run_lease_id"`
	AttemptID     *uuid.UUID `json:"attempt_id"`
	AttemptNumber int32      `json:"attempt_number"`
	StreamName    string     `json:"stream_name"`
	Seq           uint64     `json:"seq"`
	ObservedSeq   uint64     `json:"observed_seq"`
	Content       string     `json:"content"`
	SizeBytes     uint64     `json:"size_bytes"`
	ObservedAt    string     `json:"observed_at"`
}

type terminalOutputHistoryRow struct {
	StreamName  string `json:"stream_name"`
	OffsetStart uint64 `json:"offset_start"`
	OffsetEnd   uint64 `json:"offset_end"`
	Content     string `json:"content"`
	ObservedAt  string `json:"observed_at"`
	IngestedAt  string `json:"ingested_at"`
}

func (r terminalOutputHistoryRow) chunk(resourceKind string, resourceID uuid.UUID) TerminalOutputChunk {
	content, err := base64.StdEncoding.DecodeString(r.Content)
	if err != nil {
		content = []byte(r.Content)
	}
	observed := parseTime(r.ObservedAt)
	created := parseTime(r.IngestedAt)
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
		RunLeaseID:    r.RunLeaseID.String(),
		AttemptNumber: r.AttemptNumber,
		Stream:        r.StreamName,
		ContentBase64: contentBase64,
		Bytes:         int64(r.SizeBytes),
		ObservedSeq:   int64(r.ObservedSeq),
		At:            parseTime(r.ObservedAt),
	}
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return parsed.UTC()
		}
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(unix, 0).UTC()
	}
	return time.Time{}
}
