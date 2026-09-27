package clickhouse

import (
	"bytes"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"testing"
	"time"
	"uuid"
)

func TestCommandLogsRoundTripDeduplicatesAndScopesObservedCursor(t *testing.T) {
	c := disposableClient(t)
	writer, reader := NewWriter(c), NewReader(c)
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := telemetry.CommandLogRecord{OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(), EnvironmentID: uuid.NewV7(), CommandID: uuid.NewV7(), StreamName: "stdout", ObservedAt: now, AcceptedAt: now, RetentionClass: "standard", RedactionClass: "standard", Source: "worker"}
	rows := make([]telemetry.CommandLogRecord, 4)
	for i := range rows {
		rows[i] = base
		rows[i].ObservedSeq = uint64(i)
		rows[i].Seq = uint64(i + 100)
		rows[i].Content = []byte{0, 255, 128, byte(i)}
		rows[i].SizeBytes = 4
	}
	stderr := rows[0]
	stderr.StreamName = "stderr"
	other := rows[0]
	other.CommandID = uuid.NewV7()
	expired := rows[0]
	expired.ObservedSeq = 99
	expired.AcceptedAt = now.Add(-91 * 24 * time.Hour)
	all := append(append([]telemetry.CommandLogRecord{}, rows...), stderr, other, expired)
	for range 2 {
		if rejected, err := writer.WriteCommandLogs(t.Context(), all); err != nil || len(rejected) != 0 {
			t.Fatalf("write=%v, %v", rejected, err)
		}
	}
	q := telemetry.CommandLogChunkQuery{OrgID: base.OrgID, EnvironmentID: base.EnvironmentID, CommandID: base.CommandID, Stream: "stdout", Limit: 2}
	for start := 0; start < 4; start += 2 {
		page, err := reader.ListCommandLogChunks(t.Context(), q)
		if err != nil || len(page.Chunks) != 2 {
			t.Fatalf("page=%+v, %v", page, err)
		}
		for i, chunk := range page.Chunks {
			want := rows[start+i]
			if chunk.ObservedSeq != want.ObservedSeq || !bytes.Equal(chunk.Content, want.Content) || !chunk.AcceptedAt.Equal(now) {
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
