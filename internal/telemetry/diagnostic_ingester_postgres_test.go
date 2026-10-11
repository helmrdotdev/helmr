package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

type diagnosticWriterFunc func(context.Context, string, []StoredDiagnostic) ([]RejectedRow, error)

func (f diagnosticWriterFunc) WriteDiagnostics(ctx context.Context, kind string, records []StoredDiagnostic) ([]RejectedRow, error) {
	return f(ctx, kind, records)
}

func TestDiagnosticIngesterDoesNotHoldAdmissionDuringSinkIO(t *testing.T) {
	pool := diagnosticDatabase(t)
	bounds := diagnosticTestBounds()
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
	if _, err := appendDiagnosticTest(t, pool, source, record, bounds); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	writer := diagnosticWriterFunc(func(ctx context.Context, kind string, records []StoredDiagnostic) ([]RejectedRow, error) {
		close(entered)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	ingester, err := NewDiagnosticIngester(pool, writer, diagnostic.IngestConfig{Admission: diagnosticTestBounds(), Batch: diagnostic.ExportBounds{Records: 4, Bytes: 6, ClaimFor: time.Minute}, PollEvery: time.Second, RetryAfter: time.Second, OperationTimeout: 5 * time.Second, GCBatchRecords: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ingester.ExportOnce(t.Context(), "session") }()
	<-entered
	// Distinct source admission completes before the sink is released. A real
	// transaction is used, so a hidden database lock is observable here.
	other := diagnosticTestSource(t, pool, "session", uuid.Nil())
	if _, err = appendDiagnosticTest(t, pool, other, record, bounds); err != nil {
		t.Fatalf("sink blocked admission: %v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var queued int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("wrong retirement %d %v", queued, err)
	}
}

func TestDiagnosticIngesterLostSinkReceiptAndPoisonIsolation(t *testing.T) {
	pool := diagnosticDatabase(t)
	bounds := diagnosticTestBounds()
	for range 2 {
		source := diagnosticTestSource(t, pool, "session", uuid.Nil())
		record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
		if _, err := appendDiagnosticTest(t, pool, source, record, bounds); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	writer := diagnosticWriterFunc(func(_ context.Context, _ string, records []StoredDiagnostic) ([]RejectedRow, error) {
		calls++
		if len(records) != 2 {
			t.Errorf("batch changed %d", len(records))
		}
		if calls == 1 {
			return nil, errors.New("sink receipt lost")
		}
		return []RejectedRow{{Index: 0, Err: errors.New("one invalid sink record")}}, nil
	})
	ingester, err := NewDiagnosticIngester(pool, writer, diagnostic.IngestConfig{Admission: diagnosticTestBounds(), Batch: diagnostic.ExportBounds{Records: 4, Bytes: 6, ClaimFor: time.Minute}, PollEvery: time.Second, RetryAfter: time.Microsecond, OperationTimeout: 5 * time.Second, GCBatchRecords: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingester.ExportOnce(t.Context(), "session"); err == nil {
		t.Fatal("lost reply treated as durable sink receipt")
	}
	var queued int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox`).Scan(&queued); err != nil || queued != 2 {
		t.Fatalf("uncertain rows lost %d %v", queued, err)
	}
	if err = ingester.ExportOnce(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("poison handling lost wrong rows %d %v", queued, err)
	}
}
