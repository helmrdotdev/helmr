package telemetry

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

func TestEventExportRetriesWithoutClaimingDiagnostics(t *testing.T) {
	f := agenttest.New(t)
	q := db.New(f.Pool)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	id, err := q.AppendDeploymentEvent(t.Context(), db.AppendDeploymentEventParams{OrgID: pgvalue.UUID(org), ProjectID: pgvalue.UUID(project), EnvironmentID: pgvalue.UUID(f.Environment), DeploymentID: pgvalue.UUID(f.Deployment), Kind: "deployment.promoted", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	source := diagnosticTestSource(t, f.Pool, "session", uuid.Nil())
	if _, err := appendDiagnosticTest(t, f.Pool, source, diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}, diagnosticTestBounds()); err != nil {
		t.Fatal(err)
	}
	writer := &fakeIngestWriter{eventErr: errors.New("sink unavailable")}
	ingester, err := NewIngestor(nil, q, writer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ingester.ingestEvents(t.Context()); err == nil {
		t.Fatal("sink failure was lost")
	}
	var retry int32
	if err = f.Pool.QueryRow(t.Context(), `SELECT retry_count FROM telemetry_outbox WHERE id=$1 AND written_at IS NULL`, id).Scan(&retry); err != nil || retry != 1 {
		t.Fatalf("retry=%d: %v", retry, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE telemetry_outbox SET next_retry_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	rows, err := q.ClaimEventIngestBatch(t.Context(), db.ClaimEventIngestBatchParams{RowLimit: 10, MaxBatchBytes: MaxTelemetryBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute)})
	if err != nil || len(rows) != 1 || rows[0].RetryCount != 2 || rows[0].DeploymentID != pgvalue.UUID(f.Deployment) {
		t.Fatalf("reclaim=%+v: %v", rows, err)
	}
	if n, err := q.MarkTelemetryOutboxWritten(t.Context(), db.MarkTelemetryOutboxWrittenParams{Ids: []int64{id}, ExpectedRetryCounts: []int32{1}}); err != nil || n != 0 {
		t.Fatalf("stale acknowledgment=%d: %v", n, err)
	}
	writer.eventErr = nil
	success, err := ingester.writeEventCandidates(t.Context(), []eventIngestCandidate{{outboxID: id, retryCount: rows[0].RetryCount, record: eventRecord(rows[0])}})
	if err != nil || len(success) != 1 {
		t.Fatalf("write=%+v: %v", success, err)
	}
	if n, err := q.MarkTelemetryOutboxWritten(t.Context(), db.MarkTelemetryOutboxWrittenParams{Ids: []int64{id}, ExpectedRetryCounts: []int32{2}}); err != nil || n != 1 {
		t.Fatalf("acknowledgment=%d: %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE telemetry_outbox SET written_at=clock_timestamp()-interval '2 days' WHERE id=$1`, id)
	params := db.PruneTelemetryOutboxWrittenParams{RowLimit: 10, RetainFor: pgvalue.Interval(24 * time.Hour)}
	if n, err := q.PruneTelemetryOutboxWritten(t.Context(), params); err != nil || n != 0 {
		t.Fatalf("pruned unpublished event=%d: %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE telemetry_outbox SET published_at=clock_timestamp() WHERE id=$1`, id)
	if n, err := q.PruneTelemetryOutboxWritten(t.Context(), params); err != nil || n != 1 {
		t.Fatalf("pruned event=%d: %v", n, err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox WHERE stream_kind='diagnostic' AND retry_count=0 AND written_at IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("diagnostic changed=%d: %v", count, err)
	}
}
