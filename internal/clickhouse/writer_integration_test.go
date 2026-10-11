package clickhouse

import (
	"context"
	"errors"
	"os"
	"strings"
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
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	deploymentID := uuid.NewV7()
	now := time.Now().UTC()
	batchKind := os.Getenv("HELMR_TEST_CLICKHOUSE_BATCH_KIND")
	var eventElapsed, diagnosticElapsed time.Duration

	if batchKind != "diagnostics" {
		eventBody := `{"data":"` + strings.Repeat("x", telemetry.MaxEventPayloadBytes-len(`{"data":"`)-len(`"}`)) + `"}`
		eventCount := telemetry.MaxTelemetryBatchBytes / (telemetry.MaxEventPayloadBytes + telemetry.MaxEventMessageBytes)
		events := make([]telemetry.EventRecord, eventCount)
		for idx := range events {
			events[idx] = telemetry.EventRecord{
				OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID,
				SubjectKind: "deployment", SubjectID: deploymentID, EventKind: "test.maximum", Seq: uint64(idx + 1),
				DeploymentID: &deploymentID, Message: strings.Repeat("m", telemetry.MaxEventMessageBytes), Body: strings.Clone(eventBody),
				RetentionClass: "standard", RedactionClass: "internal", ObservedAt: now, AcceptedAt: now,
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

		// Exercise both many bounded chunks and the largest configured transport chunk.
		// Diagnostic export batch limits are configured independently of event limits.
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
	t.Logf("bounded batches: events=%s diagnostics=%s", eventElapsed, diagnosticElapsed)
}

func TestWriterAgainstDisposableClickHouse(t *testing.T) {
	client := disposableClient(t)
	writer := NewWriter(client)
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	deploymentID := uuid.NewV7()
	now := time.Now().UTC()
	rows := []telemetry.EventRecord{{
		OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, SubjectKind: "deployment", SubjectID: deploymentID,
		EventKind: "test.valid", Seq: 1, DeploymentID: &deploymentID, Message: "valid", Body: `{}`,
		RetentionClass: "standard", RedactionClass: "internal", ObservedAt: now, AcceptedAt: now,
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
