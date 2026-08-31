package db_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestTelemetryOutboxSinkErrorsStayIndependentAndGCGates(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)

	if _, err := queries.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          pgvalue.UUID(ids.orgID),
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		DeploymentID:   pgvalue.UUID(ids.deploymentID),
		Category:       "system",
		Severity:       "info",
		Source:         "control",
		Kind:           "deployment.promoted",
		Message:        "promoted",
		Payload:        []byte(`{}`),
		RedactionClass: "internal",
	}); err != nil {
		t.Fatal(err)
	}

	var eventID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM telemetry_outbox
		 WHERE stream_kind = 'event' AND deployment_id = $1
	`, ids.deploymentID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET ingest_error = 'clickhouse failed',
		       publish_error = 'redis failed'
		 WHERE id = $1
	`, eventID)

	claimed, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(claimed) != 1 || claimed[0].OutboxID != eventID {
		t.Fatalf("claim ingest = %+v err=%v", claimed, err)
	}
	var ingestError, publishError string
	if err := pool.QueryRow(ctx, `
		SELECT ingest_error, publish_error FROM telemetry_outbox WHERE id = $1
	`, eventID).Scan(&ingestError, &publishError); err != nil {
		t.Fatal(err)
	}
	if ingestError != "clickhouse failed" || publishError != "redis failed" {
		t.Fatalf("after ingest claim errors = ingest %q publish %q", ingestError, publishError)
	}

	if err := queries.MarkTelemetryOutboxWritten(ctx, []int64{eventID}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT ingest_error, publish_error FROM telemetry_outbox WHERE id = $1
	`, eventID).Scan(&ingestError, &publishError); err != nil {
		t.Fatal(err)
	}
	if ingestError != "" || publishError != "redis failed" {
		t.Fatalf("after ingest success errors = ingest %q publish %q", ingestError, publishError)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox SET ingest_error = 'clickhouse failed' WHERE id = $1
	`, eventID)
	if err := queries.MarkLiveTelemetryOutboxFailed(ctx, db.MarkLiveTelemetryOutboxFailedParams{
		ID:           eventID,
		RetryAfter:   pgvalue.Interval(-time.Second),
		PublishError: "redis failed again",
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT ingest_error, publish_error FROM telemetry_outbox WHERE id = $1
	`, eventID).Scan(&ingestError, &publishError); err != nil {
		t.Fatal(err)
	}
	if ingestError != "clickhouse failed" || publishError != "redis failed again" {
		t.Fatalf("after publish fail errors = ingest %q publish %q", ingestError, publishError)
	}

	live, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(live) != 1 || live[0].OutboxID != eventID {
		t.Fatalf("claim live = %+v err=%v", live, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT ingest_error, publish_error FROM telemetry_outbox WHERE id = $1
	`, eventID).Scan(&ingestError, &publishError); err != nil {
		t.Fatal(err)
	}
	if ingestError != "clickhouse failed" || publishError != "redis failed again" {
		t.Fatalf("after publish claim errors = ingest %q publish %q", ingestError, publishError)
	}
	if err := queries.MarkLiveTelemetryOutboxPublished(ctx, eventID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT ingest_error, publish_error FROM telemetry_outbox WHERE id = $1
	`, eventID).Scan(&ingestError, &publishError); err != nil {
		t.Fatal(err)
	}
	if ingestError != "clickhouse failed" || publishError != "" {
		t.Fatalf("after publish success errors = ingest %q publish %q", ingestError, publishError)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET published_at = NULL,
		       publish_locked_until = NULL
		 WHERE id = $1
	`, eventID)

	runID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, stream_name, content, size_bytes, observed_seq, source, kind, message
		) VALUES
			($1, 'run_log', 'run', $2, $3, $4, $2, 'stdout', '\x00', 1, 1, 'worker', 'run.log', 'run.log'),
			($1, 'run_log', 'run', $5, $3, $4, $5, 'stdout', '\x00', 1, 1, 'worker', 'run.log', 'run.log')
	`, ids.orgID, runID, ids.projectID, ids.environmentID, uuid.NewV7())
	var runLogFreshID, runLogStaleID int64
	if err := pool.QueryRow(ctx, `
		SELECT min(id), max(id) FROM telemetry_outbox WHERE stream_kind = 'run_log'
	`).Scan(&runLogFreshID, &runLogStaleID); err != nil {
		t.Fatal(err)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET written_at = now() - interval '23 hours',
		       state = 'written'
		 WHERE id = $1
	`, runLogFreshID)
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET written_at = now() - interval '25 hours',
		       state = 'written'
		 WHERE id = $1 OR id = $2
	`, eventID, runLogStaleID)

	pruned, err := queries.PruneTelemetryOutboxWritten(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != runLogStaleID {
		t.Fatalf("pruned = %v, want only 25h run log %d", pruned, runLogStaleID)
	}
	var retained int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM telemetry_outbox WHERE id = $1
	`, runLogFreshID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("23h run log retained = %d, want 1", retained)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET published_at = now() - interval '25 hours'
		 WHERE id = $1
	`, eventID)
	pruned, err = queries.PruneTelemetryOutboxWritten(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != eventID {
		t.Fatalf("pruned after both sinks = %v, want event %d", pruned, eventID)
	}
}

func TestTelemetryOutboxLeaseExpiryReclaimAndSourceOrder(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)

	if _, err := queries.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          pgvalue.UUID(ids.orgID),
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		DeploymentID:   pgvalue.UUID(ids.deploymentID),
		Category:       "system",
		Severity:       "info",
		Source:         "control",
		Kind:           "deployment.promoted",
		Message:        "first",
		Payload:        []byte(`{}`),
		RedactionClass: "internal",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          pgvalue.UUID(ids.orgID),
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		DeploymentID:   pgvalue.UUID(ids.deploymentID),
		Category:       "system",
		Severity:       "info",
		Source:         "control",
		Kind:           "deployment.ready",
		Message:        "second",
		Payload:        []byte(`{}`),
		RedactionClass: "internal",
	}); err != nil {
		t.Fatal(err)
	}
	var firstEventID, secondEventID int64
	if err := pool.QueryRow(ctx, `
		SELECT min(id), max(id) FROM telemetry_outbox
		 WHERE stream_kind = 'event' AND deployment_id = $1
	`, ids.deploymentID).Scan(&firstEventID, &secondEventID); err != nil {
		t.Fatal(err)
	}
	if firstEventID == secondEventID {
		t.Fatal("expected two deployment events")
	}

	ingestClaimed, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(ingestClaimed) != 1 || ingestClaimed[0].OutboxID != firstEventID {
		t.Fatalf("ingest claim = %+v err=%v", ingestClaimed, err)
	}
	ingestHeld, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(ingestHeld) != 1 || ingestHeld[0].OutboxID != secondEventID {
		t.Fatalf("ingest while first leased = %+v err=%v, want only later event %d", ingestHeld, err, secondEventID)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox SET next_retry_at = now() - interval '1 second' WHERE id = $1
	`, firstEventID)
	ingestReclaimed, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(ingestReclaimed) != 1 || ingestReclaimed[0].OutboxID != firstEventID {
		t.Fatalf("ingest reclaim = %+v err=%v, want %d", ingestReclaimed, err, firstEventID)
	}

	runID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, stream_name, content, size_bytes, observed_seq, source, kind, message
		) VALUES (
			$1, 'run_log', 'run', $2, $3, $4,
			$2, 'stdout', '\x00', 1, 1, 'worker', 'run.log', 'run.log'
		)
	`, ids.orgID, runID, ids.projectID, ids.environmentID)
	var runLogID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM telemetry_outbox WHERE stream_kind = 'run_log' AND run_id = $1
	`, runID).Scan(&runLogID); err != nil {
		t.Fatal(err)
	}
	runLogClaimed, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(runLogClaimed) != 1 || runLogClaimed[0].OutboxID != runLogID {
		t.Fatalf("run log ingest claim = %+v err=%v", runLogClaimed, err)
	}
	runLogHeld, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(runLogHeld) != 0 {
		t.Fatalf("run log ingest while leased = %+v err=%v", runLogHeld, err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox SET next_retry_at = now() - interval '1 second' WHERE id = $1
	`, runLogID)
	runLogReclaimed, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(runLogReclaimed) != 1 || runLogReclaimed[0].OutboxID != runLogID {
		t.Fatalf("run log ingest reclaim = %+v err=%v, want %d", runLogReclaimed, err, runLogID)
	}

	liveHeld, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(liveHeld) != 1 || liveHeld[0].OutboxID != firstEventID {
		t.Fatalf("live claim = %+v err=%v, want earlier event %d", liveHeld, err, firstEventID)
	}
	liveBlocked, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(liveBlocked) != 0 {
		t.Fatalf("live claim while earlier unpublished = %+v err=%v", liveBlocked, err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox SET publish_locked_until = now() - interval '1 second' WHERE id = $1
	`, firstEventID)
	liveReclaimed, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(liveReclaimed) != 1 || liveReclaimed[0].OutboxID != firstEventID {
		t.Fatalf("live reclaim = %+v err=%v, want %d", liveReclaimed, err, firstEventID)
	}
	if err := queries.MarkLiveTelemetryOutboxPublished(ctx, firstEventID); err != nil {
		t.Fatal(err)
	}
	liveNext, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(liveNext) != 1 || liveNext[0].OutboxID != secondEventID {
		t.Fatalf("live claim after earlier published = %+v err=%v, want %d", liveNext, err, secondEventID)
	}
}
