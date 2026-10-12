package clickhouse

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

func TestWriterBoundedBatchesAgainstDisposableClickHouse(t *testing.T) {
	client := disposableClient(t)
	writer := NewWriter(client)
	if err := client.Exec(t.Context(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HELMR_TEST_CLICKHOUSE_IDLE_ONLY") == "1" {
		return
	}
	environmentID := uuid.NewV7()
	now := time.Now().UTC()
	var diagnosticElapsed time.Duration

	{
		// Exercise both many bounded chunks and the largest configured transport chunk.
		for _, chunkBytes := range []int{64 << 10, 16 << 20} {
			const batchBytes = 16 << 20
			count := batchBytes / chunkBytes
			source := telemetry.DiagnosticSource{EnvironmentID: environmentID, Kind: "session", ID: uuid.New(), ProducerEpoch: 1}
			rows := diagnosticRows(source, count, chunkBytes, now.Truncate(time.Microsecond))
			started := time.Now()
			result, err := writer.WriteDiagnostics(t.Context(), "session", rows)
			if err != nil || len(result) != 0 {
				t.Fatalf("diagnostics chunk=%d: %v %v", chunkBytes, result, err)
			}
			diagnosticElapsed += time.Since(started)
			if err := client.Exec(t.Context(), `SELECT throwIf(count() != ? OR min(length(data)) != ? OR max(length(data)) != ?, 'unexpected stored diagnostic batch') FROM helmr_telemetry.session_logs WHERE environment_id = ? AND session_id = ?`, count, chunkBytes, chunkBytes, environmentID, source.ID); err != nil {
				t.Fatal(err)
			}
		}

	}
	t.Logf("bounded batches: diagnostics=%s", diagnosticElapsed)
}

func TestWriterAgainstDisposableClickHouse(t *testing.T) {
	client := disposableClient(t)
	writer := NewWriter(client)
	environmentID := uuid.NewV7()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := diagnosticRows(telemetry.DiagnosticSource{EnvironmentID: environmentID, Kind: "session", ID: uuid.New(), ProducerEpoch: 1}, 1, 5, now)
	result, err := writer.WriteDiagnostics(t.Context(), "session", rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("rejected = %+v, want none", result)
	}
	if err := client.Exec(t.Context(), `SELECT throwIf(count() != 1, 'expected exactly one stored chunk') FROM helmr_telemetry.session_logs WHERE environment_id = ?`, environmentID); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = writer.WriteDiagnostics(canceled, "session", rows)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error = %v, want context canceled", err)
	}
}
