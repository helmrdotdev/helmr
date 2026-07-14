package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	liveTelemetryOutboxBatchSize     = int32(100)
	liveTelemetryOutboxLeaseDuration = 30 * time.Second
	liveTelemetryPublisherIdleEvery  = 100 * time.Millisecond
	liveTelemetryPublisherRetryMin   = 250 * time.Millisecond
	liveTelemetryPublisherRetryMax   = 30 * time.Second
	liveTelemetryStreamBlockEvery    = time.Second
	liveTelemetryStreamMaxLen        = int64(10000)
)

const (
	eventSubjectTypeRun        = "run"
	eventSubjectTypeDeployment = "deployment"
)

var errLiveTelemetryFollowComplete = errors.New("live telemetry follow complete")

type EventStream struct {
	log             *slog.Logger
	db              db.Querier
	redis           redis.Cmdable
	telemetryReader telemetry.Reader
}

type EventStreamConfig struct {
	TelemetryReader telemetry.Reader
}

func NewEventStream(log *slog.Logger, queries db.Querier, redis redis.Cmdable, configs ...EventStreamConfig) (*EventStream, error) {
	if queries == nil {
		return nil, errors.New("event stream database is required")
	}
	if redis == nil {
		return nil, errors.New("event stream redis client is required")
	}
	if log == nil {
		log = slog.Default()
	}
	var cfg EventStreamConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}
	reader := cfg.TelemetryReader
	if reader == nil {
		return nil, errors.New("event stream telemetry reader is required")
	}
	return &EventStream{log: log, db: queries, redis: redis, telemetryReader: reader}, nil
}

func (s *EventStream) RunPublisher(ctx context.Context) error {
	consecutiveFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := s.db.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
			RowLimit:      liveTelemetryOutboxBatchSize,
			LeaseDuration: pgvalue.Interval(liveTelemetryOutboxLeaseDuration),
		})
		if err != nil {
			consecutiveFailures++
			s.log.Warn("claim live telemetry outbox failed", "error", err)
			if sleepErr := sleepWithContext(ctx, liveTelemetryPublisherBackoff(consecutiveFailures)); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		consecutiveFailures = 0
		if len(claimed) == 0 {
			if err := sleepWithContext(ctx, liveTelemetryPublisherIdleEvery); err != nil {
				return err
			}
			continue
		}
		for _, row := range claimed {
			if err := s.publishOutboxRow(ctx, row); err != nil {
				s.log.Warn("publish live telemetry outbox row failed", "outbox_id", row.OutboxID, "stream_kind", row.StreamKind, "error", err)
				if markErr := s.db.MarkLiveTelemetryOutboxFailed(ctx, db.MarkLiveTelemetryOutboxFailedParams{
					ID:         row.OutboxID,
					LastError:  err.Error(),
					RetryAfter: pgvalue.Interval(liveTelemetryPublisherBackoff(int(row.Attempts))),
				}); markErr != nil {
					s.log.Warn("mark live telemetry outbox failed", "outbox_id", row.OutboxID, "error", markErr)
					if sleepErr := sleepWithContext(ctx, liveTelemetryPublisherBackoff(int(row.Attempts))); sleepErr != nil {
						return sleepErr
					}
				}
				continue
			}
			if err := s.db.MarkLiveTelemetryOutboxPublished(ctx, row.OutboxID); err != nil {
				s.log.Warn("mark live telemetry outbox published failed", "outbox_id", row.OutboxID, "error", err)
				if sleepErr := sleepWithContext(ctx, liveTelemetryPublisherBackoff(int(row.Attempts))); sleepErr != nil {
					return sleepErr
				}
			}
		}
	}
}

func (s *EventStream) publishOutboxRow(ctx context.Context, row db.ClaimLiveTelemetryOutboxRow) error {
	switch row.StreamKind {
	case db.TelemetryStreamKindEvent:
		return s.publishEventOutboxRow(ctx, row)
	case db.TelemetryStreamKindRunLog:
		return s.publishRunLogOutboxRow(ctx, row)
	case db.TelemetryStreamKindTerminalOutput:
		return s.publishTerminalOutputOutboxRow(ctx, row)
	default:
		return fmt.Errorf("unsupported live telemetry stream kind %q", row.StreamKind)
	}
}

