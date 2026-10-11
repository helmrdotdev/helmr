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
		"seq", "deployment_id",
		"trace_id", "span_id", "traceparent", "category", "severity", "source",
		"event_kind", "message", "body", "redaction_class", "observed_at",
	})
	assertClickHouseTags(t, commandLogRow{}, []string{"sequence", "through_sequence", "kind", "data", "observed_at_unix_nano", "dropped_bytes", "complete", "accepted_at", "expires_at"})
}

type partialHistoricalClient struct{}

func (partialHistoricalClient) Select(_ context.Context, dest any, _ string, _ ...any) error {
	switch rows := dest.(type) {
	case *[]eventRow:
		*rows = []eventRow{{Seq: 1}}
	case *[]commandLogRow:
		*rows = []commandLogRow{{Sequence: 1}}
	}
	return errors.New("query limit exceeded after a partial block")
}

func TestHistoricalReadDiscardsPartialRowsOnFailure(t *testing.T) {
	reader := NewReader(partialHistoricalClient{})
	events, err := reader.ListEvents(t.Context(), telemetry.EventQuery{AfterSeq: 10, Limit: 200})
	if !errors.Is(err, telemetry.ErrHistoricalUnavailable) || len(events.Events) != 0 || events.LastSeq != 0 {
		t.Fatalf("partial event page escaped: %+v %v", events, err)
	}
	logs, err := reader.ListCommandLogChunks(t.Context(), telemetry.CommandLogChunkQuery{OrgID: uuid.New(), EnvironmentID: uuid.New(), CommandID: uuid.New(), Stream: "stdout", Limit: 2})
	if !errors.Is(err, telemetry.ErrHistoricalUnavailable) || len(logs.Chunks) != 0 {
		t.Fatalf("partial log page escaped: %+v %v", logs, err)
	}
}

func TestHistoricalRowsMapUUIDStrings(t *testing.T) {
	deploymentID := uuid.NewV7().String()
	event := (eventRow{DeploymentID: &deploymentID}).event()
	if event.DeploymentID == nil || *event.DeploymentID != deploymentID {
		t.Fatalf("event Deployment ID = %v", event.DeploymentID)
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
