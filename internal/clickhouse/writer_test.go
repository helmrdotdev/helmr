package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

func TestClickHouseWriterAppendsTypedBatchRows(t *testing.T) {
	deploymentID := uuid.NewV7()
	runID := uuid.NewV7()
	runLeaseID := uuid.NewV7()
	attemptNumber := int32(2)
	observedAt := time.Date(2026, 7, 3, 1, 2, 3, 456000000, time.UTC)
	client := &fakeBatchClient{}
	writer := NewWriter(client)

	if _, err := writer.WriteEvents(context.Background(), []telemetry.EventRecord{{
		OrgID:          uuid.NewV7(),
		ProjectID:      uuid.NewV7(),
		EnvironmentID:  uuid.NewV7(),
		SubjectKind:    "run",
		SubjectID:      runID,
		EventKind:      "run.started",
		Seq:            7,
		RunID:          &runID,
		DeploymentID:   &deploymentID,
		RunLeaseID:     &runLeaseID,
		AttemptNumber:  &attemptNumber,
		TraceID:        "trace",
		SpanID:         "span",
		ParentSpanID:   "parent",
		Traceparent:    "traceparent",
		Category:       "execution",
		Severity:       "info",
		Source:         "worker",
		Message:        "started",
		Body:           "{}",
		IdempotencyKey: "event-key",
		RetentionClass: "standard",
		RedactionClass: "standard",
		ObservedAt:     observedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	eventBatch := client.takeLast(t)
	assertQueryContains(t, eventBatch.query, "INSERT INTO helmr_telemetry.events", "observed_at")
	assertRowShape(t, eventBatch.rows, 1, 24)
	if got := eventBatch.rows[0][6]; got != uint64(7) {
		t.Fatalf("event seq = %v, want 7", got)
	}
	if got := eventBatch.rows[0][23]; got != observedAt {
		t.Fatalf("event observed_at = %v, want %v", got, observedAt)
	}

	if _, err := writer.WriteRunLogs(context.Background(), []telemetry.RunLogRecord{{
		OrgID:          uuid.NewV7(),
		ProjectID:      uuid.NewV7(),
		EnvironmentID:  uuid.NewV7(),
		RunID:          runID,
		RunLeaseID:     runLeaseID,
		AttemptNumber:  attemptNumber,
		StreamName:     "stdout",
		Seq:            8,
		ObservedSeq:    9,
		Content:        "aGVsbG8=",
		SizeBytes:      5,
		IdempotencyKey: "log-key",
		RetentionClass: "standard",
		RedactionClass: "standard",
		Source:         "worker",
		ObservedAt:     observedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	runLogBatch := client.takeLast(t)
	assertQueryContains(t, runLogBatch.query, "INSERT INTO helmr_telemetry.run_logs", "run_lease_id", "observed_at")
	assertRowShape(t, runLogBatch.rows, 1, 16)
	if got := runLogBatch.rows[0][4]; got != runLeaseID {
		t.Fatalf("run log run_lease_id = %v, want %s", got, runLeaseID)
	}
}

func TestClickHouseWriterKeepsValidRowsAfterAttributedAppendFailure(t *testing.T) {
	client := &fakeBatchClient{appendFailures: []int{1, -1}}
	writer := NewWriter(client)
	rows := make([]telemetry.EventRecord, 3)
	for idx := range rows {
		rows[idx].Body = `{}`
	}

	result, err := writer.WriteEvents(context.Background(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Index != 1 {
		t.Fatalf("rejected = %+v, want row 1", result)
	}
	if len(client.batches) != 2 {
		t.Fatalf("prepared batches = %d, want invalid batch plus rebuilt batch", len(client.batches))
	}
	batch := client.takeLast(t)
	if !batch.sent || len(batch.rows) != 2 {
		t.Fatalf("sent = %v rows = %d, want one send with two rows", batch.sent, len(batch.rows))
	}
	if client.batches[0].sent {
		t.Fatal("invalid batch must not be sent")
	}
}

func TestClickHouseWriterKeepsValidRowsAfterRepeatedAppendFailures(t *testing.T) {
	client := &fakeBatchClient{appendFailures: []int{1, 1, -1}}
	writer := NewWriter(client)
	rows := make([]telemetry.EventRecord, 3)
	for idx := range rows {
		rows[idx].Body = `{}`
	}

	result, err := writer.WriteEvents(context.Background(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].Index != 1 || result[1].Index != 2 {
		t.Fatalf("rejected = %+v, want rows 1 and 2", result)
	}
	if len(client.batches) != 3 {
		t.Fatalf("prepared batches = %d, want two invalid batches plus rebuilt batch", len(client.batches))
	}
	if got := totalSends(client.batches); got != 1 {
		t.Fatalf("send calls = %d, want 1", got)
	}
	if got := totalCloses(client.batches); got != 3 {
		t.Fatalf("close calls = %d, want 3", got)
	}
	if got := len(client.takeLast(t).rows); got != 1 {
		t.Fatalf("sent rows = %d, want 1", got)
	}
}

func TestClickHouseWriterRebuildsRunLogBatchAfterAppendFailure(t *testing.T) {
	client := &fakeBatchClient{appendFailures: []int{0, -1}}
	writer := NewWriter(client)
	rows := []telemetry.RunLogRecord{{Content: "aGVsbG8=", SizeBytes: 5}, {Content: "d29ybGQ=", SizeBytes: 5}}

	result, err := writer.WriteRunLogs(context.Background(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Index != 0 {
		t.Fatalf("rejected = %+v, want row 0", result)
	}
	if got := totalSends(client.batches); got != 1 {
		t.Fatalf("send calls = %d, want 1", got)
	}
	if got := totalCloses(client.batches); got != 2 {
		t.Fatalf("close calls = %d, want 2", got)
	}
	if got := len(client.takeLast(t).rows); got != 1 {
		t.Fatalf("sent rows = %d, want 1", got)
	}
}

func TestClickHouseWriterReturnsSendFailureAndClosesBatch(t *testing.T) {
	sendErr := errors.New("send failed")
	client := &fakeBatchClient{sendErrors: []error{sendErr}}
	writer := NewWriter(client)

	_, err := writer.WriteEvents(context.Background(), []telemetry.EventRecord{{Body: `{}`}})
	if !errors.Is(err, sendErr) {
		t.Fatalf("error = %v, want %v", err, sendErr)
	}
	if got := totalSends(client.batches); got != 1 {
		t.Fatalf("send calls = %d, want 1", got)
	}
	if got := totalCloses(client.batches); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

type fakeBatchClient struct {
	batches        []*fakeBatch
	appendFailures []int
	sendErrors     []error
}

func (c *fakeBatchClient) PrepareBatch(_ context.Context, query string) (driver.Batch, error) {
	batch := &fakeBatch{query: query, appendFailureAt: -1}
	batchIndex := len(c.batches)
	if batchIndex < len(c.appendFailures) {
		batch.appendFailureAt = c.appendFailures[batchIndex]
	}
	if batchIndex < len(c.sendErrors) {
		batch.sendErr = c.sendErrors[batchIndex]
	}
	c.batches = append(c.batches, batch)
	return batch, nil
}

func (c *fakeBatchClient) takeLast(t *testing.T) *fakeBatch {
	t.Helper()
	if len(c.batches) == 0 {
		t.Fatalf("no prepared batches")
	}
	return c.batches[len(c.batches)-1]
}

type fakeBatch struct {
	query           string
	rows            [][]any
	sent            bool
	sendCalls       int
	closeCalls      int
	sendErr         error
	appendCalls     int
	appendFailureAt int
}

func (b *fakeBatch) Abort() error                  { return nil }
func (b *fakeBatch) Close() error                  { b.closeCalls++; return nil }
func (b *fakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *fakeBatch) Columns() []column.Interface   { return nil }
func (b *fakeBatch) Flush() error                  { return nil }
func (b *fakeBatch) IsSent() bool                  { return b.sent }
func (b *fakeBatch) Rows() int                     { return len(b.rows) }

func (b *fakeBatch) Append(v ...any) error {
	if b.appendCalls == b.appendFailureAt {
		b.appendCalls++
		return errors.New("bad row")
	}
	b.appendCalls++
	b.rows = append(b.rows, append([]any(nil), v...))
	return nil
}

func (b *fakeBatch) AppendStruct(any) error {
	panic("AppendStruct should not be used")
}

func (b *fakeBatch) Send() error {
	b.sendCalls++
	b.sent = true
	return b.sendErr
}

func totalSends(batches []*fakeBatch) int {
	total := 0
	for _, batch := range batches {
		total += batch.sendCalls
	}
	return total
}

func totalCloses(batches []*fakeBatch) int {
	total := 0
	for _, batch := range batches {
		total += batch.closeCalls
	}
	return total
}

func assertQueryContains(t *testing.T, query string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(query, part) {
			t.Fatalf("query %q does not contain %q", query, part)
		}
	}
}

func assertRowShape(t *testing.T, rows [][]any, rowCount int, columnCount int) {
	t.Helper()
	if len(rows) != rowCount {
		t.Fatalf("rows = %d, want %d", len(rows), rowCount)
	}
	for idx, row := range rows {
		if len(row) != columnCount {
			t.Fatalf("row %d columns = %d, want %d", idx, len(row), columnCount)
		}
	}
}