func (s *EventStream) publishEventOutboxRow(ctx context.Context, row db.ClaimLiveTelemetryOutboxRow) error {
	event := eventResponseFromClaim(row)
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	return s.publishJSON(ctx, row.StreamKey, redisEventID(row.Seq), "event", payload, row.Seq)
}

func (s *EventStream) publishRunLogOutboxRow(ctx context.Context, row db.ClaimLiveTelemetryOutboxRow) error {
	chunk := runLogChunkResponseFromClaim(row)
	payload, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("encode run log: %w", err)
	}
	return s.publishJSON(ctx, row.StreamKey, redisEventID(row.Seq), "run_log", payload, row.Seq)
}

func (s *EventStream) publishTerminalOutputOutboxRow(ctx context.Context, row db.ClaimLiveTelemetryOutboxRow) error {
	chunk := terminalOutputChunkFromClaim(row)
	payload, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("encode terminal output: %w", err)
	}
	return s.publishJSON(ctx, row.StreamKey, redisEventID(row.OffsetEnd), "terminal_output", payload, row.OffsetEnd)
}

func (s *EventStream) publishJSON(ctx context.Context, streamKey string, id string, field string, payload []byte, seq int64) error {
	add := func() error {
		return s.redis.XAdd(ctx, &redis.XAddArgs{
			Stream: streamKey,
			MaxLen: liveTelemetryStreamMaxLen,
			Approx: true,
			ID:     id,
			Values: map[string]any{field: string(payload)},
		}).Err()
	}
	err := add()
	if err == nil {
		return nil
	}
	if !redisIDAlreadyExists(err) {
		return err
	}
	records, rangeErr := s.redis.XRangeN(ctx, streamKey, id, id, 1).Result()
	if rangeErr != nil {
		return rangeErr
	}
	if len(records) == 0 {
		if advanced, advancedErr := s.streamAdvancedPastID(ctx, streamKey, seq); advancedErr != nil {
			return advancedErr
		} else if advanced {
			return nil
		}
		return err
	}
	existing, ok := records[0].Values[field].(string)
	if !ok || existing != string(payload) {
		return fmt.Errorf("live telemetry stream record %s conflicts with outbox %s", id, field)
	}
	return nil
}

func (s *EventStream) streamAdvancedPastID(ctx context.Context, streamKey string, seq int64) (bool, error) {
	records, err := s.redis.XRevRangeN(ctx, streamKey, "+", "-", 1).Result()
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return false, nil
	}
	latestSeq, err := redisSeq(records[0].ID)
	if err != nil {
		return false, err
	}
	return latestSeq > seq, nil
}

