package clickhouse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

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

type eventRow struct {
	Seq            uint64    `ch:"seq"`
	RunID          *string   `ch:"run_id"`
	DeploymentID   *string   `ch:"deployment_id"`
	RunLeaseID     *string   `ch:"run_lease_id"`
	AttemptNumber  *int32    `ch:"attempt_number"`
	TraceID        string    `ch:"trace_id"`
	SpanID         string    `ch:"span_id"`
	Traceparent    string    `ch:"traceparent"`
	Category       string    `ch:"category"`
	Severity       string    `ch:"severity"`
	Source         string    `ch:"source"`
	EventKind      string    `ch:"event_kind"`
	Message        string    `ch:"message"`
	Body           string    `ch:"body"`
	RedactionClass string    `ch:"redaction_class"`
	ObservedAt     time.Time `ch:"observed_at"`
}

func (r eventRow) event() api.RunEvent {
	var runID, deploymentID *string
	if r.RunID != nil {
		value := *r.RunID
		runID = &value
	}
	if r.DeploymentID != nil {
		value := *r.DeploymentID
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
	RunID         string    `ch:"run_id"`
	RunLeaseID    string    `ch:"run_lease_id"`
	AttemptNumber int32     `ch:"attempt_number"`
	StreamName    string    `ch:"stream_name"`
	Seq           uint64    `ch:"seq"`
	ObservedSeq   uint64    `ch:"observed_seq"`
	Content       string    `ch:"content"`
	SizeBytes     uint64    `ch:"size_bytes"`
	ObservedAt    time.Time `ch:"observed_at"`
}

func (r runLogRow) chunk() api.RunLogChunk {
	contentBase64 := r.Content
	if _, err := base64.StdEncoding.DecodeString(r.Content); err != nil {
		contentBase64 = base64.StdEncoding.EncodeToString([]byte(r.Content))
	}
	return api.RunLogChunk{
		ID:            telemetry.Cursor(int64(r.Seq)),
		RunID:         r.RunID,
		AttemptNumber: r.AttemptNumber,
		Stream:        r.StreamName,
		ContentBase64: contentBase64,
		Bytes:         int64(r.SizeBytes),
		ObservedSeq:   int64(r.ObservedSeq),
		At:            r.ObservedAt.UTC(),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
