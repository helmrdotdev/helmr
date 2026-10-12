package clickhouse

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

// These tests require an isolated server with the checked-in schema applied.
func disposableClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("HELMR_TEST_CLICKHOUSE_URL")
	cfg := Config{URL: url}
	managed := url == "" && os.Getenv("HELMR_TEST_CLICKHOUSE_BOOTSTRAP") == "1"
	if managed {
		cfg = startClickHouseTestServer(t)
	} else if url == "" {
		t.Skip("HELMR_TEST_CLICKHOUSE_URL is not set")
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if managed {
		ddl, err := os.ReadFile("schema/migrations/000001_telemetry.sql")
		if err != nil {
			t.Fatal(err)
		}
		for statement := range strings.SplitSeq(string(ddl), ";") {
			if strings.TrimSpace(statement) != "" {
				if err := c.Exec(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	if managed {
		var tables []struct {
			Name string `ch:"name"`
		}
		if err := c.Select(t.Context(), &tables, "SELECT name FROM system.tables WHERE database = 'helmr_telemetry' ORDER BY name LIMIT 10"); err != nil {
			t.Fatal(err)
		}
		want := []string{"computer_command_logs", "computer_preparation_logs", "session_logs"}
		if len(tables) != len(want) {
			t.Fatalf("unexpected bootstrap histories: %+v", tables)
		}
		for i, name := range want {
			if tables[i].Name != name {
				t.Fatalf("unexpected bootstrap histories: %+v", tables)
			}
		}
	}
	return c
}

func TestProjectionReplayKeepsAcceptedPartitionAndRetention(t *testing.T) {
	c := disposableClient(t)
	writer, reader := NewWriter(c), NewReader(c)
	org, environment, command, otherCommand := uuid.NewV7(), uuid.NewV7(), uuid.New(), uuid.New()
	accepted := time.Now().UTC().Truncate(time.Microsecond).Add(-48 * time.Hour)

	if err := c.Exec(t.Context(), "SYSTEM STOP MERGES helmr_telemetry.computer_command_logs"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Exec(context.Background(), "SYSTEM START MERGES helmr_telemetry.computer_command_logs"); err != nil {
			t.Error(err)
		}
	})

	var rows []telemetry.StoredDiagnostic
	for _, id := range []uuid.UUID{otherCommand, command} {
		rows = append(rows, diagnosticRows(telemetry.DiagnosticSource{EnvironmentID: environment, Kind: "computer_command", ID: id, ProducerEpoch: 1}, 1, 5, accepted)...)
	}
	if rejected, err := writer.WriteDiagnostics(t.Context(), "computer_command", rows); err != nil || len(rejected) != 0 {
		t.Fatalf("diagnostics: %v, %v", rejected, err)
	}

	if err := c.Exec(t.Context(), "INSERT INTO helmr_telemetry.computer_command_logs SELECT * EXCEPT (ingested_at), ingested_at + INTERVAL 1 DAY FROM helmr_telemetry.computer_command_logs WHERE environment_id = ?", environment); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), "SELECT throwIf(count() != 4 OR uniqExact(_partition_id) != 1 OR uniqExact(accepted_at) != 1 OR uniqExact(toDate(ingested_at)) != 2 OR toUnixTimestamp64Micro(min(expires_at)) != ?, 'unstable replay retention') FROM helmr_telemetry.computer_command_logs WHERE environment_id = ?", accepted.Add(90*24*time.Hour).UnixMicro(), environment); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), "SELECT throwIf(position(create_table_query, 'TTL expires_at') = 0 OR position(create_table_query, 'PARTITION BY toDate(accepted_at)') = 0, 'unexpected retention DDL') FROM system.tables WHERE database = 'helmr_telemetry' AND name = 'computer_command_logs'"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []uuid.UUID{otherCommand, command} {
		page, err := reader.ListCommandLogChunks(t.Context(), telemetry.CommandLogChunkQuery{OrgID: org, EnvironmentID: environment, CommandID: id, Stream: "stdout", Limit: 10})
		if err != nil || len(page.Chunks) != 1 || page.Chunks[0].ThroughSequence != 1 {
			t.Fatalf("command logs FINAL: %+v, %v", page, err)
		}
	}
}
