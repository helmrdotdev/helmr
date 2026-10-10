package clickhouse

import (
	"bytes"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"testing"
	"time"
	"uuid"
)

func TestCommandLogsRoundTripDeduplicatesAndScopesObservedCursor(t *testing.T) {
	c := disposableClient(t)
	writer, reader := NewWriter(c), NewReader(c)
	now := time.Now().UTC().Truncate(time.Millisecond)
	source := telemetry.DiagnosticSource{EnvironmentID: uuid.NewV7(), ID: uuid.NewV7(), Kind: "computer_command", ProducerEpoch: 1}
	rows := make([]telemetry.StoredDiagnostic, 4)
	for i := range rows {
		rows[i] = telemetry.StoredDiagnostic{Source: source, Record: diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: int64(i + 1), ThroughSequence: int64(i + 1), ObservedAtUnixNano: now.UnixNano(), Data: []byte{0, 255, 128, byte(i)}}, ByteOffset: int64(i * 4), ThroughByteOffset: int64((i + 1) * 4), AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}
	}
	gap := rows[0]
	gap.Record = diagnostic.Record{Stream: "stdout", Kind: "gap", Sequence: 5, ThroughSequence: 8, ObservedAtUnixNano: now.UnixNano(), DroppedBytes: 16}
	gap.ByteOffset, gap.ThroughByteOffset = 16, 32
	end := gap
	end.Record = diagnostic.Record{Stream: "stdout", Kind: "end", Sequence: 9, ThroughSequence: 9, ObservedAtUnixNano: now.UnixNano(), Complete: true}
	end.ByteOffset, end.ThroughByteOffset = 32, 32
	rows = append(rows, gap, end)
	stderr := rows[0]
	stderr.Record.Stream = "stderr"
	other := rows[0]
	other.Source.ID = uuid.NewV7()
	expired := rows[0]
	expired.Record.Sequence = 99
	expired.Record.ThroughSequence = 99
	expired.AcceptedAt = now.Add(-91 * 24 * time.Hour)
	expired.ExpiresAt = expired.AcceptedAt.Add(90 * 24 * time.Hour)
	all := append(append([]telemetry.StoredDiagnostic{}, rows...), stderr, other, expired)
	for range 2 {
		if rejected, err := writer.WriteDiagnostics(t.Context(), "computer_command", all); err != nil || len(rejected) != 0 {
			t.Fatalf("write=%v %v", rejected, err)
		}
	}
	q := telemetry.CommandLogChunkQuery{OrgID: uuid.NewV7(), EnvironmentID: source.EnvironmentID, CommandID: source.ID, Stream: "stdout", Limit: 2}
	for start := 0; start < len(rows); start += 2 {
		page, err := reader.ListCommandLogChunks(t.Context(), q)
		if err != nil || len(page.Chunks) != 2 {
			t.Fatalf("page=%+v, %v", page, err)
		}
		for i, chunk := range page.Chunks {
			want := rows[start+i]
			if chunk.ObservedSeq != uint64(want.Record.Sequence) || chunk.ThroughSequence != uint64(want.Record.ThroughSequence) || chunk.Kind != want.Record.Kind || chunk.DroppedBytes != want.Record.DroppedBytes || chunk.Complete != want.Record.Complete || !bytes.Equal(chunk.Content, want.Record.Data) || !chunk.AcceptedAt.Equal(now) {
				t.Fatalf("chunk=%+v want=%+v", chunk, want)
			}
		}
		last := page.Chunks[1].ObservedSeq
		q.AfterObservedSeq = &last
	}
	if page, err := reader.ListCommandLogChunks(t.Context(), q); err != nil || len(page.Chunks) != 0 {
		t.Fatalf("end=%+v, %v", page, err)
	}
	q.AfterObservedSeq = nil
	q.Stream = "stderr"
	if page, err := reader.ListCommandLogChunks(t.Context(), q); err != nil || len(page.Chunks) != 1 {
		t.Fatalf("stderr=%+v, %v", page, err)
	}
	q.EnvironmentID = uuid.NewV7()
	if page, err := reader.ListCommandLogChunks(t.Context(), q); err != nil || len(page.Chunks) != 0 {
		t.Fatalf("scope=%+v, %v", page, err)
	}
}
