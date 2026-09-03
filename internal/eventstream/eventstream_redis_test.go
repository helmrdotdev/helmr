package eventstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

func TestPublishEventOutboxBatchPipelinesInClaimOrder(t *testing.T) {
	client := openTestRedis(t, nil)
	recorder := &pipelineRecorder{}
	client.AddHook(recorder)
	stream := &Stream{redis: client}
	rows := make([]db.ClaimLiveTelemetryOutboxRow, 100)
	for index := range rows {
		rows[index] = testLiveTelemetryRow(int64(index+1), fmt.Sprintf("test:events:%03d", index))
	}

	for index, err := range stream.publishEventOutboxBatch(t.Context(), rows) {
		if err != nil {
			t.Fatalf("publish result %d: %v", index, err)
		}
	}
	if calls := recorder.pipelineCalls(); calls != 1 {
		t.Fatalf("pipeline calls = %d, want 1", calls)
	}
	if keys := recorder.xaddKeys(); len(keys) != len(rows) {
		t.Fatalf("recorded XADD keys = %d, want %d", len(keys), len(rows))
	} else {
		for index, key := range keys {
			if key != rows[index].StreamKey {
				t.Fatalf("XADD key %d = %q, want %q", index, key, rows[index].StreamKey)
			}
		}
	}
	for _, row := range rows {
		records, err := client.XRangeN(t.Context(), row.StreamKey, "-", "+", 1).Result()
		if err != nil || len(records) != 1 || records[0].ID != redisEventID(row.Seq) {
			t.Fatalf("stream %q records = %+v err=%v", row.StreamKey, records, err)
		}
	}
	for index, err := range stream.publishEventOutboxBatch(t.Context(), rows) {
		if err != nil {
			t.Fatalf("replay result %d: %v", index, err)
		}
	}
	if calls := recorder.pipelineCalls(); calls != 3 {
		t.Fatalf("pipeline calls after 100-row replay = %d, want one XADD and one verification pipeline after the initial call", calls)
	}
}

func TestLiveTelemetryClaimBatchRejectsInvalidShape(t *testing.T) {
	if err := validateLiveTelemetryClaimBatch([]int64{1}, nil); err == nil {
		t.Fatal("mismatched claim cardinality was accepted")
	}
}

