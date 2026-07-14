package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

func TestClickHouseWriterAppendsTypedBatchRows(t *testing.T) {
	deploymentID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runLeaseID := uuid.Must(uuid.NewV7())
	attemptNumber := int32(2)
	observedAt := time.Date(2026, 7, 3, 1, 2, 3, 456000000, time.UTC)
	client := &fakeBatchClient{}
	writer := NewClickHouseWriter(client)

	if err := writer.WriteEvents(context.Background(), []EventRecord{{
		OrgID:          uuid.Must(uuid.NewV7()),
		ProjectID:      uuid.Must(uuid.NewV7()),
		EnvironmentID:  uuid.Must(uuid.NewV7()),
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

	if err := writer.WriteRunLogs(context.Background(), []RunLogRecord{{
		OrgID:          uuid.Must(uuid.NewV7()),
		ProjectID:      uuid.Must(uuid.NewV7()),
		EnvironmentID:  uuid.Must(uuid.NewV7()),
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
	forbiddenAttemptColumn := "attempt" + "_id"
	if strings.Contains(runLogBatch.query, forbiddenAttemptColumn) {
		t.Fatalf("run log query contains removed attempt column: %s", runLogBatch.query)
	}
	assertRowShape(t, runLogBatch.rows, 1, 16)
	if got := runLogBatch.rows[0][4]; got != runLeaseID {
		t.Fatalf("run log run_lease_id = %v, want %s", got, runLeaseID)
	}

	if err := writer.WriteMeterEvents(context.Background(), []MeterEventRecord{{
		OrgID:          uuid.Must(uuid.NewV7()),
		ProjectID:      uuid.Must(uuid.NewV7()),
		EnvironmentID:  uuid.Must(uuid.NewV7()),
		SourceType:     "run_lease",
		SourceID:       runLeaseID,
		RunID:          &runID,
		AttemptNumber:  attemptNumber,
		TraceID:        "trace",
		SpanID:         "span",
		Meter:          "active_time",
		Quantity:       "123",
		Unit:           "milliseconds",
		MeasuredTo:     &observedAt,
		Details:        `{"phase":"final"}`,
		IdempotencyKey: "meter-key",
		Fingerprint:    "meter-fingerprint",
		OccurredAt:     observedAt,
		CreatedAt:      observedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	meterBatch := client.takeLast(t)
	assertQueryContains(t, meterBatch.query, "INSERT INTO helmr_telemetry.meter_events", "quantity", "occurred_at")
	assertRowShape(t, meterBatch.rows, 1, 20)
	if got := meterBatch.rows[0][10]; got != "active_time" {
		t.Fatalf("meter event meter = %v, want active_time", got)
	}
	if got := meterBatch.rows[0][11]; got != "123" {
		t.Fatalf("meter event quantity = %v, want 123", got)
	}

	if err := writer.WriteTerminalOutput(context.Background(), []TerminalOutputRecord{{
		OrgID:          uuid.Must(uuid.NewV7()),
		ProjectID:      uuid.Must(uuid.NewV7()),
		EnvironmentID:  uuid.Must(uuid.NewV7()),
		WorkspaceID:    uuid.Must(uuid.NewV7()),
		ResourceKind:   "workspace_process",
		ResourceID:     uuid.Must(uuid.NewV7()),
		StreamName:     "output",
		OffsetStart:    10,
		OffsetEnd:      15,
		Content:        "aGVsbG8=",
		SizeBytes:      5,
		IdempotencyKey: "terminal-key",
		RetentionClass: "standard",
		RedactionClass: "standard",
		ObservedAt:     observedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	terminalBatch := client.takeLast(t)
	assertQueryContains(t, terminalBatch.query, "INSERT INTO helmr_telemetry.terminal_outputs", "offset_start", "observed_at")
	assertRowShape(t, terminalBatch.rows, 1, 15)
	if got := terminalBatch.rows[0][7]; got != uint64(10) {
		t.Fatalf("terminal offset_start = %v, want 10", got)
	}
}

type fakeBatchClient struct {
	batches []*fakeBatch
}

func (c *fakeBatchClient) PrepareBatch(_ context.Context, query string) (driver.Batch, error) {
	batch := &fakeBatch{query: query}
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
	query string
	rows  [][]any
	sent  bool
}

func (b *fakeBatch) Abort() error                  { return nil }
func (b *fakeBatch) Close() error                  { return nil }
func (b *fakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *fakeBatch) Columns() []column.Interface   { return nil }
func (b *fakeBatch) Flush() error                  { return nil }
func (b *fakeBatch) IsSent() bool                  { return b.sent }
func (b *fakeBatch) Rows() int                     { return len(b.rows) }

func (b *fakeBatch) Append(v ...any) error {
	b.rows = append(b.rows, append([]any(nil), v...))
	return nil
}

func (b *fakeBatch) AppendStruct(any) error {
	panic("AppendStruct should not be used")
}

func (b *fakeBatch) Send() error {
	b.sent = true
	return nil
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
