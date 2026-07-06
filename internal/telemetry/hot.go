package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type HotReader struct {
	db db.Querier
}

func NewHotReader(queries db.Querier) *HotReader {
	return &HotReader{db: queries}
}

func (r *HotReader) EventWatermark(ctx context.Context, q EventQuery) (int64, error) {
	return r.db.GetEventWatermark(ctx, db.GetEventWatermarkParams{
		OrgID:         pgvalue.UUID(q.OrgID),
		WorkerGroupID: q.WorkerGroupID,
		SubjectType:   q.SubjectType,
		SubjectID:     pgvalue.UUID(q.SubjectID),
	})
}

func (r *HotReader) RunLogWatermark(ctx context.Context, q RunLogChunkQuery) (int64, error) {
	return r.db.GetRunLogWatermark(ctx, db.GetRunLogWatermarkParams{
		OrgID:         pgvalue.UUID(q.OrgID),
		WorkerGroupID: q.WorkerGroupID,
		RunID:         pgvalue.UUID(q.RunID),
	})
}

func (r *HotReader) TerminalOutputWatermark(ctx context.Context, q TerminalOutputQuery) (int64, error) {
	return r.db.GetTerminalOutputWatermark(ctx, db.GetTerminalOutputWatermarkParams{
		OrgID:         pgvalue.UUID(q.OrgID),
		WorkerGroupID: q.WorkerGroupID,
		WorkspaceID:   pgvalue.UUID(q.WorkspaceID),
		ResourceKind:  q.ResourceKind,
		ResourceID:    pgvalue.UUID(q.ResourceID),
		StreamName:    q.StreamName,
	})
}

func (r *HotReader) DeadLetteredTelemetrySeqs(ctx context.Context, q DeadLetteredTelemetryQuery) ([]int64, error) {
	return r.db.ListDeadLetteredTelemetrySeqs(ctx, db.ListDeadLetteredTelemetrySeqsParams{
		OrgID:         pgvalue.UUID(q.OrgID),
		WorkerGroupID: q.WorkerGroupID,
		StreamKind:    db.TelemetryStreamKind(q.StreamKind),
		SourceKind:    q.SourceKind,
		SourceID:      pgvalue.UUID(q.SourceID),
		StreamName:    "",
		AfterSeq:      q.AfterSeq,
		WatermarkSeq:  q.Watermark,
	})
}

func (r *HotReader) ListEventsAboveWatermark(ctx context.Context, q EventQuery, watermark int64) ([]api.RunEvent, int64, error) {
	rows, err := r.db.ListSubjectEventsAfterWatermark(ctx, db.ListSubjectEventsAfterWatermarkParams{
		OrgID:         pgvalue.UUID(q.OrgID),
		WorkerGroupID: q.WorkerGroupID,
		SubjectType:   db.EventSubjectType(q.SubjectType),
		SubjectID:     pgvalue.UUID(q.SubjectID),
		WatermarkSeq:  watermark,
		Seq:           q.AfterSeq,
		RowLimit:      q.Limit,
	})
	if err != nil {
		return nil, q.AfterSeq, err
	}
	events := make([]api.RunEvent, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		events = append(events, eventFromHot(row))
		last = row.Seq
	}
	return events, last, nil
}

func (r *HotReader) ListRunLogChunksAboveWatermark(ctx context.Context, q RunLogChunkQuery, watermark int64) ([]api.RunLogChunk, int64, error) {
	rows, err := r.db.ListRunLogChunksAfterWatermark(ctx, db.ListRunLogChunksAfterWatermarkParams{
		OrgID:         pgvalue.UUID(q.OrgID),
		WorkerGroupID: q.WorkerGroupID,
		RunID:         pgvalue.UUID(q.RunID),
		WatermarkSeq:  watermark,
		Seq:           q.AfterSeq,
		RowLimit:      q.Limit,
	})
	if err != nil {
		return nil, q.AfterSeq, err
	}
	chunks := make([]api.RunLogChunk, 0, len(rows))
	last := q.AfterSeq
	for _, row := range rows {
		chunks = append(chunks, runLogChunkFromHot(row))
		last = row.Seq
	}
	return chunks, last, nil
}

func (r *HotReader) ListTerminalOutputAboveWatermark(ctx context.Context, q TerminalOutputQuery, watermark int64) ([]TerminalOutputChunk, int64, error) {
	var chunks []TerminalOutputChunk
	last := q.AfterOffset
	switch q.ResourceKind {
	case "workspace_exec":
		rows, err := r.db.ListWorkspaceExecStreamChunksAfterWatermark(ctx, db.ListWorkspaceExecStreamChunksAfterWatermarkParams{
			OrgID:           pgvalue.UUID(q.OrgID),
			WorkerGroupID:   q.WorkerGroupID,
			ProjectID:       pgvalue.UUID(q.ProjectID),
			EnvironmentID:   pgvalue.UUID(q.EnvironmentID),
			WorkspaceID:     pgvalue.UUID(q.WorkspaceID),
			ExecID:          pgvalue.UUID(q.ResourceID),
			Stream:          db.WorkspaceExecStream(q.StreamName),
			WatermarkOffset: watermark,
			CursorOffset:    q.AfterOffset,
			LimitCount:      q.Limit,
		})
		if err != nil {
			return nil, q.AfterOffset, err
		}
		chunks = make([]TerminalOutputChunk, 0, len(rows))
		for _, row := range rows {
			chunks = append(chunks, terminalOutputFromExecHot(row))
			last = row.OffsetEnd
		}
	case "workspace_pty":
		rows, err := r.db.ListWorkspacePtyStreamChunksAfterWatermark(ctx, db.ListWorkspacePtyStreamChunksAfterWatermarkParams{
			OrgID:           pgvalue.UUID(q.OrgID),
			WorkerGroupID:   q.WorkerGroupID,
			ProjectID:       pgvalue.UUID(q.ProjectID),
			EnvironmentID:   pgvalue.UUID(q.EnvironmentID),
			WorkspaceID:     pgvalue.UUID(q.WorkspaceID),
			PtySessionID:    pgvalue.UUID(q.ResourceID),
			Stream:          db.WorkspacePtyStream(q.StreamName),
			WatermarkOffset: watermark,
			CursorOffset:    q.AfterOffset,
			LimitCount:      q.Limit,
		})
		if err != nil {
			return nil, q.AfterOffset, err
		}
		chunks = make([]TerminalOutputChunk, 0, len(rows))
		for _, row := range rows {
			chunks = append(chunks, terminalOutputFromPtyHot(row))
			last = row.OffsetEnd
		}
	default:
		return nil, q.AfterOffset, nil
	}
	return chunks, last, nil
}