func (s *EventStream) ReadSubject(ctx context.Context, orgID uuid.UUID, subjectType string, subjectID uuid.UUID, cursor int64, onEvent func(api.RunEvent) error, onIdle func() error) error {
	streamKey := eventStreamKey(orgID, subjectType, subjectID)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nextCursor, hasMore, err := s.readDurableSubjectEvents(ctx, orgID, subjectType, subjectID, cursor, onEvent)
		durableErr := err
		if err != nil {
			s.log.Debug("read durable subject events failed; continuing with redis live stream", "subject_type", subjectType, "subject_id", subjectID.String(), "error", err)
			hasMore = false
		}
		if nextCursor > cursor {
			cursor = nextCursor
		}
		if hasMore {
			continue
		}
		if durableErr != nil {
			covers, coverErr := s.redisEventStreamCoversCursor(ctx, streamKey, cursor)
			if coverErr != nil {
				return coverErr
			}
			if !covers {
				return durableErr
			}
		}
		streams, err := s.redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, redisEventID(cursor)},
			Count:   int64(runEventsPageSize),
			Block:   liveTelemetryStreamBlockEvery,
		}).Result()
		if errors.Is(err, redis.Nil) {
			if onIdle != nil {
				if idleErr := onIdle(); idleErr != nil {
					return idleErr
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, stream := range streams {
			for _, message := range stream.Messages {
				seq, err := redisSeq(message.ID)
				if err != nil {
					return err
				}
				raw, ok := message.Values["event"].(string)
				if !ok {
					return fmt.Errorf("event stream record %s missing event field", message.ID)
				}
				var event api.RunEvent
				if err := json.Unmarshal([]byte(raw), &event); err != nil {
					return fmt.Errorf("decode event stream record %s: %w", message.ID, err)
				}
				cursor = seq
				if err := onEvent(event); err != nil {
					return err
				}
			}
		}
	}
}

func (s *EventStream) ReadRunLogs(ctx context.Context, orgID uuid.UUID, runID uuid.UUID, cursor int64, onChunk func(api.RunLogChunk) error, onIdle func() error) error {
	streamKey := runLogStreamKey(orgID, runID)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nextCursor, hasMore, err := s.readDurableRunLogs(ctx, orgID, runID, cursor, onChunk)
		durableErr := err
		if err != nil {
			s.log.Debug("read durable run logs failed; continuing with redis live stream", "run_id", runID.String(), "error", err)
			hasMore = false
		}
		if nextCursor > cursor {
			cursor = nextCursor
		}
		if hasMore {
			continue
		}
		if durableErr != nil {
			covers, coverErr := s.redisEventStreamCoversCursor(ctx, streamKey, cursor)
			if coverErr != nil {
				return coverErr
			}
			if !covers {
				return durableErr
			}
		}
		streams, err := s.redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, redisEventID(cursor)},
			Count:   int64(runLogStreamBatchSize),
			Block:   liveTelemetryStreamBlockEvery,
		}).Result()
		if errors.Is(err, redis.Nil) {
			if onIdle != nil {
				if idleErr := onIdle(); idleErr != nil {
					return idleErr
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, stream := range streams {
			for _, message := range stream.Messages {
				seq, err := redisSeq(message.ID)
				if err != nil {
					return err
				}
				raw, ok := message.Values["run_log"].(string)
				if !ok {
					return fmt.Errorf("run log stream record %s missing run_log field", message.ID)
				}
				var chunk api.RunLogChunk
				if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
					return fmt.Errorf("decode run log stream record %s: %w", message.ID, err)
				}
				cursor = seq
				if err := onChunk(chunk); err != nil {
					return err
				}
			}
		}
	}
}

func (s *EventStream) readDurableRunLogs(ctx context.Context, orgID uuid.UUID, runID uuid.UUID, cursor int64, onChunk func(api.RunLogChunk) error) (int64, bool, error) {
	page, err := s.telemetryReader.ListRunLogChunks(ctx, telemetry.RunLogChunkQuery{
		OrgID:    orgID,
		RunID:    runID,
		AfterSeq: cursor,
		Limit:    runLogStreamBatchSize,
	})
	if err != nil {
		return cursor, false, fmt.Errorf("list durable run logs: %w", err)
	}
	for _, chunk := range page.Chunks {
		nextCursor, err := telemetry.ParseCursor(chunk.ID)
		if err != nil {
			return cursor, false, err
		}
		cursor = nextCursor
		if err := onChunk(chunk); err != nil {
			return cursor, false, err
		}
	}
	return cursor, len(page.Chunks) == int(runLogStreamBatchSize), nil
}

