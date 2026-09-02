package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"uuid"

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
	subjectPageSize                  = int32(200)
)

type Stream struct {
	log             *slog.Logger
	db              db.Querier
	redis           redis.Cmdable
	telemetryReader telemetry.Reader
}

type Config struct {
	TelemetryReader telemetry.Reader
}

func New(log *slog.Logger, queries db.Querier, redis redis.Cmdable, configs ...Config) (*Stream, error) {
	if queries == nil {
		return nil, errors.New("event stream database is required")
	}
	if redis == nil {
		return nil, errors.New("event stream redis client is required")
	}
	if log == nil {
		log = slog.Default()
	}
	var cfg Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	reader := cfg.TelemetryReader
	if reader == nil {
		return nil, errors.New("event stream telemetry reader is required")
	}
	return &Stream{log: log, db: queries, redis: redis, telemetryReader: reader}, nil
}

func (s *Stream) RunPublisher(ctx context.Context) error {
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
		publishErrors := s.publishEventOutboxBatch(ctx, claimed)
		publishedIDs := make([]int64, 0, len(claimed))
		failedIDs := make([]int64, 0, len(claimed))
		failureRetryAfters := make([]pgtype.Interval, 0, len(claimed))
		failureMessages := make([]string, 0, len(claimed))
		for index, row := range claimed {
			if err := publishErrors[index]; err != nil {
				s.log.Warn("publish live telemetry outbox row failed",
					"outbox_id", row.OutboxID,
					"stream_kind", row.StreamKind,
					"attempts", row.Attempts,
					"pending_age", time.Since(pgvalue.Time(row.CreatedAt)).Truncate(time.Millisecond),
					"error", err,
				)
				retryAfter := liveTelemetryPublisherBackoff(int(row.Attempts))
				failedIDs = append(failedIDs, row.OutboxID)
				failureRetryAfters = append(failureRetryAfters, pgvalue.Interval(retryAfter))
				failureMessages = append(failureMessages, err.Error())
				continue
			}
			publishedIDs = append(publishedIDs, row.OutboxID)
		}

		completionFailed := false
		if len(publishedIDs) > 0 {
			updated, err := s.db.MarkLiveTelemetryOutboxBatchPublished(ctx, publishedIDs)
			if err != nil || updated != int64(len(publishedIDs)) {
				completionFailed = true
				s.log.Warn("mark live telemetry outbox batch published failed",
					"expected", len(publishedIDs), "updated", updated, "error", err)
			}
		}
		if len(failedIDs) > 0 {
			updated, err := s.db.MarkLiveTelemetryOutboxBatchFailed(ctx, db.MarkLiveTelemetryOutboxBatchFailedParams{
				Ids:           failedIDs,
				RetryAfters:   failureRetryAfters,
				PublishErrors: failureMessages,
			})
			if err != nil || updated != int64(len(failedIDs)) {
				completionFailed = true
				s.log.Warn("mark live telemetry outbox batch failed",
					"expected", len(failedIDs), "updated", updated, "error", err)
			}
		}
		if completionFailed {
			if err := sleepWithContext(ctx, liveTelemetryPublisherRetryMin); err != nil {
				return err
			}
		}
	}
}

func (s *Stream) publishEventOutboxBatch(ctx context.Context, rows []db.ClaimLiveTelemetryOutboxRow) []error {
	results := make([]error, len(rows))
	payloads := make([][]byte, len(rows))
	commands := make([]*redis.StringCmd, len(rows))
	pipeline := s.redis.Pipeline()
	for index, row := range rows {
		payload, err := json.Marshal(eventResponseFromClaim(row))
		if err != nil {
			results[index] = fmt.Errorf("encode event: %w", err)
			continue
		}
		payloads[index] = payload
		commands[index] = pipeline.XAdd(ctx, &redis.XAddArgs{
			Stream: row.StreamKey,
			MaxLen: liveTelemetryStreamMaxLen,
			Approx: true,
			ID:     redisEventID(row.Seq),
			Values: map[string]any{"event": string(payload)},
		})
	}
	_, _ = pipeline.Exec(ctx)
	verificationCommands := make([]*redis.XMessageSliceCmd, len(rows))
	verificationPipeline := s.redis.Pipeline()
	for index, command := range commands {
		if command == nil {
			continue
		}
		err := command.Err()
		if redisIDAlreadyExists(err) {
			id := redisEventID(rows[index].Seq)
			verificationCommands[index] = verificationPipeline.XRangeN(ctx, rows[index].StreamKey, id, id, 1)
			continue
		}
		results[index] = err
	}
	_, _ = verificationPipeline.Exec(ctx)
	for index, command := range verificationCommands {
		if command == nil {
			continue
		}
		records, err := command.Result()
		results[index] = verifyPublishedJSON(redisEventID(rows[index].Seq), payloads[index], records, err)
	}
	return results
}

func verifyPublishedJSON(id string, payload []byte, records []redis.XMessage, err error) error {
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	existing, ok := records[0].Values["event"].(string)
	if !ok || existing != string(payload) {
		return fmt.Errorf("live telemetry stream record %s conflicts with outbox event", id)
	}
	return nil
}

func (s *Stream) ReadSubject(ctx context.Context, orgID uuid.UUID, subjectType string, subjectID uuid.UUID, cursor int64, onEvent func(api.RunEvent) error, onIdle func() error) error {
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
			Count:   int64(subjectPageSize),
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

func (s *Stream) redisEventStreamCoversCursor(ctx context.Context, streamKey string, cursor int64) (bool, error) {
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

func (s *Stream) readDurableSubjectEvents(ctx context.Context, orgID uuid.UUID, subjectType string, subjectID uuid.UUID, cursor int64, onEvent func(api.RunEvent) error) (int64, bool, error) {
	page, err := s.telemetryReader.ListEvents(ctx, telemetry.EventQuery{
		OrgID:       orgID,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		AfterSeq:    cursor,
		Limit:       subjectPageSize,
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
	return cursor, len(page.Events) == int(subjectPageSize), nil
}

func eventStreamKey(orgID uuid.UUID, subjectType string, subjectID uuid.UUID) string {
	return "helmr:events:" + orgID.String() + ":" + subjectType + ":" + subjectID.String()
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
		ID:             telemetry.Cursor(seq),
		RunID:          runIDValue,
		DeploymentID:   deploymentIDValue,
		AttemptNumber:  attemptNumber,
		Trace:          api.TraceContext{TraceID: traceID, SpanID: spanID, Traceparent: traceparent},
		Category:       category,
		Severity:       severity,
		Source:         source,
		Kind:           kind,
		Message:        firstNonEmpty(message, rawKind),
		At:             pgvalue.Time(createdAt),
		OccurredAt:     pgvalue.Time(occurredAt),
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
