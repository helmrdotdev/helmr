package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEventSinkOutageUsesOneWriteAndOneFailureUpdate(t *testing.T) {
	store := &fakeIngestStore{}
	writer := &fakeIngestWriter{eventErr: errors.New("sink unavailable")}
	ingester := testIngestor(store, writer)
	candidates := make([]eventIngestCandidate, defaultIngestBatchSize)
	for idx := range candidates {
		candidates[idx].outboxID = int64(idx + 1)
	}

	successes, err := ingester.writeEventCandidates(context.Background(), candidates)
	if err == nil || len(successes) != 0 {
		t.Fatalf("successes = %d err = %v, want failed batch", len(successes), err)
	}
	if writer.eventCalls != 1 || store.failureCalls != 1 || len(store.failedIDs) != int(defaultIngestBatchSize) {
		t.Fatalf("writes = %d failure updates = %d failed IDs = %d", writer.eventCalls, store.failureCalls, len(store.failedIDs))
	}
}

func TestRunLogSinkOutageUsesOneWriteAndOneFailureUpdate(t *testing.T) {
	store := &fakeIngestStore{}
	writer := &fakeIngestWriter{runLogErr: errors.New("sink unavailable")}
	ingester := testIngestor(store, writer)
	candidates := make([]runLogIngestCandidate, defaultIngestBatchSize)
	for idx := range candidates {
		candidates[idx].outboxID = int64(idx + 1)
	}

	successes, err := ingester.writeRunLogCandidates(context.Background(), candidates)
	if err == nil || len(successes) != 0 {
		t.Fatalf("successes = %d err = %v, want failed batch", len(successes), err)
	}
	if writer.runLogCalls != 1 || store.failureCalls != 1 || len(store.failedIDs) != int(defaultIngestBatchSize) {
		t.Fatalf("writes = %d failure updates = %d failed IDs = %d", writer.runLogCalls, store.failureCalls, len(store.failedIDs))
	}
}

func TestEventRejectedRowAdvancesRemainingBatch(t *testing.T) {
	store := &fakeIngestStore{}
	writer := &fakeIngestWriter{eventResult: []RejectedRow{{Index: 0, Err: errors.New("bad row")}}}
	ingester := testIngestor(store, writer)
	candidates := make([]eventIngestCandidate, defaultIngestBatchSize)
	for idx := range candidates {
		candidates[idx].outboxID = int64(idx + 1)
	}

	successes, err := ingester.writeEventCandidates(context.Background(), candidates)
	if err == nil {
		t.Fatal("expected attributed rejection")
	}
	if len(successes) != int(defaultIngestBatchSize)-1 || writer.eventCalls != 1 || store.failureCalls != 1 {
		t.Fatalf("successes = %d writes = %d failure updates = %d", len(successes), writer.eventCalls, store.failureCalls)
	}
	if len(store.failedIDs) != 1 || store.failedIDs[0] != 1 {
		t.Fatalf("failed IDs = %v, want [1]", store.failedIDs)
	}
}

func TestFailureUpdateErrorIsReturned(t *testing.T) {
	updateErr := errors.New("postgres unavailable")
	store := &fakeIngestStore{failureErr: updateErr}
	writer := &fakeIngestWriter{eventErr: errors.New("sink unavailable")}
	ingester := testIngestor(store, writer)

	_, err := ingester.writeEventCandidates(context.Background(), []eventIngestCandidate{{outboxID: 1}})
	if !errors.Is(err, updateErr) {
		t.Fatalf("error = %v, want failure update error", err)
	}
}

func TestRunCancellationStopsIngestAndGC(t *testing.T) {
	store := &fakeIngestStore{}
	ingester := testIngestor(store, &fakeIngestWriter{})
	ingester.idleEvery = time.Hour
	ingester.gcEvery = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ingester.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func TestGCTickUsesFrozenBatchBudget(t *testing.T) {
	store := &fakeIngestStore{pruned: make(chan db.PruneTelemetryOutboxWrittenParams, 1)}
	ingester := testIngestor(store, &fakeIngestWriter{})
	ingester.gcEvery = time.Millisecond
	ingester.gcBatchSize = defaultOutboxGCBatchSize
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ingester.runGC(ctx); close(done) }()
	params := <-store.pruned
	cancel()
	<-done
	if params.RowLimit != defaultOutboxGCBatchSize {
		t.Fatalf("GC row limit = %d, want %d", params.RowLimit, defaultOutboxGCBatchSize)
	}
}