func (s *EventStream) ReadTerminalOutput(ctx context.Context, query telemetry.TerminalOutputQuery, cursor int64, limit int32, onChunk func(telemetry.TerminalOutputChunk) error, onIdle func() error) error {
	query.AfterOffset = cursor
	query.Limit = limit
	streamKey := terminalOutputStreamKey(query.OrgID, query.WorkspaceID, query.ResourceKind, query.ResourceID, query.StreamName)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nextCursor, hasMore, err := s.readDurableTerminalOutput(ctx, query, cursor, onChunk)
		durableErr := err
		if err != nil {
			s.log.Debug("read durable terminal output failed; continuing with redis live stream", "workspace_id", query.WorkspaceID.String(), "resource_kind", query.ResourceKind, "resource_id", query.ResourceID.String(), "stream", query.StreamName, "error", err)
			hasMore = false
		}
		if nextCursor > cursor {
			cursor = nextCursor
			query.AfterOffset = cursor
		}
		if hasMore {
			continue
		}
		if durableErr != nil {
			covers, coverErr := s.redisTerminalStreamCoversCursor(ctx, streamKey, cursor)
			if coverErr != nil {
				return coverErr
			}
			if !covers {
				return durableErr
			}
		}
		streams, err := s.redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, redisEventID(cursor)},
			Count:   int64(limit),
			Block:   liveTelemetryStreamBlockEvery,
		}).Result()
		if errors.Is(err, redis.Nil) {
			if onIdle != nil {
				if idleErr := onIdle(); idleErr != nil {
					return idleErr
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, stream := range streams {
			for _, message := range stream.Messages {
				offset, err := redisSeq(message.ID)
				if err != nil {
					return err
				}
				raw, ok := message.Values["terminal_output"].(string)
				if !ok {
					return fmt.Errorf("terminal output stream record %s missing terminal_output field", message.ID)
				}
				var chunk telemetry.TerminalOutputChunk
				if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
					return fmt.Errorf("decode terminal output stream record %s: %w", message.ID, err)
				}
				cursor = offset
				query.AfterOffset = cursor
				if err := onChunk(chunk); err != nil {
					return err
				}
			}
		}
	}
}

func (s *EventStream) readDurableTerminalOutput(ctx context.Context, query telemetry.TerminalOutputQuery, cursor int64, onChunk func(telemetry.TerminalOutputChunk) error) (int64, bool, error) {
	query.AfterOffset = cursor
	page, err := s.telemetryReader.ListTerminalOutput(ctx, query)
	if err != nil {
		return cursor, false, fmt.Errorf("list durable terminal output: %w", err)
	}
	for _, chunk := range page.Chunks {
		cursor = chunk.OffsetEnd
		if err := onChunk(chunk); err != nil {
			return cursor, false, err
		}
	}
	return cursor, len(page.Chunks) == int(query.Limit), nil
}

func (s *EventStream) redisEventStreamCoversCursor(ctx context.Context, streamKey string, cursor int64) (bool, error) {
	if cursor <= 0 {
		return true, nil
	}
	records, err := s.redis.XRangeN(ctx, streamKey, "-", "+", 1).Result()
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return true, nil
	}
	first, err := redisSeq(records[0].ID)
	if err != nil {
		return false, err
	}
	return first <= cursor, nil
}

func (s *EventStream) redisTerminalStreamCoversCursor(ctx context.Context, streamKey string, cursor int64) (bool, error) {
	records, err := s.redis.XRangeN(ctx, streamKey, "-", "+", 1).Result()
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return true, nil
	}
	raw, ok := records[0].Values["terminal_output"].(string)
	if !ok {
		return false, fmt.Errorf("terminal output stream record %s missing terminal_output field", records[0].ID)
	}
	var chunk telemetry.TerminalOutputChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		return false, fmt.Errorf("decode terminal output stream record %s: %w", records[0].ID, err)
	}
	return chunk.OffsetStart <= cursor, nil
}

func (s *EventStream) readDurableSubjectEvents(ctx context.Context, orgID uuid.UUID, subjectType string, subjectID uuid.UUID, cursor int64, onEvent func(api.RunEvent) error) (int64, bool, error) {
	page, err := s.telemetryReader.ListEvents(ctx, telemetry.EventQuery{
		OrgID:       orgID,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		AfterSeq:    cursor,
		Limit:       runEventsPageSize,
	})
	if err != nil {
		return cursor, false, fmt.Errorf("list durable subject events: %w", err)
	}
	for _, event := range page.Events {
		nextCursor, err := telemetry.ParseCursor(event.ID)
		if err != nil {
			return cursor, false, err
		}
		cursor = nextCursor
		if err := onEvent(event); err != nil {
			return cursor, false, err
		}
	}
	return cursor, len(page.Events) == int(runEventsPageSize), nil
}

func eventStreamKey(orgID uuid.UUID, subjectType string, subjectID uuid.UUID) string {
	return "helmr:events:" + orgID.String() + ":" + subjectType + ":" + subjectID.String()
}

func runLogStreamKey(orgID uuid.UUID, runID uuid.UUID) string {
	return "helmr:run_logs:" + orgID.String() + ":" + runID.String()
}

