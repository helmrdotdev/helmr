package clickhouse

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"testing"
	"time"
	"uuid"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

// Run on an isolated server with the current schema. Each subbenchmark is also
// runnable in a fresh test process so OS peak RSS includes retained source rows.
func BenchmarkWriterEnvelope(b *testing.B) {
	url := os.Getenv("HELMR_TEST_CLICKHOUSE_URL")
	if url == "" {
		b.Skip("HELMR_TEST_CLICKHOUSE_URL is not set")
	}
	for _, kind := range []string{"logs", "events"} {
		for _, size := range []int{512, 4096, map[string]int{"logs": telemetry.MaxRunLogContentBytes, "events": telemetry.MaxEventPayloadBytes}[kind]} {
			for _, random := range []bool{false, true} {
				for _, budget := range []struct{ rows, mib int }{{250, 8}, {10000, 8}, {10000, 16}, {10000, 32}, {10000, 64}} {
					b.Run(fmt.Sprintf("%s/bytes%d/random%t/rows%d/mib%d", kind, size, random, budget.rows, budget.mib), func(b *testing.B) {
						client, err := New(Config{URL: url})
						if err != nil {
							b.Fatal(err)
						}
						defer client.Close()
						writer := NewWriter(client)
						ctx := b.Context()
						if mode := os.Getenv("HELMR_TEST_CLICKHOUSE_INSERT_MODE"); mode == "sync" {
							ctx = ch.Context(ctx, ch.WithSettings(ch.Settings{"async_insert": 0}))
						}
						count := min(budget.rows, (budget.mib<<20)/size)
						org, run, lease := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
						now := time.Now().UTC()
						logs := make([]telemetry.RunLogRecord, 0, count)
						events := make([]telemetry.EventRecord, 0, count)
						rng := rand.NewChaCha8([32]byte{1})
						for i := 0; i < count; i++ {
							content := make([]byte, size)
							if random {
								_, _ = rng.Read(content)
							}
							if kind == "logs" {
								logs = append(logs, telemetry.RunLogRecord{OrgID: org, RunID: run, RunLeaseID: lease, AttemptNumber: 1, StreamName: "stdout", Seq: uint64(i + 1), ObservedSeq: uint64(i + 1), Content: content, SizeBytes: uint32(size), ObservedAt: now, AcceptedAt: now})
							} else {
								for j := range content {
									content[j] = 'a' + content[j]%26
								}
								content[0], content[len(content)-1] = '"', '"'
								events = append(events, telemetry.EventRecord{OrgID: org, SubjectKind: "run", SubjectID: run, RunID: &run, EventKind: "benchmark", Seq: uint64(i + 1), Body: string(content), ObservedAt: now, AcceptedAt: now})
							}
						}
						b.SetBytes(int64(count * size))
						b.ReportAllocs()
						b.ResetTimer()
						for range b.N {
							var rejected []telemetry.RejectedRow
							if kind == "logs" {
								rejected, err = writer.WriteRunLogs(ctx, logs)
							} else {
								rejected, err = writer.WriteEvents(ctx, events)
							}
							if err != nil || len(rejected) > 0 {
								b.Fatalf("write: %v %v", rejected, err)
							}
						}
						b.StopTimer()
						runtime.KeepAlive(logs)
						runtime.KeepAlive(events)
						b.ReportMetric(float64(count), "rows/op")
					})
				}
			}
		}
	}
}

func TestWriterInsertStrategyMeasurement(t *testing.T) {
	if os.Getenv("HELMR_TEST_CLICKHOUSE_INSERT_PROBE") != "1" {
		t.Skip("HELMR_TEST_CLICKHOUSE_INSERT_PROBE is not set")
	}
	client := disposableClient(t)
	if err := client.Exec(t.Context(), "SYSTEM STOP MERGES helmr_telemetry.run_logs"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Exec(context.Background(), "SYSTEM START MERGES helmr_telemetry.run_logs") })
	for _, mode := range []int{0, 1} {
		org, run, lease := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
		now := time.Now().UTC()
		rows := make([]telemetry.RunLogRecord, 250)
		for i := range rows {
			rows[i] = telemetry.RunLogRecord{OrgID: org, RunID: run, RunLeaseID: lease, StreamName: "stdout", Seq: uint64(i + 1), Content: make([]byte, 512), SizeBytes: 512, ObservedAt: now, AcceptedAt: now}
		}
		ctx := ch.Context(t.Context(), ch.WithSettings(ch.Settings{"async_insert": mode, "wait_for_async_insert": 1, "async_insert_use_adaptive_busy_timeout": 0, "async_insert_busy_timeout_ms": 100}))
		start := time.Now()
		for range 5 {
			if rejected, err := NewWriter(settingsBatchClient{client, ch.Settings{"async_insert": mode, "wait_for_async_insert": 1, "async_insert_use_adaptive_busy_timeout": 0, "async_insert_busy_timeout_ms": 100}}).WriteRunLogs(ctx, rows); err != nil || len(rejected) > 0 {
				t.Fatalf("write: %v %v", rejected, err)
			}
		}
		elapsed := time.Since(start)
		var got []struct {
			Rows  uint64 `ch:"rows"`
			Parts uint64 `ch:"parts"`
		}
		if err := client.Select(t.Context(), &got, "SELECT count() AS rows,uniqExact(_part) AS parts FROM helmr_telemetry.run_logs WHERE org_id = ?", org); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Rows != 1250 || got[0].Parts != 5 {
			t.Fatalf("unexpected durable part counts: %+v", got)
		}
		t.Logf("async_insert=%d wait=1 five_serial_sends=%s rows=%d parts=%d", mode, elapsed, got[0].Rows, got[0].Parts)
	}
}

// Override the production insert mode only for comparative measurements.
type settingsBatchClient struct {
	*Client
	settings ch.Settings
}

func (c settingsBatchClient) PrepareBatch(ctx context.Context, query string) (driver.Batch, error) {
	return c.Client.PrepareBatch(ch.Context(ctx, ch.WithSettings(c.settings)), query)
}