func TestRunPublisherBacksOffAfterPartialClaimLoss(t *testing.T) {
	client := openTestRedis(t, nil)
	store := &partialLiveCompletionStore{
		rows: []db.ClaimLiveTelemetryOutboxRow{
			testLiveTelemetryRow(101, "test:events:partial:1"),
			testLiveTelemetryRow(102, "test:events:partial:2"),
		},
		marked: make(chan struct{}),
	}
	stream := &Stream{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: store, redis: client,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- stream.RunPublisher(ctx) }()
	select {
	case <-store.marked:
	case <-time.After(time.Second):
		t.Fatal("publisher did not persist the partial completion")
	}
	time.Sleep(50 * time.Millisecond)
	if calls := store.claimCalls.Load(); calls != 1 {
		t.Fatalf("claim calls during completion backoff = %d, want 1", calls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("publisher exit = %v", err)
	}
}

func TestPublishEventOutboxBatchResolvesReplay(t *testing.T) {
	client := openTestRedis(t, nil)
	stream := &Stream{redis: client}

	t.Run("matching duplicate", func(t *testing.T) {
		row := testLiveTelemetryRow(11, "test:events:duplicate")
		if err := stream.publishEventOutboxBatch(t.Context(), []db.ClaimLiveTelemetryOutboxRow{row})[0]; err != nil {
			t.Fatal(err)
		}
		if err := stream.publishEventOutboxBatch(t.Context(), []db.ClaimLiveTelemetryOutboxRow{row})[0]; err != nil {
			t.Fatalf("replay: %v", err)
		}
	})

	t.Run("conflicting duplicate", func(t *testing.T) {
		row := testLiveTelemetryRow(21, "test:events:conflict")
		if err := client.XAdd(t.Context(), &redis.XAddArgs{
			Stream: row.StreamKey,
			ID:     redisEventID(row.Seq),
			Values: map[string]any{"event": `{"wrong":true}`},
		}).Err(); err != nil {
			t.Fatal(err)
		}
		err := stream.publishEventOutboxBatch(t.Context(), []db.ClaimLiveTelemetryOutboxRow{row})[0]
		if err == nil || !strings.Contains(err.Error(), "conflicts with outbox") {
			t.Fatalf("conflicting replay error = %v", err)
		}
	})

	t.Run("advanced", func(t *testing.T) {
		row := testLiveTelemetryRow(31, "test:events:advanced")
		if err := client.XAdd(t.Context(), &redis.XAddArgs{
			Stream: row.StreamKey,
			ID:     redisEventID(row.Seq + 1),
			Values: map[string]any{"event": `{}`},
		}).Err(); err != nil {
			t.Fatal(err)
		}
		if err := stream.publishEventOutboxBatch(t.Context(), []db.ClaimLiveTelemetryOutboxRow{row})[0]; err != nil {
			t.Fatalf("advanced replay: %v", err)
		}
	})

	t.Run("trimmed", func(t *testing.T) {
		row := testLiveTelemetryRow(41, "test:events:trimmed")
		if err := stream.publishEventOutboxBatch(t.Context(), []db.ClaimLiveTelemetryOutboxRow{row})[0]; err != nil {
			t.Fatal(err)
		}
		if err := client.XTrimMaxLen(t.Context(), row.StreamKey, 0).Err(); err != nil {
			t.Fatal(err)
		}
		if err := stream.publishEventOutboxBatch(t.Context(), []db.ClaimLiveTelemetryOutboxRow{row})[0]; err != nil {
			t.Fatalf("trimmed replay: %v", err)
		}
	})
}

func TestPublishEventOutboxBatchRetriesAfterPrefixWriteDisconnect(t *testing.T) {
	controller := &prefixCutController{}
	client := openTestRedis(t, controller)
	stream := &Stream{redis: client}
	rows := []db.ClaimLiveTelemetryOutboxRow{
		testLiveTelemetryRow(51, "test:events:cut:1"),
		testLiveTelemetryRow(52, "test:events:cut:2"),
		testLiveTelemetryRow(53, "test:events:cut:3"),
	}
	controller.arm()

	for index, err := range stream.publishEventOutboxBatch(t.Context(), rows) {
		if err != nil {
			t.Fatalf("publish result %d: %v", index, err)
		}
	}
	if !controller.didCut() {
		t.Fatal("test connection did not cut the pipeline after its first XADD")
	}
	for _, row := range rows {
		records, err := client.XRangeN(t.Context(), row.StreamKey, "-", "+", 1).Result()
		if err != nil || len(records) != 1 || records[0].ID != redisEventID(row.Seq) {
			t.Fatalf("stream %q records = %+v err=%v", row.StreamKey, records, err)
		}
	}
}

func TestRunPublisherCompletesRedisReplayAfterDatabaseGap(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	client := openTestRedis(t, nil)
	queries := db.New(database.Pool)
	stream := &Stream{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:    queries,
		redis: client,
	}
	outboxID, _ := insertLiveTelemetryTestRow(t, database.Pool, 0, "replay")

	claimed, err := queries.ClaimLiveTelemetryOutbox(t.Context(), db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(liveTelemetryOutboxLeaseDuration),
	})
	if err != nil || len(claimed) != 1 || claimed[0].OutboxID != outboxID {
		t.Fatalf("initial claim = %+v err=%v", claimed, err)
	}
	if err := stream.publishEventOutboxBatch(t.Context(), claimed)[0]; err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), database.Pool, `
		UPDATE telemetry_outbox SET publish_locked_until = NULL WHERE id = $1
	`, outboxID)

	cancel, publisherDone := runTestPublisher(t, stream)
	waitForPublisherCondition(t, func() bool {
		var published bool
		if err := database.Pool.QueryRow(t.Context(), `
			SELECT published_at IS NOT NULL FROM telemetry_outbox WHERE id = $1
		`, outboxID).Scan(&published); err != nil {
			t.Fatal(err)
		}
		return published
	})
	cancel()
	if err := <-publisherDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("publisher exit = %v", err)
	}
	records, err := client.XRangeN(t.Context(), claimed[0].StreamKey, "-", "+", 2).Result()
	if err != nil || len(records) != 1 || records[0].ID != redisEventID(outboxID) {
		t.Fatalf("replayed stream records = %+v err=%v", records, err)
	}
}