func eventFromHot(event db.EventHotPayload) api.RunEvent {
	return eventResponse(event.Seq, event.RunID, event.DeploymentID, event.RunLeaseID, event.AttemptNumber, event.TraceID, event.SpanID, event.Traceparent, event.Category, event.Severity, event.Source, event.Kind, event.Message, event.Payload, event.RedactionClass, event.CreatedAt, event.OccurredAt)
}

func runLogChunkFromHot(chunk db.RunLogHotChunk) api.RunLogChunk {
	return api.RunLogChunk{
		ID:            Cursor(chunk.Seq),
		RunID:         pgvalue.MustUUIDValue(chunk.RunID).String(),
		RunLeaseID:    pgvalue.MustUUIDValue(chunk.RunLeaseID).String(),
		AttemptNumber: chunk.AttemptNumber,
		Stream:        string(chunk.Stream),
		ContentBase64: base64.StdEncoding.EncodeToString(chunk.Content),
		Bytes:         chunk.SizeBytes,
		ObservedSeq:   chunk.ObservedSeq,
		At:            pgvalue.Time(chunk.CreatedAt),
	}
}

func terminalOutputFromExecHot(row db.WorkspaceExecStreamChunk) TerminalOutputChunk {
	return TerminalOutputChunk{
		ID:          pgvalue.MustUUIDValue(row.ID).String(),
		Stream:      string(row.Stream),
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  pgvalue.Time(row.ObservedAt),
		CreatedAt:   pgvalue.Time(row.CreatedAt),
	}
}

func terminalOutputFromPtyHot(row db.WorkspacePtyStreamChunk) TerminalOutputChunk {
	return TerminalOutputChunk{
		ID:          pgvalue.MustUUIDValue(row.ID).String(),
		Stream:      string(row.Stream),
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  pgvalue.Time(row.ObservedAt),
		CreatedAt:   pgvalue.Time(row.CreatedAt),
	}
}

func eventResponse(seq int64, runID pgtype.UUID, deploymentID pgtype.UUID, runLeaseID pgtype.UUID, attemptNumberValue pgtype.Int4, traceIDValue pgtype.Text, spanIDValue pgtype.Text, traceparentValue pgtype.Text, category string, severity string, source string, rawKind string, message string, payload []byte, redactionClass string, createdAt pgtype.Timestamptz, occurredAt pgtype.Timestamptz) api.RunEvent {
	var runIDValue *string
	if runID.Valid {
		value := pgvalue.MustUUIDValue(runID).String()
		runIDValue = &value
	}
	var deploymentIDValue *string
	if deploymentID.Valid {
		value := pgvalue.MustUUIDValue(deploymentID).String()
		deploymentIDValue = &value
	}
	var runLeaseIDValue *string
	if runLeaseID.Valid {
		value := pgvalue.MustUUIDValue(runLeaseID).String()
		runLeaseIDValue = &value
	}
	var attemptNumber *int32
	if attemptNumberValue.Valid {
		attemptNumber = &attemptNumberValue.Int32
	}
	traceID := ""
	if traceIDValue.Valid {
		traceID = traceIDValue.String
	}
	spanID := ""
	if spanIDValue.Valid {
		spanID = spanIDValue.String
	}
	traceparent := ""
	if traceparentValue.Valid {
		traceparent = traceparentValue.String
	}
	attributes := json.RawMessage(payload)
	if len(attributes) == 0 || !json.Valid(attributes) {
		attributes = json.RawMessage(`{}`)
	}
	if redactionClass == "sensitive" {
		attributes = json.RawMessage(`{"redacted":true}`)
	}
	at := pgvalue.Time(createdAt)
	occurred := pgvalue.Time(occurredAt)
	if occurred.IsZero() {
		occurred = at
	}
	return api.RunEvent{
		ID:             Cursor(seq),
		RunID:          runIDValue,
		DeploymentID:   deploymentIDValue,
		RunLeaseID:     runLeaseIDValue,
		AttemptNumber:  attemptNumber,
		Trace:          api.TraceContext{TraceID: traceID, SpanID: spanID, Traceparent: traceparent},
		Category:       category,
		Severity:       severity,
		Source:         source,
		Kind:           rawKind,
		Message:        firstNonEmpty(message, rawKind),
		At:             at,
		OccurredAt:     occurred,
		RedactionClass: redactionClass,
		Attributes:     attributes,
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
