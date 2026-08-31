package clickhouse

import (
	"reflect"
	"testing"
	"uuid"
)

func TestHistoricalRowsDeclareClickHouseTagsForSelectedColumns(t *testing.T) {
	assertClickHouseTags(t, eventRow{}, []string{
		"seq", "run_id", "deployment_id", "run_lease_id", "attempt_number",
		"trace_id", "span_id", "traceparent", "category", "severity", "source",
		"event_kind", "message", "body", "redaction_class", "observed_at",
	})
	assertClickHouseTags(t, runLogRow{}, []string{
		"run_id", "run_lease_id", "attempt_number", "stream_name",
		"seq", "observed_seq", "content", "size_bytes", "observed_at",
	})
}

func TestHistoricalRowsMapUUIDStrings(t *testing.T) {
	runID := uuid.NewV7().String()
	deploymentID := uuid.NewV7().String()
	runLeaseID := uuid.NewV7().String()
	event := (eventRow{RunID: &runID, DeploymentID: &deploymentID, RunLeaseID: &runLeaseID}).event()
	if event.RunID == nil || *event.RunID != runID || event.DeploymentID == nil || *event.DeploymentID != deploymentID {
		t.Fatalf("event UUIDs = run %v, deployment %v", event.RunID, event.DeploymentID)
	}
	if got := (runLogRow{RunID: runID, RunLeaseID: runLeaseID}).chunk().RunID; got != runID {
		t.Fatalf("run log UUID = %q, want %q", got, runID)
	}
}

func assertClickHouseTags(t *testing.T, row any, columns []string) {
	t.Helper()
	tags := make(map[string]struct{})
	rowType := reflect.TypeOf(row)
	for field := range rowType.Fields() {
		tag := field.Tag.Get("ch")
		if tag == "" || tag == "-" {
			continue
		}
		tags[tag] = struct{}{}
	}
	for _, column := range columns {
		if _, ok := tags[column]; !ok {
			t.Fatalf("%T missing ch tag for selected column %q", row, column)
		}
	}
}