func TestRunPublisherPersistsMixedOutcomesByRow(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	client := openTestRedis(t, nil)
	stream := &Stream{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:    db.New(database.Pool),
		redis: client,
	}
	firstID, firstKey := insertLiveTelemetryTestRow(t, database.Pool, 0, "first-conflict")
	secondID, secondKey := insertLiveTelemetryTestRow(t, database.Pool, 5, "second-conflict")
	successID, _ := insertLiveTelemetryTestRow(t, database.Pool, 2, "success")
	for id, key := range map[int64]string{firstID: firstKey, secondID: secondKey} {
		if err := client.XAdd(t.Context(), &redis.XAddArgs{
			Stream: key,
			ID:     redisEventID(id),
			Values: map[string]any{"event": `{"wrong":true}`},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}

	cancel, publisherDone := runTestPublisher(t, stream)
	var firstError, secondError string
	var firstDelay, secondDelay float64
	waitForPublisherCondition(t, func() bool {
		var successPublished bool
		if err := database.Pool.QueryRow(t.Context(), `
			SELECT published_at IS NOT NULL FROM telemetry_outbox WHERE id = $1
		`, successID).Scan(&successPublished); err != nil {
			t.Fatal(err)
		}
		if err := database.Pool.QueryRow(t.Context(), `
			SELECT publish_error,
			       COALESCE(EXTRACT(EPOCH FROM (publish_locked_until - updated_at)), 0)::double precision
			  FROM telemetry_outbox WHERE id = $1
		`, firstID).Scan(&firstError, &firstDelay); err != nil {
			t.Fatal(err)
		}
		if err := database.Pool.QueryRow(t.Context(), `
			SELECT publish_error,
			       COALESCE(EXTRACT(EPOCH FROM (publish_locked_until - updated_at)), 0)::double precision
			  FROM telemetry_outbox WHERE id = $1
		`, secondID).Scan(&secondError, &secondDelay); err != nil {
			t.Fatal(err)
		}
		return successPublished && firstError != "" && secondError != ""
	})
	cancel()
	if err := <-publisherDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("publisher exit = %v", err)
	}
	if firstError == secondError || !strings.Contains(firstError, redisEventID(firstID)) || !strings.Contains(secondError, redisEventID(secondID)) {
		t.Fatalf("row errors = %q and %q", firstError, secondError)
	}
	if firstDelay != 0.25 || secondDelay != 8 {
		t.Fatalf("retry delays = %v and %v, want 0.25 and 8", firstDelay, secondDelay)
	}
}

func insertLiveTelemetryTestRow(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, attempts int, label string) (int64, string) {
	t.Helper()
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	deploymentID := uuid.NewV7()
	var outboxID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id,
			environment_id, deployment_id, kind, message, publish_attempts
		) VALUES ($1, 'event', 'deployment', $2, $3, $4, $2, 'deployment.ready', $5, $6)
		RETURNING id
	`, orgID, deploymentID, projectID, environmentID, label, attempts).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	return outboxID, fmt.Sprintf("helmr:events:%s:deployment:%s", orgID, deploymentID)
}

func runTestPublisher(t *testing.T, stream *Stream) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- stream.RunPublisher(ctx) }()
	return cancel, done
}

func waitForPublisherCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("publisher condition was not satisfied")
}

func openTestRedis(t *testing.T, cut *prefixCutController) *redis.Client {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("HELMR_TEST_REDIS_URL"))
	if rawURL == "" {
		t.Skip("Redis tests run in the dedicated PostgreSQL integration lane")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	options.MaxRetries = 1
	options.MinRetryBackoff = time.Millisecond
	options.MaxRetryBackoff = time.Millisecond
	if cut != nil {
		dialer := &net.Dialer{Timeout: time.Second}
		options.Dialer = func(ctx context.Context, network string, address string) (net.Conn, error) {
			connection, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &prefixCutConn{Conn: connection, controller: cut}, nil
		}
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.FlushDB(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	return client
}

func testLiveTelemetryRow(seq int64, streamKey string) db.ClaimLiveTelemetryOutboxRow {
	now := time.Unix(1_700_000_000, seq).UTC()
	return db.ClaimLiveTelemetryOutboxRow{
		OutboxID:       seq,
		StreamKind:     db.TelemetryStreamKindEvent,
		StreamKey:      streamKey,
		Attempts:       1,
		Seq:            seq,
		OrgID:          pgvalue.UUID(uuid.NewV7()),
		ProjectID:      pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:  pgvalue.UUID(uuid.NewV7()),
		SourceKind:     "deployment",
		SourceID:       pgvalue.UUID(uuid.NewV7()),
		DeploymentID:   pgvalue.UUID(uuid.NewV7()),
		Category:       "system",
		Severity:       "info",
		Source:         "control",
		Kind:           "deployment.ready",
		Message:        "ready",
		Payload:        []byte(`{"ready":true}`),
		RedactionClass: "internal",
		CreatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		OccurredAt:     pgtype.Timestamptz{Time: now, Valid: true},
	}
}

type pipelineRecorder struct {
	mu    sync.Mutex
	calls int
	keys  []string
}

type partialLiveCompletionStore struct {
	claimCalls atomic.Int32
	rows       []db.ClaimLiveTelemetryOutboxRow
	marked     chan struct{}
}

func (store *partialLiveCompletionStore) ClaimLiveTelemetryOutbox(context.Context, db.ClaimLiveTelemetryOutboxParams) ([]db.ClaimLiveTelemetryOutboxRow, error) {
	if store.claimCalls.Add(1) != 1 {
		return nil, nil
	}
	return store.rows, nil
}

func (store *partialLiveCompletionStore) MarkLiveTelemetryOutboxBatchPublished(_ context.Context, params db.MarkLiveTelemetryOutboxBatchPublishedParams) (int64, error) {
	close(store.marked)
	return int64(len(params.Ids) - 1), nil
}

func (*partialLiveCompletionStore) MarkLiveTelemetryOutboxBatchFailed(context.Context, db.MarkLiveTelemetryOutboxBatchFailedParams) (int64, error) {
	return 0, nil
}

func (recorder *pipelineRecorder) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (recorder *pipelineRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (recorder *pipelineRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		recorder.mu.Lock()
		recorder.calls++
		for _, command := range commands {
			if command.Name() == "xadd" && len(command.Args()) > 1 {
				recorder.keys = append(recorder.keys, fmt.Sprint(command.Args()[1]))
			}
		}
		recorder.mu.Unlock()
		return next(ctx, commands)
	}
}

func (recorder *pipelineRecorder) pipelineCalls() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.calls
}

func (recorder *pipelineRecorder) xaddKeys() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.keys...)
}

type prefixCutController struct {
	armed atomic.Bool
	cut   atomic.Bool
}

func (controller *prefixCutController) arm() {
	controller.armed.Store(true)
}

func (controller *prefixCutController) didCut() bool {
	return controller.cut.Load()
}

type prefixCutConn struct {
	net.Conn
	controller *prefixCutController
}

func (connection *prefixCutConn) Write(payload []byte) (int, error) {
	if !connection.controller.armed.Load() {
		return connection.Conn.Write(payload)
	}
	boundary := secondXAddBoundary(payload)
	if boundary < 0 || !connection.controller.armed.CompareAndSwap(true, false) {
		return connection.Conn.Write(payload)
	}
	written, err := connection.Conn.Write(payload[:boundary])
	if err == nil {
		time.Sleep(20 * time.Millisecond)
		err = io.ErrUnexpectedEOF
	}
	connection.controller.cut.Store(true)
	_ = connection.Conn.Close()
	return written, err
}

func secondXAddBoundary(payload []byte) int {
	marker := []byte("$4\r\nxadd\r\n")
	first := bytes.Index(payload, marker)
	if first < 0 {
		return -1
	}
	second := bytes.Index(payload[first+len(marker):], marker)
	if second < 0 {
		return -1
	}
	second += first + len(marker)
	frame := bytes.LastIndex(payload[:second], []byte("\r\n*"))
	if frame < 0 {
		return -1
	}
	return frame + 2
}

var _ redis.Hook = (*pipelineRecorder)(nil)

var _ net.Conn = (*prefixCutConn)(nil)
