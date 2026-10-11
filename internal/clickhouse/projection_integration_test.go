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
		want := []string{"computer_command_logs", "computer_preparation_logs", "events", "session_logs"}
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
	org, deployment, otherDeployment := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	accepted := time.Now().UTC().Truncate(time.Millisecond).Add(-48 * time.Hour)
	observed := accepted.Add(-time.Hour)

	if err := c.Exec(t.Context(), "SYSTEM STOP MERGES helmr_telemetry.events"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Exec(context.Background(), "SYSTEM START MERGES helmr_telemetry.events"); err != nil {
			t.Error(err)
		}
	})

	events := []telemetry.EventRecord{
		{OrgID: org, SubjectKind: "deployment", SubjectID: otherDeployment, DeploymentID: &otherDeployment, EventKind: "deployment.test", Seq: 1, Body: `{}`, ObservedAt: observed, AcceptedAt: accepted},
		{OrgID: org, SubjectKind: "deployment", SubjectID: deployment, DeploymentID: &deployment, EventKind: "deployment.test", Seq: 1, Body: `{}`, ObservedAt: observed, AcceptedAt: accepted},
	}
	if rejected, err := writer.WriteEvents(t.Context(), events); err != nil || len(rejected) != 0 {
		t.Fatalf("events: %v, %v", rejected, err)
	}

	if err := c.Exec(t.Context(), "INSERT INTO helmr_telemetry.events SELECT * EXCEPT (ingested_at), ingested_at + INTERVAL 1 DAY FROM helmr_telemetry.events WHERE org_id = ?", org); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), "SELECT throwIf(count() != 4 OR uniqExact(_partition_id) != 1 OR uniqExact(accepted_at) != 1 OR uniqExact(toDate(ingested_at)) != 2 OR toUnixTimestamp64Milli(min(accepted_at + INTERVAL 90 DAY)) != ?, 'unstable replay retention') FROM helmr_telemetry.events WHERE org_id = ?", accepted.Add(90*24*time.Hour).UnixMilli(), org); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), "SELECT throwIf(position(create_table_query, 'TTL accepted_at + toIntervalDay(90)') = 0 OR position(create_table_query, 'PARTITION BY toDate(accepted_at)') = 0, 'unexpected retention DDL') FROM system.tables WHERE database = 'helmr_telemetry' AND name = 'events'"); err != nil {
		t.Fatal(err)
	}

	for _, event := range events {
		page, err := reader.ListEvents(t.Context(), telemetry.EventQuery{OrgID: org, SubjectType: event.SubjectKind, SubjectID: event.SubjectID, Limit: 10})
		if err != nil || len(page.Events) != 1 || page.LastSeq != 1 {
			t.Fatalf("events FINAL: %+v, %v", page, err)
		}
		got := page.Events[0]
		if got.DeploymentID == nil || *got.DeploymentID != event.SubjectID.String() {
			t.Fatalf("deployment identity: %+v", got)
		}

		if !got.At.Equal(observed) {
			t.Fatalf("observed timestamp changed: %s", got.At)
		}
	}
}
