package clickhouse

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/telemetry"
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

type partialHistoricalClient struct{}

func (partialHistoricalClient) Select(_ context.Context, dest any, _ string, _ ...any) error {
	switch rows := dest.(type) {
	case *[]eventRow:
		*rows = []eventRow{{Seq: 1}}
	case *[]runLogRow:
		*rows = []runLogRow{{Seq: 1}}
	}
	return errors.New("query limit exceeded after a partial block")
}

func TestHistoricalReadDiscardsPartialRowsOnFailure(t *testing.T) {
	reader := NewReader(partialHistoricalClient{})
	events, err := reader.ListEvents(t.Context(), telemetry.EventQuery{AfterSeq: 10, Limit: 200})
	if !errors.Is(err, telemetry.ErrHistoricalUnavailable) || len(events.Events) != 0 || events.LastSeq != 0 {
		t.Fatalf("partial event page escaped: %+v %v", events, err)
	}
	logs, err := reader.ListRunLogChunks(t.Context(), telemetry.RunLogChunkQuery{AfterSeq: 10, Limit: 200})
	if !errors.Is(err, telemetry.ErrHistoricalUnavailable) || len(logs.Chunks) != 0 || logs.LastSeq != 0 {
		t.Fatalf("partial log page escaped: %+v %v", logs, err)
	}
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
