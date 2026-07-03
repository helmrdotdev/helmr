package telemetry

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

func TestHistoricalReaderListsTerminalOutputFromClickHouse(t *testing.T) {
	resourceID := uuid.Must(uuid.NewV7())
	observedAt := time.Date(2026, 7, 2, 1, 2, 3, 123000000, time.UTC)
	ingestedAt := time.Date(2026, 7, 2, 1, 2, 4, 456000000, time.UTC)
	client := &fakeHistoricalClient{
		selectFunc: func(_ context.Context, dest any, query string, args ...any) error {
			params := namedArgs(args)
			if !strings.Contains(query, "helmr_telemetry.terminal_outputs FINAL") {
				t.Fatalf("query = %q, want terminal_outputs FINAL", query)
			}
			if !strings.Contains(query, "offset_end > @after") || !strings.Contains(query, "offset_end <= @watermark") {
				t.Fatalf("query = %q, want bounded historical offsets", query)
			}
			if params["after"] != uint64(5) || params["watermark"] != uint64(10) {
				t.Fatalf("params after=%v watermark=%v, want 5/10", params["after"], params["watermark"])
			}
			if params["resource_id"] != resourceID {
				t.Fatalf("resource_id = %v, want %s", params["resource_id"], resourceID)
			}
			rows, ok := dest.(*[]terminalOutputHistoryRow)
			if !ok {
				t.Fatalf("dest type = %T, want *[]terminalOutputHistoryRow", dest)
			}
			*rows = append(*rows, terminalOutputHistoryRow{
				StreamName:  "output",
				OffsetStart: 5,
				OffsetEnd:   10,
				Content:     base64.StdEncoding.EncodeToString([]byte("hello")),
				ObservedAt:  observedAt,
				IngestedAt:  ingestedAt,
			})
			return nil
		},
	}
	reader := NewHistoricalReader(client)
	rows, last, err := reader.ListTerminalOutput(context.Background(), TerminalOutputQuery{
		OrgID:        uuid.Must(uuid.NewV7()),
		CellID:       "us-east-1-cell-1",
		WorkspaceID:  uuid.Must(uuid.NewV7()),
		ResourceKind: "workspace_pty",
		ResourceID:   resourceID,
		StreamName:   "output",
		AfterOffset:  5,
		Limit:        25,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if last != 10 || len(rows) != 1 {
		t.Fatalf("last=%d rows=%d", last, len(rows))
	}
	if rows[0].ID == "" || rows[0].Stream != "output" || rows[0].OffsetStart != 5 || rows[0].OffsetEnd != 10 || string(rows[0].Data) != "hello" {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[0].ObservedAt.IsZero() || rows[0].CreatedAt.IsZero() {
		t.Fatalf("timestamps were not parsed: %+v", rows[0])
	}
}

func TestHistoricalRowsDeclareClickHouseTagsForSelectedColumns(t *testing.T) {
	assertClickHouseTags(t, eventRow{}, []string{
		"seq", "run_id", "deployment_id", "attempt_id", "run_lease_id", "attempt_number",
		"trace_id", "span_id", "traceparent", "category", "severity", "source",
		"event_kind", "message", "body", "redaction_class", "observed_at",
	})
	assertClickHouseTags(t, runLogRow{}, []string{
		"run_id", "run_lease_id", "attempt_id", "attempt_number", "stream_name",
		"seq", "observed_seq", "content", "size_bytes", "observed_at",
	})
	assertClickHouseTags(t, terminalOutputHistoryRow{}, []string{
		"stream_name", "offset_start", "offset_end", "content", "observed_at", "ingested_at",
	})
}

type fakeHistoricalClient struct {
	selectFunc func(context.Context, any, string, ...any) error
}

func (c *fakeHistoricalClient) Select(ctx context.Context, dest any, query string, args ...any) error {
	return c.selectFunc(ctx, dest, query, args...)
}

func namedArgs(args []any) map[string]any {
	values := make(map[string]any, len(args))
	for _, arg := range args {
		named, ok := arg.(chdriver.NamedValue)
		if !ok {
			continue
		}
		values[named.Name] = named.Value
	}
	return values
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