func TestTelemetryClaimsUseFrozenRowAndByteBudgets(t *testing.T) {
	store := &fakeIngestStore{}
	ingester := testIngestor(store, &fakeIngestWriter{})
	ingester.batchSize = defaultIngestBatchSize
	ingester.batchBytes = MaxTelemetryBatchBytes

	if _, err := ingester.ingestEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.ingestRunLogs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.eventClaims) != 1 {
		t.Fatalf("event claims = %d, want 1", len(store.eventClaims))
	}
	if len(store.runLogClaims) != 1 {
		t.Fatalf("run-log claims = %d, want 1", len(store.runLogClaims))
	}
	if params := store.eventClaims[0]; params.RowLimit != defaultIngestBatchSize || params.MaxBatchBytes != MaxTelemetryBatchBytes {
		t.Fatalf("event claim budget = rows %d bytes %d, want rows %d bytes %d",
			params.RowLimit, params.MaxBatchBytes, defaultIngestBatchSize, MaxTelemetryBatchBytes)
	}
	if params := store.runLogClaims[0]; params.RowLimit != defaultIngestBatchSize || params.MaxBatchBytes != MaxTelemetryBatchBytes {
		t.Fatalf("run-log claim budget = rows %d bytes %d, want rows %d bytes %d",
			params.RowLimit, params.MaxBatchBytes, defaultIngestBatchSize, MaxTelemetryBatchBytes)
	}
}

func testIngestor(store ingestStore, writer IngestWriter) *Ingestor {
	return &Ingestor{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: store, writer: writer,
		retryAfter: time.Second,
	}
}

type fakeIngestWriter struct {
	eventCalls  int
	eventResult []RejectedRow
	eventErr    error
	runLogCalls int
	runLogErr   error
}

func (w *fakeIngestWriter) WriteEvents(context.Context, []EventRecord) ([]RejectedRow, error) {
	w.eventCalls++
	return w.eventResult, w.eventErr
}

func (w *fakeIngestWriter) WriteRunLogs(context.Context, []RunLogRecord) ([]RejectedRow, error) {
	w.runLogCalls++
	return nil, w.runLogErr
}

type fakeIngestStore struct {
	failureCalls int
	failedIDs    []int64
	failureErr   error
	pruned       chan db.PruneTelemetryOutboxWrittenParams
	eventClaims  []db.ClaimEventIngestBatchParams
	runLogClaims []db.ClaimRunLogIngestBatchParams
}

func (s *fakeIngestStore) ClaimEventIngestBatch(_ context.Context, params db.ClaimEventIngestBatchParams) ([]db.ClaimEventIngestBatchRow, error) {
	s.eventClaims = append(s.eventClaims, params)
	return nil, nil
}

func (s *fakeIngestStore) ClaimRunLogIngestBatch(_ context.Context, params db.ClaimRunLogIngestBatchParams) ([]db.ClaimRunLogIngestBatchRow, error) {
	s.runLogClaims = append(s.runLogClaims, params)
	return nil, nil
}

func (*fakeIngestStore) MarkTelemetryOutboxWritten(_ context.Context, ids []int64) (int64, error) {
	return int64(len(ids)), nil
}

func (s *fakeIngestStore) MarkTelemetryOutboxBatchFailed(_ context.Context, params db.MarkTelemetryOutboxBatchFailedParams) (int64, error) {
	s.failureCalls++
	s.failedIDs = append([]int64(nil), params.Ids...)
	if s.failureErr != nil {
		return 0, s.failureErr
	}
	return int64(len(params.Ids)), nil
}

func (s *fakeIngestStore) PruneTelemetryOutboxWritten(_ context.Context, params db.PruneTelemetryOutboxWrittenParams) (int64, error) {
	if s.pruned != nil {
		s.pruned <- params
	}
	return 0, nil
}

func (*fakeIngestStore) GetTelemetryOutboxLifecycle(context.Context, pgtype.Interval) (db.GetTelemetryOutboxLifecycleRow, error) {
	return db.GetTelemetryOutboxLifecycleRow{}, nil
}
