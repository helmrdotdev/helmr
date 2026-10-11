package guestd

import (
	"bytes"
	"math"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticBufferKeepsBytesAndFreezesGapRetries(t *testing.T) {
	b, err := newDiagnosticBuffer(diagnosticLimits{ChunkBytes: 4, BufferBytes: 4, BufferRecords: 2})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := []byte{0, 255, 128, 1}
	if kept, err := b.append(input, now); err != nil || !kept {
		t.Fatalf("first %v %v", kept, err)
	}
	input[0] = 9
	for range 2 {
		if kept, err := b.append([]byte("drop"), now); err != nil || kept {
			t.Fatalf("drop %v %v", kept, err)
		}
	}
	first, ok := b.peek()
	if !ok || !bytes.Equal(first.Data, []byte{0, 255, 128, 1}) {
		t.Fatalf("original bytes %v", first)
	}
	if err := b.acknowledge(3); err == nil {
		t.Fatal("ack skipped first bytes")
	}
	if err := b.acknowledge(1); err != nil {
		t.Fatal(err)
	}
	gap, ok := b.peek()
	// The first gap was frozen when space allowed; subsequent drops remain a
	// separate immutable range rather than extending an exposed record.
	if !ok || gap.Kind != diagnosticGap || gap.Sequence != 2 || gap.Through != 2 || gap.DroppedBytes != 4 {
		t.Fatalf("gap %+v", gap)
	}
	if _, err := b.append([]byte("more"), now); err != nil {
		t.Fatal(err)
	}
	retry, _ := b.peek()
	if retry.Kind != gap.Kind || retry.Sequence != gap.Sequence || retry.Through != gap.Through || retry.DroppedBytes != gap.DroppedBytes {
		t.Fatalf("retry mutated %+v %+v", gap, retry)
	}
	b.close(true, now)
	if kept, err := b.append([]byte("late"), now); err != nil || kept {
		t.Fatalf("closed write %v %v", kept, err)
	}
	var last int64 = 1
	var dropped int64
	var ended bool
	for {
		record, ok := b.peek()
		if !ok {
			break
		}
		if record.Sequence != last+1 {
			t.Fatalf("unaccounted boundary %+v after %d", record, last)
		}
		if record.Kind == diagnosticGap {
			dropped += record.DroppedBytes
		}
		if record.Kind == diagnosticEnd {
			ended = record.Complete
		}
		if err := b.acknowledge(record.Through); err != nil {
			t.Fatal(err)
		}
		if err := b.acknowledge(record.Through); err != nil {
			t.Fatal("lost ACK was not idempotent", err)
		}
		last = record.Through
	}
	if dropped != 12 || !ended || last != 5 {
		t.Fatalf("coverage dropped=%d end=%v last=%d", dropped, ended, last)
	}
}

func TestDiagnosticBufferSaturationRemainsBounded(t *testing.T) {
	b, err := newDiagnosticBuffer(diagnosticLimits{ChunkBytes: 3, BufferBytes: 6, BufferRecords: 2})
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for range 4 {
		writers.Go(func() {
			for range 1000 {
				if _, err := b.append([]byte("abc"), time.Now()); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	writers.Wait()
	b.close(false, time.Now())
	if b.count != 2 || b.bytes != 6 || b.pendingGap == nil || b.pendingGap.DroppedBytes != 11994 {
		t.Fatalf("unbounded or wrong accounting count=%d bytes=%d gap=%+v", b.count, b.bytes, b.pendingGap)
	}
	var total int64
	var last int64
	for {
		record, ok := b.peek()
		if !ok {
			break
		}
		if record.Sequence != last+1 {
			t.Fatalf("sequence %+v after %d", record, last)
		}
		total += int64(len(record.Data)) + record.DroppedBytes
		last = record.Through
		if record.Kind == diagnosticEnd && record.Complete {
			t.Fatal("read failure became complete EOF")
		}
		if err := b.acknowledge(record.Through); err != nil {
			t.Fatal(err)
		}
	}
	if total != 12000 || last != 4001 {
		t.Fatalf("coverage bytes=%d sequence=%d", total, last)
	}
}

func TestDiagnosticBufferSequenceExhaustionMarksUnknownTail(t *testing.T) {
	b, _ := newDiagnosticBuffer(diagnosticLimits{ChunkBytes: 1, BufferBytes: 1, BufferRecords: 1})
	b.next = math.MaxInt64
	for range 2 {
		if kept, err := b.append([]byte("x"), time.Now()); err != nil || kept {
			t.Fatalf("exhaustion %v %v", kept, err)
		}
	}
	record, ok := b.peek()
	if !ok || record.Kind != diagnosticEnd || record.Complete || record.Sequence != math.MaxInt64 {
		t.Fatalf("exhaustion %+v", record)
	}
}
