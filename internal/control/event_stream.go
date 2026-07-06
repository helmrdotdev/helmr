package control

import (
	"context"
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
	eventOutboxBatchSize     = int32(100)
	eventOutboxLeaseDuration = 30 * time.Second
	eventPublisherIdleEvery  = 100 * time.Millisecond
	eventPublisherRetryMin   = 250 * time.Millisecond
	eventPublisherRetryMax   = 30 * time.Second
	eventStreamBlockEvery    = 25 * time.Second
	eventStreamMaxLen        = int64(10000)
)

type EventStream struct {
	log             *slog.Logger
	db              db.Querier
	redis           redis.Cmdable
	workerGroupID   string
	telemetryReader telemetry.Reader
}

type EventStreamConfig struct {
	WorkerGroupID   string
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
	if strings.TrimSpace(cfg.WorkerGroupID) == "" {
		return nil, errors.New("event stream worker group id is required")
	}
	reader := cfg.TelemetryReader
	if reader == nil {
		return nil, errors.New("event stream telemetry reader is required")
	}
	return &EventStream{log: log, db: queries, redis: redis, workerGroupID: cfg.WorkerGroupID, telemetryReader: reader}, nil
}

func (s *EventStream) RunPublisher(ctx context.Context) error {
	consecutiveFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := s.db.ClaimEventOutbox(ctx, db.ClaimEventOutboxParams{
			RowLimit:      eventOutboxBatchSize,
			LeaseDuration: pgvalue.Interval(eventOutboxLeaseDuration),
		})
		if err != nil {
			consecutiveFailures++
			s.log.Warn("claim event outbox failed", "error", err)
			if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(consecutiveFailures)); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		consecutiveFailures = 0
		if len(claimed) == 0 {
			if err := sleepWithContext(ctx, eventPublisherIdleEvery); err != nil {
				return err
			}
			continue
		}
		for _, row := range claimed {
			if err := s.publishOutboxRow(ctx, row); err != nil {
				s.log.Warn("publish event outbox row failed", "outbox_id", row.OutboxID, "error", err)
				if markErr := s.db.MarkEventOutboxFailed(ctx, db.MarkEventOutboxFailedParams{
					ID:         row.OutboxID,
					LastError:  err.Error(),
					RetryAfter: pgvalue.Interval(eventPublisherBackoff(int(row.Attempts))),
				}); markErr != nil {
					s.log.Warn("mark event outbox failed", "outbox_id", row.OutboxID, "error", markErr)
					if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(int(row.Attempts))); sleepErr != nil {
						return sleepErr
					}
				}
				continue
			}
			if err := s.db.MarkEventOutboxPublished(ctx, row.OutboxID); err != nil {
				s.log.Warn("mark event outbox published failed", "outbox_id", row.OutboxID, "error", err)
				if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(int(row.Attempts))); sleepErr != nil {
					return sleepErr
				}
			}
		}
	}
}

func (s *EventStream) publishOutboxRow(ctx context.Context, row db.ClaimEventOutboxRow) error {
	event := eventResponseFromClaim(row)
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	id := redisEventID(row.Seq)
	add := func() error {
		return s.redis.XAdd(ctx, &redis.XAddArgs{
			Stream: row.StreamKey,
			MaxLen: eventStreamMaxLen,
			Approx: true,
			ID:     id,
			Values: map[string]any{"event": string(payload)},
		}).Err()
	}
	err = add()
	if err == nil {
		return nil
	}
	if !redisIDAlreadyExists(err) {
		return err
	}
	records, rangeErr := s.redis.XRangeN(ctx, row.StreamKey, id, id, 1).Result()
	if rangeErr != nil {
		return rangeErr
	}
	if len(records) == 0 {
		if advanced, advancedErr := s.streamAdvancedPastID(ctx, row.StreamKey, row.Seq); advancedErr != nil {
			return advancedErr
		} else if advanced {
			return nil
		}
		return err
	}
	existing, ok := records[0].Values["event"].(string)
	if !ok || existing != string(payload) {
		return fmt.Errorf("event stream record %s conflicts with outbox event", id)
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

func (s *EventStream) ReadSubject(ctx context.Context, orgID uuid.UUID, workerGroupID string, subjectType db.EventSubjectType, subjectID uuid.UUID, cursor int64, onEvent func(api.RunEvent) error, onIdle func() error) error {
	streamKey := eventStreamKey(orgID, workerGroupID, subjectType, subjectID)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nextCursor, hasMore, err := s.readDurableSubjectEvents(ctx, orgID, workerGroupID, subjectType, subjectID, cursor, onEvent)
		if err != nil {
			return err
		}
		if nextCursor > cursor {
			cursor = nextCursor
		}
		if hasMore {
			continue
		}
		streams, err := s.redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, redisEventID(cursor)},
			Count:   int64(runEventsPageSize),
			Block:   eventStreamBlockEvery,
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

func (s *EventStream) readDurableSubjectEvents(ctx context.Context, orgID uuid.UUID, workerGroupID string, subjectType db.EventSubjectType, subjectID uuid.UUID, cursor int64, onEvent func(api.RunEvent) error) (int64, bool, error) {
	page, err := s.telemetryReader.ListEvents(ctx, telemetry.EventQuery{
		OrgID:         orgID,
		WorkerGroupID: workerGroupID,
		SubjectType:   string(subjectType),
		SubjectID:     subjectID,
		AfterSeq:      cursor,
		Limit:         runEventsPageSize,
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

func eventStreamKey(orgID uuid.UUID, workerGroupID string, subjectType db.EventSubjectType, subjectID uuid.UUID) string {
	return "helmr:events:" + orgID.String() + ":" + workerGroupID + ":" + string(subjectType) + ":" + subjectID.String()
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

func eventPublisherBackoff(attempts int) time.Duration {
	if attempts < 1 {
		return eventPublisherRetryMin
	}
	backoff := eventPublisherRetryMin
	for i := 1; i < attempts; i++ {
		backoff *= 2
		if backoff >= eventPublisherRetryMax {
			return eventPublisherRetryMax
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

func eventResponseFromClaim(event db.ClaimEventOutboxRow) api.RunEvent {
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
