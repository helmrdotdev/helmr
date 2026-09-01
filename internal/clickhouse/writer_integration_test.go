package clickhouse

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

func TestWriterMaximumBoundedBatchesAgainstDisposableClickHouse(t *testing.T) {
	url := os.Getenv("HELMR_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("HELMR_TEST_CLICKHOUSE_URL is not set")
	}
	client, err := New(Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	writer := NewWriter(client)
	if err := client.Exec(t.Context(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HELMR_TEST_CLICKHOUSE_IDLE_ONLY") == "1" {
		return
	}
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	runID := uuid.NewV7()
	now := time.Now().UTC()
	batchKind := os.Getenv("HELMR_TEST_CLICKHOUSE_BATCH_KIND")
	var eventElapsed, runLogElapsed time.Duration

	if batchKind != "run_logs" {
		eventBody := `{"data":"` + strings.Repeat("x", telemetry.MaxEventPayloadBytes-len(`{"data":"`)-len(`"}`)) + `"}`
		eventCount := telemetry.MaxTelemetryBatchBytes / (telemetry.MaxEventPayloadBytes + telemetry.MaxEventMessageBytes)
		events := make([]telemetry.EventRecord, eventCount)
		for idx := range events {
			events[idx] = telemetry.EventRecord{
				OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID,
				SubjectKind: "run", SubjectID: runID, EventKind: "test.maximum", Seq: uint64(idx + 1),
				RunID: &runID, Message: strings.Repeat("m", telemetry.MaxEventMessageBytes), Body: strings.Clone(eventBody),
				RetentionClass: "standard", RedactionClass: "internal", ObservedAt: now,
			}
		}
		started := time.Now()
		result, err := writer.WriteEvents(t.Context(), events)
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 0 {
			t.Fatalf("maximum event rejects = %+v", result)
		}
		eventElapsed = time.Since(started)
		if err := client.Exec(t.Context(), `SELECT throwIf(count() != ?, 'unexpected stored event count') FROM helmr_telemetry.events WHERE org_id = ?`, eventCount, orgID); err != nil {
			t.Fatal(err)
		}
	}

	if batchKind != "events" {
		runLogCount := telemetry.MaxTelemetryBatchBytes / telemetry.MaxRunLogContentBytes
		decodedLogs := make([][]byte, runLogCount)
		runLogs := make([]telemetry.RunLogRecord, runLogCount)
		for idx := range runLogs {
			decodedLogs[idx] = make([]byte, telemetry.MaxRunLogContentBytes)
			runLogs[idx] = telemetry.RunLogRecord{
				OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, RunID: runID,
				AttemptNumber: 1, StreamName: "stdout", Seq: uint64(idx + 1), ObservedSeq: uint64(idx + 1),
				Content: base64.StdEncoding.EncodeToString(decodedLogs[idx]), SizeBytes: uint64(len(decodedLogs[idx])), RetentionClass: "standard",
				RedactionClass: "internal", Source: "worker", ObservedAt: now,
			}
		}
		started := time.Now()
		result, err := writer.WriteRunLogs(t.Context(), runLogs)
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 0 {
			t.Fatalf("maximum run-log rejects = %+v", result)
		}
		runtime.KeepAlive(decodedLogs)
		runLogElapsed = time.Since(started)
		if err := client.Exec(t.Context(), `SELECT throwIf(count() != ?, 'unexpected stored run-log count') FROM helmr_telemetry.run_logs WHERE org_id = ?`, runLogCount, orgID); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("maximum batches: events=%s run_logs=%s", eventElapsed, runLogElapsed)
	if eventElapsed > time.Second || runLogElapsed > time.Second {
		t.Fatalf("maximum batch application time exceeds 1s: events=%s run_logs=%s", eventElapsed, runLogElapsed)
	}
}

func TestWriterAgainstDisposableClickHouse(t *testing.T) {
	url := os.Getenv("HELMR_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("HELMR_TEST_CLICKHOUSE_URL is not set")
	}
	client, err := New(Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	writer := NewWriter(client)
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	runID := uuid.NewV7()
	now := time.Now().UTC()
	rows := []telemetry.EventRecord{{
		OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, SubjectKind: "run", SubjectID: runID,
		EventKind: "test.valid", Seq: 1, RunID: &runID, Message: "valid", Body: `{}`,
		RetentionClass: "standard", RedactionClass: "internal", ObservedAt: now,
	}}
	result, err := writer.WriteEvents(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("rejected = %+v, want none", result)
	}
	if err := client.Exec(t.Context(), `SELECT throwIf(count() != 1, 'expected exactly one stored event') FROM helmr_telemetry.events WHERE org_id = ?`, orgID); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = writer.WriteEvents(canceled, rows[:1])
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error = %v, want context canceled", err)
	}
}