func terminalOutputStreamKey(orgID uuid.UUID, workspaceID uuid.UUID, resourceKind string, resourceID uuid.UUID, streamName string) string {
	return "helmr:terminal_outputs:" + orgID.String() + ":" + workspaceID.String() + ":" + resourceKind + ":" + resourceID.String() + ":" + streamName
}

func redisEventID(seq int64) string {
	if seq <= 0 {
		return "0-0"
	}
	return strconv.FormatInt(seq, 10) + "-0"
}

func redisSeq(id string) (int64, error) {
	before, _, ok := strings.Cut(id, "-")
	if !ok {
		return 0, fmt.Errorf("invalid redis stream id %q", id)
	}
	seq, err := strconv.ParseInt(before, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("invalid redis stream id %q", id)
	}
	return seq, nil
}

func redisIDAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "equal or smaller") || strings.Contains(message, "ID specified in XADD")
}

func liveTelemetryPublisherBackoff(attempts int) time.Duration {
	if attempts < 1 {
		return liveTelemetryPublisherRetryMin
	}
	backoff := liveTelemetryPublisherRetryMin
	for i := 1; i < attempts; i++ {
		backoff *= 2
		if backoff >= liveTelemetryPublisherRetryMax {
			return liveTelemetryPublisherRetryMax
		}
	}
	return backoff
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func eventResponseFromClaim(event db.ClaimLiveTelemetryOutboxRow) api.RunEvent {
	return apiEventResponse(event.Seq, event.RunID, event.DeploymentID, event.RunLeaseID, event.AttemptNumber, event.TraceID, event.SpanID, event.Traceparent, event.Category, event.Severity, event.Source, event.Kind, event.Message, event.Payload, event.RedactionClass, event.CreatedAt, event.OccurredAt)
}

func runLogChunkResponseFromClaim(row db.ClaimLiveTelemetryOutboxRow) api.RunLogChunk {
	attemptNumber := int32(0)
	if row.AttemptNumber.Valid {
		attemptNumber = row.AttemptNumber.Int32
	}
	return api.RunLogChunk{
		ID:            telemetryCursor(row.Seq),
		RunID:         pgvalue.MustUUIDValue(row.RunID).String(),
		AttemptNumber: attemptNumber,
		Stream:        row.StreamName,
		ContentBase64: base64.StdEncoding.EncodeToString(row.Content),
		Bytes:         row.SizeBytes,
		ObservedSeq:   row.ObservedSeq,
		At:            pgvalue.Time(row.CreatedAt),
	}
}

func terminalOutputChunkFromClaim(row db.ClaimLiveTelemetryOutboxRow) telemetry.TerminalOutputChunk {
	return telemetry.TerminalOutputChunk{
		ID:          strconv.FormatInt(row.OffsetEnd, 10),
		Stream:      row.StreamName,
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Content,
		ObservedAt:  pgvalue.Time(row.OccurredAt),
		CreatedAt:   pgvalue.Time(row.CreatedAt),
	}
}

func apiEventResponse(seq int64, runID pgtype.UUID, deploymentID pgtype.UUID, _ pgtype.UUID, attemptNumberValue pgtype.Int4, traceIDValue pgtype.Text, spanIDValue pgtype.Text, traceparentValue pgtype.Text, category string, severity string, source string, rawKind string, message string, payload []byte, redactionClass string, createdAt pgtype.Timestamptz, occurredAt pgtype.Timestamptz) api.RunEvent {
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
	var attemptNumber *int32
	if attemptNumberValue.Valid {
		attemptNumber = &attemptNumberValue.Int32
	}
	kind := rawKind
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
	return api.RunEvent{
		ID:             telemetryCursor(seq),
		RunID:          runIDValue,
		DeploymentID:   deploymentIDValue,
		AttemptNumber:  attemptNumber,
		Trace:          api.TraceContext{TraceID: traceID, SpanID: spanID, Traceparent: traceparent},
		Category:       category,
		Severity:       severity,
		Source:         source,
		Kind:           kind,
		Message:        firstNonEmptyString(message, rawKind),
		At:             pgvalue.Time(createdAt),
		OccurredAt:     pgvalue.Time(occurredAt),
		RedactionClass: redactionClass,
		Attributes:     attributes,
	}
}
