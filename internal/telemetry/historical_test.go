package telemetry

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
)

func TestHistoricalReaderListsTerminalOutputFromClickHouse(t *testing.T) {
	var query string
	resourceID := uuid.Must(uuid.NewV7())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		if !strings.Contains(query, "helmr_telemetry.terminal_output FINAL") {
			t.Fatalf("query = %q, want terminal_output FINAL", query)
		}
		if !strings.Contains(query, "offset_end > {after:UInt64}") || !strings.Contains(query, "offset_end <= {watermark:UInt64}") {
			t.Fatalf("query = %q, want bounded historical offsets", query)
		}
		if r.URL.Query().Get("param_after") != "5" || r.URL.Query().Get("param_watermark") != "10" {
			t.Fatalf("params after=%q watermark=%q, want 5/10", r.URL.Query().Get("param_after"), r.URL.Query().Get("param_watermark"))
		}
		if r.URL.Query().Get("param_resource_id") != resourceID.String() {
			t.Fatalf("param_resource_id = %q, want %s", r.URL.Query().Get("param_resource_id"), resourceID)
		}
		_, _ = w.Write([]byte(`{"stream_name":"output","offset_start":5,"offset_end":10,"content":"` + base64.StdEncoding.EncodeToString([]byte("hello")) + `","observed_at":"2026-07-02 01:02:03.123","ingested_at":"2026-07-02 01:02:04.456"}` + "\n"))
	}))
	defer server.Close()

	client, err := clickhouse.New(clickhouse.Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
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
	if !strings.Contains(query, "resource_id = {resource_id:UUID}") {
		t.Fatalf("query = %q, want resource scope placeholder", query)
	}
}
