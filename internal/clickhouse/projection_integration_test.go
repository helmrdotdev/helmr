package clickhouse

import (
	"context"
	"encoding/base64"
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
	return c
}

func TestProjectionBinaryContentAndFilteredPagination(t *testing.T) {
	c := disposableClient(t)
	writer, reader := NewWriter(c), NewReader(c)
	org, run, lease := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	now := time.Now().UTC().Truncate(time.Millisecond)
	contents := [][]byte{
		{0, 0xff, 0x80, 'A'}, []byte("aGVsbG8="),
		[]byte(`{"level":"error","message":"stdout is not structured"}`),
		[]byte(`{"level":"error","message":"first"}`),
		[]byte(`{"level":"info","message":"middle"}`),
		[]byte(`{"level":"error","message":"last"}`),
	}
	rows := make([]telemetry.RunLogRecord, len(contents))
	for i, content := range contents {
		stream := "stdout"
		if i >= 3 {
			stream = "structured"
		}
		rows[i] = telemetry.RunLogRecord{OrgID: org, RunID: run, RunLeaseID: lease,
			AttemptNumber: 1, StreamName: stream, Seq: uint64(i + 1), ObservedSeq: uint64(i + 11),
			Content: content, SizeBytes: uint32(len(content)), ObservedAt: now, AcceptedAt: now}
	}
	if rejected, err := writer.WriteRunLogs(t.Context(), rows); err != nil || len(rejected) != 0 {
		t.Fatalf("write: %v, %v", rejected, err)
	}
	query := telemetry.RunLogChunkQuery{OrgID: org, RunID: run, Limit: 2}
	for after := int64(0); after < int64(len(rows)); {
		query.AfterSeq = after
		page, err := reader.ListRunLogChunks(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Chunks) != 2 || page.LastSeq != after+2 {
			t.Fatalf("page: %+v", page)
		}
		for i, chunk := range page.Chunks {
			want := rows[int(after)+i]
			if chunk.ContentBase64 != base64.StdEncoding.EncodeToString(want.Content) || chunk.Bytes != int64(want.SizeBytes) || chunk.ObservedSeq != int64(want.ObservedSeq) || !chunk.At.Equal(now) {
				t.Fatalf("binary roundtrip: %+v, want %+v", chunk, want)
			}
		}
		after = page.LastSeq
	}
	query.Levels, query.AfterSeq, query.Limit = []string{"error"}, 0, 1
	for _, want := range []int64{4, 6} {
		page, err := reader.ListRunLogChunks(t.Context(), query)
		if err != nil || len(page.Chunks) != 1 || page.LastSeq != want {
			t.Fatalf("filtered page: %+v, %v", page, err)
		}
		query.AfterSeq = page.LastSeq
	}
	page, err := reader.ListRunLogChunks(t.Context(), query)
	if err != nil || len(page.Chunks) != 0 || page.LastSeq != 6 {
		t.Fatalf("filtered end: %+v, %v", page, err)
	}
	query.OrgID = uuid.NewV7()
	page, err = reader.ListRunLogChunks(t.Context(), query)
	if err != nil || len(page.Chunks) != 0 {
		t.Fatalf("tenant isolation: %+v, %v", page, err)
	}
}

func TestProjectionReplayKeepsAcceptedPartitionAndRetention(t *testing.T) {
	c := disposableClient(t)
	writer, reader := NewWriter(c), NewReader(c)
	org, run, deployment, lease := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	accepted := time.Now().UTC().Truncate(time.Millisecond).Add(-48 * time.Hour)
	observed := accepted.Add(-time.Hour)
	for _, table := range []string{"events", "run_logs"} {
		if err := c.Exec(t.Context(), "SYSTEM STOP MERGES helmr_telemetry."+table); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := c.Exec(context.Background(), "SYSTEM START MERGES helmr_telemetry."+table); err != nil {
				t.Error(err)
			}
		})
	}
	events := []telemetry.EventRecord{
		{OrgID: org, SubjectKind: "run", SubjectID: run, RunID: &run, RunLeaseID: &lease, EventKind: "run.test", Seq: 1, Body: `{}`, ObservedAt: observed, AcceptedAt: accepted},
		{OrgID: org, SubjectKind: "deployment", SubjectID: deployment, DeploymentID: &deployment, EventKind: "deployment.test", Seq: 1, Body: `{}`, ObservedAt: observed, AcceptedAt: accepted},
	}
	if rejected, err := writer.WriteEvents(t.Context(), events); err != nil || len(rejected) != 0 {
		t.Fatalf("events: %v, %v", rejected, err)
	}
	logs := []telemetry.RunLogRecord{{OrgID: org, RunID: run, RunLeaseID: lease, StreamName: "stderr", Seq: 1, Content: []byte("x"), SizeBytes: 1, ObservedAt: observed, AcceptedAt: accepted}}
	if rejected, err := writer.WriteRunLogs(t.Context(), logs); err != nil || len(rejected) != 0 {
		t.Fatalf("logs: %v, %v", rejected, err)
	}
	for _, table := range []string{"events", "run_logs"} {
		except := "ingested_at"
		if table == "run_logs" {
			except += ", level"
		}
		if err := c.Exec(t.Context(), "INSERT INTO helmr_telemetry."+table+" SELECT * EXCEPT ("+except+"), ingested_at + INTERVAL 1 DAY FROM helmr_telemetry."+table+" WHERE org_id = ?", org); err != nil {
			t.Fatal(err)
		}
		wantCount := 2
		if table == "events" {
			wantCount = 4
		}
		if err := c.Exec(t.Context(), "SELECT throwIf(count() != ? OR uniqExact(_partition_id) != 1 OR uniqExact(accepted_at) != 1 OR uniqExact(toDate(ingested_at)) != 2 OR toUnixTimestamp64Milli(min(accepted_at + INTERVAL 90 DAY)) != ?, 'unstable replay retention') FROM helmr_telemetry."+table+" WHERE org_id = ?", wantCount, accepted.Add(90*24*time.Hour).UnixMilli(), org); err != nil {
			t.Fatal(err)
		}
		if err := c.Exec(t.Context(), "SELECT throwIf(position(create_table_query, 'TTL accepted_at + toIntervalDay(90)') = 0 OR position(create_table_query, 'PARTITION BY toDate(accepted_at)') = 0, 'unexpected retention DDL') FROM system.tables WHERE database = 'helmr_telemetry' AND name = ?", table); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range events {
		page, err := reader.ListEvents(t.Context(), telemetry.EventQuery{OrgID: org, SubjectType: event.SubjectKind, SubjectID: event.SubjectID, Limit: 10})
		if err != nil || len(page.Events) != 1 || page.LastSeq != 1 {
			t.Fatalf("events FINAL: %+v, %v", page, err)
		}
		got := page.Events[0]
		if event.SubjectKind == "deployment" {
			if got.RunID != nil || got.DeploymentID == nil || *got.DeploymentID != deployment.String() || got.AttemptNumber != nil {
				t.Fatalf("deployment identity: %+v", got)
			}
		} else if got.DeploymentID != nil || got.RunID == nil || *got.RunID != run.String() {
			t.Fatalf("run identity: %+v", got)
		}
		if !got.At.Equal(observed) {
			t.Fatalf("observed timestamp changed: %s", got.At)
		}
	}
	page, err := reader.ListRunLogChunks(t.Context(), telemetry.RunLogChunkQuery{OrgID: org, RunID: run, Limit: 10})
	if err != nil || len(page.Chunks) != 1 {
		t.Fatalf("logs FINAL: %+v, %v", page, err)
	}
}
