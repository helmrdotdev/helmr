package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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

func TestTelemetryOutboxClaimBatchRejectsInvalidShape(t *testing.T) {
	if err := validateTelemetryOutboxClaimBatch([]int64{1}, nil); err == nil {
		t.Fatal("mismatched claim cardinality was accepted")
	}
}

func TestPartialIngestCompletionBacksOffBeforeReclaim(t *testing.T) {
	updated := int64(1)
	orgID := pgvalue.NewUUIDv7()
	projectID := pgvalue.NewUUIDv7()
	environmentID := pgvalue.NewUUIDv7()
	subjectID := pgvalue.NewUUIDv7()
	store := &fakeIngestStore{
		writtenResult: &updated,
		written:       make(chan struct{}, 1),
		eventRows: []db.ClaimEventIngestBatchRow{
			{OutboxID: 1, RetryCount: 3, OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, SubjectID: subjectID, Payload: []byte(`{}`)},
			{OutboxID: 2, RetryCount: 4, OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, SubjectID: subjectID, Payload: []byte(`{}`)},
		},
	}
	ingester := testIngestor(store, &fakeIngestWriter{})
	ingester.batchSize = 2
	ingester.batchBytes = MaxTelemetryBatchBytes
	ingester.leaseDuration = time.Minute
	ingester.retryAfter = time.Hour
	ingester.idleEvery = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ingester.runIngest(ctx) }()

	select {
	case <-store.written:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("partial completion was not attempted")
	}
	time.Sleep(50 * time.Millisecond)
	if got := store.eventClaimCalls.Load(); got != 1 {
		cancel()
		t.Fatalf("event claim calls during retry backoff = %d, want 1", got)
	}
	if store.writtenCalls != 1 || !slices.Equal(store.writtenIDs, []int64{1, 2}) ||
		!slices.Equal(store.writtenCounts, []int32{3, 4}) {
		cancel()
		t.Fatalf("written calls/claims = %d %v/%v", store.writtenCalls, store.writtenIDs, store.writtenCounts)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runIngest() error = %v, want canceled", err)
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
	failureCalls    int
	failedIDs       []int64
	failureErr      error
	writtenCalls    int
	writtenIDs      []int64
	writtenCounts   []int32
	writtenResult   *int64
	written         chan struct{}
	pruned          chan db.PruneTelemetryOutboxWrittenParams
	eventClaims     []db.ClaimEventIngestBatchParams
	eventClaimCalls atomic.Int32
	eventRows       []db.ClaimEventIngestBatchRow
	runLogClaims    []db.ClaimRunLogIngestBatchParams
	runLogRows      []db.ClaimRunLogIngestBatchRow
}

func (s *fakeIngestStore) ClaimEventIngestBatch(_ context.Context, params db.ClaimEventIngestBatchParams) ([]db.ClaimEventIngestBatchRow, error) {
	s.eventClaims = append(s.eventClaims, params)
	if s.eventClaimCalls.Add(1) == 1 {
		return s.eventRows, nil
	}
	return nil, nil
}

func (s *fakeIngestStore) ClaimRunLogIngestBatch(_ context.Context, params db.ClaimRunLogIngestBatchParams) ([]db.ClaimRunLogIngestBatchRow, error) {
	s.runLogClaims = append(s.runLogClaims, params)
	return s.runLogRows, nil
}

func (s *fakeIngestStore) MarkTelemetryOutboxWritten(_ context.Context, params db.MarkTelemetryOutboxWrittenParams) (int64, error) {
	s.writtenCalls++
	s.writtenIDs = append([]int64(nil), params.Ids...)
	s.writtenCounts = append([]int32(nil), params.ExpectedRetryCounts...)
	if s.written != nil {
		select {
		case s.written <- struct{}{}:
		default:
		}
	}
	if s.writtenResult != nil {
		return *s.writtenResult, nil
	}
	return int64(len(params.Ids)), nil
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

func validRunLogRow() db.ClaimRunLogIngestBatchRow {
	return db.ClaimRunLogIngestBatchRow{
		OutboxID: 1, RetryCount: 2,
		OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()),
		EnvironmentID: pgvalue.UUID(uuid.NewV7()), RunID: pgvalue.UUID(uuid.NewV7()),
		RunLeaseID: pgvalue.UUID(uuid.NewV7()), AttemptNumber: pgtype.Int4{Int32: 3, Valid: true},
		Content: []byte("hello"),
	}
}

func TestRunLogRecordRequiresLeaseAndPositiveAttempt(t *testing.T) {
	row := validRunLogRow()
	record, err := runLogRecord(row)
	if err != nil || record.RunLeaseID != pgvalue.MustUUIDValue(row.RunLeaseID) || record.AttemptNumber != 3 || record.Content != "aGVsbG8=" {
		t.Fatalf("record = %+v, error = %v", record, err)
	}
	for _, tc := range []struct {
		name    string
		lease   pgtype.UUID
		attempt pgtype.Int4
		field   string
	}{
		{"missing lease", pgtype.UUID{}, row.AttemptNumber, "run_lease_id"},
		{"missing attempt", row.RunLeaseID, pgtype.Int4{}, "attempt_number"},
		{"zero attempt", row.RunLeaseID, pgtype.Int4{Valid: true}, "attempt_number"},
		{"negative attempt", row.RunLeaseID, pgtype.Int4{Int32: -1, Valid: true}, "attempt_number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := row
			invalid.RunLeaseID, invalid.AttemptNumber = tc.lease, tc.attempt
			if _, err := runLogRecord(invalid); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %v, want %s contract error", err, tc.field)
			}
		})
	}
}

func TestIngestRunLogsIsolatesInvalidProvenance(t *testing.T) {
	invalid, valid := validRunLogRow(), validRunLogRow()
	invalid.RunLeaseID.Valid = false
	valid.OutboxID = 2
	store := &fakeIngestStore{runLogRows: []db.ClaimRunLogIngestBatchRow{invalid, valid}}
	writer := &fakeIngestWriter{}
	count, err := testIngestor(store, writer).ingestRunLogs(t.Context())
	if count != 2 || err == nil || !strings.Contains(err.Error(), "run_lease_id") {
		t.Fatalf("count = %d, error = %v", count, err)
	}
	if !slices.Equal(store.failedIDs, []int64{1}) || !slices.Equal(store.writtenIDs, []int64{2}) || writer.runLogCalls != 1 {
		t.Fatalf("failed = %v, written = %v, writes = %d", store.failedIDs, store.writtenIDs, writer.runLogCalls)
	}
}
