package guestd

import (
	"errors"
	"math"
	"sync"
	"time"
)

type diagnosticKind uint8

const (
	diagnosticData diagnosticKind = iota + 1
	diagnosticGap
	diagnosticEnd
)

// A diagnostic sequence belongs to one retained producer and stdout/stderr stream.
// Gap ranges consume the observed sequences they replace. End has its own final
// sequence; Complete is false when the reader could not establish an exact EOF.
type diagnosticRecord struct {
	Kind         diagnosticKind
	Sequence     int64
	Through      int64
	ObservedAt   time.Time
	Data         []byte
	DroppedBytes int64
	Complete     bool
}

type diagnosticLimits struct {
	ChunkBytes    int
	BufferBytes   int64
	BufferRecords int
}

// diagnosticBuffer never waits for ingestion. It keeps admitted local bytes and
// coalesces newly dropped output into one pending gap, with one reserved final
// record. Published records are immutable across uncertain acknowledgments.
// The owner supplies qualified limits; this mechanism chooses no product defaults.
type diagnosticBuffer struct {
	mu                 sync.Mutex
	limits             diagnosticLimits
	records            []diagnosticRecord
	head, count        int
	bytes              int64
	next, acknowledged int64
	pendingGap         *diagnosticRecord
	end                *diagnosticRecord
	closed             bool
}

func newDiagnosticBuffer(limits diagnosticLimits) (*diagnosticBuffer, error) {
	if limits.ChunkBytes <= 0 || limits.BufferBytes < int64(limits.ChunkBytes) || limits.BufferRecords <= 0 {
		return nil, errors.New("diagnostic buffer requires positive, consistent bounds")
	}
	return &diagnosticBuffer{limits: limits, records: make([]diagnosticRecord, limits.BufferRecords), next: 1}, nil
}

// append copies at most ChunkBytes. A false result means bytes were deliberately
// dropped or the source already closed; it never means durable acceptance.
func (b *diagnosticBuffer) append(data []byte, observed time.Time) (bool, error) {
	if len(data) == 0 || len(data) > b.limits.ChunkBytes || observed.IsZero() {
		return false, errors.New("invalid diagnostic chunk")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false, nil
	}
	// Reserve the last representable sequence for an explicit unknown boundary.
	// Keep draining the caller's pipe even if diagnostic accounting is exhausted.
	if b.next == math.MaxInt64 {
		b.closeLocked(false, observed)
		return false, nil
	}
	sequence := b.next
	b.next++
	b.promoteGapLocked()
	if b.pendingGap == nil && b.count < len(b.records) && int64(len(data)) <= b.limits.BufferBytes-b.bytes {
		b.pushLocked(diagnosticRecord{Kind: diagnosticData, Sequence: sequence, Through: sequence, ObservedAt: observed, Data: append([]byte(nil), data...)})
		return true, nil
	}
	if b.pendingGap == nil {
		b.pendingGap = &diagnosticRecord{Kind: diagnosticGap, Sequence: sequence, Through: sequence, ObservedAt: observed, DroppedBytes: int64(len(data))}
	} else if int64(len(data)) <= math.MaxInt64-b.pendingGap.DroppedBytes {
		b.pendingGap.Through = sequence
		b.pendingGap.DroppedBytes += int64(len(data))
	} else {
		// Counts beyond the representation become an unknown tail, never a wrapped
		// exact count. The unknown boundary occupies this unaccounted sequence.
		b.next = sequence
		b.closeLocked(false, observed)
	}
	return false, nil
}

func (b *diagnosticBuffer) close(complete bool, observed time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked(complete, observed)
}
func (b *diagnosticBuffer) closeLocked(complete bool, observed time.Time) {
	if b.closed {
		return
	}
	b.closed = true
	b.end = &diagnosticRecord{Kind: diagnosticEnd, Sequence: b.next, Through: b.next, ObservedAt: observed, Complete: complete}
}
func (b *diagnosticBuffer) pushLocked(record diagnosticRecord) {
	b.records[(b.head+b.count)%len(b.records)] = record
	b.count++
	b.bytes += int64(len(record.Data))
}
func (b *diagnosticBuffer) promoteGapLocked() {
	if b.pendingGap != nil && b.count < len(b.records) {
		b.pushLocked(*b.pendingGap)
		b.pendingGap = nil
	}
}

// peek freezes a pending gap before exposing it, so later writes cannot change
// bytes or metadata that may already have committed upstream. Its copy is owned
// by the sender and may be retained across transport retries.
func (b *diagnosticBuffer) peek() (diagnosticRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.promoteGapLocked()
	if b.count > 0 {
		record := b.records[b.head]
		record.Data = append([]byte(nil), record.Data...)
		return record, true
	}
	if b.end != nil {
		return *b.end, true
	}
	return diagnosticRecord{}, false
}

// acknowledge releases only the exact exposed head. The transport owner must
// first validate current sender/attachment and either durable acceptance or an
// explicit disposition proving this entire retained range already expired.
func (b *diagnosticBuffer) acknowledge(through int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if through <= 0 {
		return errors.New("invalid diagnostic acknowledgment")
	}
	if through <= b.acknowledged {
		return nil
	}
	if b.count > 0 {
		record := &b.records[b.head]
		if through != record.Through {
			return errors.New("diagnostic acknowledgment skips retained output")
		}
		b.bytes -= int64(len(record.Data))
		clear(record.Data)
		*record = diagnosticRecord{}
		b.head = (b.head + 1) % len(b.records)
		b.count--
	} else if b.pendingGap == nil && b.end != nil && through == b.end.Through {
		b.end = nil
	} else {
		return errors.New("diagnostic acknowledgment has no retained record")
	}
	b.acknowledged = through
	b.promoteGapLocked()
	return nil
}
