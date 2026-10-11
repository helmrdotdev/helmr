package telemetry

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

func TestDiagnosticExportRetirementPreservesRetryAndCounters(t *testing.T) {
	pool := diagnosticDatabase(t)
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte{0, 255, 128}}
	bounds := diagnosticTestBounds()
	first, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimDiagnostics(t.Context(), pool, "session", diagnostic.ExportBounds{Records: 4, Bytes: 6, ClaimFor: time.Minute})
	if err != nil || len(claim.Records) != 1 || claim.Records[0].ByteOffset != 0 || claim.Records[0].ThroughByteOffset != 3 {
		t.Fatalf("claim %+v %v", claim, err)
	}
	// Simulate a lost sink reply: only the claim lease changes. The next exporter
	// must preserve exact bytes and receipt times while fencing the old token.
	dbtest.MustExec(t, t.Context(), pool, `UPDATE telemetry_outbox SET export_after=clock_timestamp()-interval '1 second'`)
	next, err := ClaimDiagnostics(t.Context(), pool, "session", diagnostic.ExportBounds{Records: 4, Bytes: 6, ClaimFor: time.Minute})
	if err != nil || len(next.Records) != 1 || next.Token == claim.Token {
		t.Fatalf("reclaim %+v %v", next, err)
	}
	if next.Records[0].AcceptedAt != claim.Records[0].AcceptedAt || next.Records[0].Record.ObservedAtUnixNano != 1 || string(next.Records[0].Record.Data) != string(record.Data) {
		t.Fatal("reclaimed immutable envelope changed")
	}
	old, err := RetireDiagnostics(t.Context(), pool, claim.Token, claim.IDs)
	if err != nil || old.Delivered != 0 || old.Expired != 0 {
		t.Fatalf("old claim retired %+v %v", old, err)
	}
	done, err := RetireDiagnostics(t.Context(), pool, next.Token, next.IDs)
	if err != nil || done.Delivered != 1 || done.Expired != 0 {
		t.Fatalf("retirement %+v %v", done, err)
	}
	again, err := RetireDiagnostics(t.Context(), pool, next.Token, next.IDs)
	if err != nil || again.Delivered != 0 {
		t.Fatalf("double retirement %+v %v", again, err)
	}
	retry, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil || retry.Expired || !retry.AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatalf("lost producer ACK after prune %+v %v", retry, err)
	}
	changed := record
	changed.Data = []byte("bad")
	if _, err = appendDiagnosticTest(t, pool, source, changed, bounds); !errors.Is(err, ErrDiagnosticConflict) {
		t.Fatalf("changed pruned retry %v", err)
	}
	record.Sequence, record.ThroughSequence = 2, 2
	if _, err = appendDiagnosticTest(t, pool, source, record, bounds); err != nil {
		t.Fatal(err)
	}
	after, err := ClaimDiagnostics(t.Context(), pool, "session", diagnostic.ExportBounds{Records: 4, Bytes: 6, ClaimFor: time.Minute})
	if err != nil || len(after.Records) != 1 || after.Records[0].ByteOffset != 3 || after.Records[0].ThroughByteOffset != 6 {
		t.Fatalf("predecessor %+v %v", after, err)
	}
	record.Sequence, record.ThroughSequence = 1, 1
	if _, err = appendDiagnosticTest(t, pool, source, record, bounds); !errors.Is(err, ErrDiagnosticStale) {
		t.Fatalf("superseded witness %v", err)
	}
	var bytes, records int64
	if err = pool.QueryRow(t.Context(), `SELECT COALESCE(sum(ingest_size_bytes),0)::bigint,count(*) FROM telemetry_outbox WHERE stream_kind='diagnostic'`).Scan(&bytes, &records); err != nil || bytes != 3 || records != 1 {
		t.Fatalf("occupancy=%d/%d %v", bytes, records, err)
	}
}

func TestDiagnosticExportBoundsAndFailedRowsRemainCounted(t *testing.T) {
	pool := diagnosticDatabase(t)
	bounds := diagnosticTestBounds()
	for range 3 {
		source := diagnosticTestSource(t, pool, "session", uuid.Nil())
		record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
		if _, err := appendDiagnosticTest(t, pool, source, record, bounds); err != nil {
			t.Fatal(err)
		}
	}
	claim, err := ClaimDiagnostics(t.Context(), pool, "session", diagnostic.ExportBounds{Records: 3, Bytes: 6, ClaimFor: time.Minute})
	if err != nil || len(claim.IDs) != 2 {
		t.Fatalf("byte bound %+v %v", claim, err)
	}
	if err = RetryDiagnostics(t.Context(), pool, claim.Token, claim.IDs[:1], time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = RetireDiagnostics(t.Context(), pool, claim.Token, claim.IDs[1:]); err != nil {
		t.Fatal(err)
	}
	next, err := ClaimDiagnostics(t.Context(), pool, "session", diagnostic.ExportBounds{Records: 1, Bytes: 6, ClaimFor: time.Minute})
	if err != nil || len(next.IDs) != 1 || next.IDs[0] == claim.IDs[0] {
		t.Fatalf("failed row blocked neighbor %+v %v", next, err)
	}
	var records int64
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox WHERE stream_kind='diagnostic'`).Scan(&records); err != nil || records != 2 {
		t.Fatalf("failed record evicted %d %v", records, err)
	}
}

func TestDiagnosticExportRejectsUnclaimableRecord(t *testing.T) {
	pool := diagnosticDatabase(t)
	for _, data := range []string{"abc", "x"} {
		source := diagnosticTestSource(t, pool, "session", uuid.Nil())
		record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte(data)}
		if _, err := appendDiagnosticTest(t, pool, source, record, diagnosticTestBounds()); err != nil {
			t.Fatal(err)
		}
	}
	batch := diagnostic.ExportBounds{Records: 2, Bytes: 2, ClaimFor: time.Minute}
	if _, err := ClaimDiagnostics(t.Context(), pool, "session", batch); !errors.Is(err, ErrDiagnosticExportSize) {
		t.Fatalf("unclaimable head: %v", err)
	}
	config := diagnostic.IngestConfig{Admission: diagnosticTestBounds(), Batch: batch, PollEvery: time.Second, RetryAfter: time.Second, OperationTimeout: time.Second, GCBatchRecords: 2}
	if err := config.Validate(); !errors.Is(err, ErrDiagnosticInvalid) {
		t.Fatalf("inconsistent configuration: %v", err)
	}
	batch.Bytes = 4
	claim, err := ClaimDiagnostics(t.Context(), pool, "session", batch)
	if err != nil || len(claim.IDs) != 2 {
		t.Fatalf("corrected bound did not release both neighbors: %+v %v", claim, err)
	}
}
