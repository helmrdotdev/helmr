package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEventClaimUsesLongestByteBoundedPrefix(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)
	ids := seedPostgres(t, ctx, pool)
	const maxBatchBytes = int64(8 << 20)
	prefix := `{"data":"`
	suffix := `"}`
	payload := prefix + strings.Repeat("x", (64<<10)-len(prefix)-len(suffix)) + suffix

	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			environment_id, deployment_id, stream_kind, kind, message, payload
		)
		SELECT $1, $2, 'event', 'deployment.test', repeat('m', 4 * 1024), $3::jsonb
		  FROM generate_series(1, 121)
	`, ids.environmentID, ids.deploymentID, payload)

	var orderedIDs []int64
	rows, err := pool.Query(ctx, `
		SELECT id FROM telemetry_outbox WHERE deployment_id = $1 ORDER BY id ASC
	`, ids.deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		orderedIDs = append(orderedIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	claimed, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 250, MaxBatchBytes: maxBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 120 {
		t.Fatalf("claimed rows = %d, want 120 maximum-size events", len(claimed))
	}
	for idx, row := range claimed {
		if row.OutboxID != orderedIDs[idx] {
			t.Fatalf("claimed row %d id = %d, want source-order id %d", idx, row.OutboxID, orderedIDs[idx])
		}
	}
	claimed, err = queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 250, MaxBatchBytes: maxBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].OutboxID != orderedIDs[120] {
		t.Fatalf("second claim = %+v, want remaining source-order event", claimed)
	}

	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			environment_id, deployment_id, stream_kind, kind, message, payload
		)
		SELECT $1, $2, 'event', 'deployment.test', '', '{}'::jsonb
		  FROM generate_series(1, 300)
	`, ids.environmentID, ids.deploymentID)
	claimed, err = queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 250, MaxBatchBytes: maxBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 250 {
		t.Fatalf("small event claim rows = %d, want row ceiling 250", len(claimed))
	}
}

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
		MaxBatchBytes: 1 << 20,
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

	writeParams := db.MarkTelemetryOutboxWrittenParams{
		Ids: []int64{eventID}, ExpectedRetryCounts: []int32{claimed[0].RetryCount},
	}
	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, writeParams); err != nil || updated != 1 {
		t.Fatalf("mark written updated = %d err = %v, want 1", updated, err)
	}
	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, writeParams); err != nil || updated != 0 {
		t.Fatalf("second mark written updated = %d err = %v, want 0", updated, err)
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
	if updated, err := queries.MarkLiveTelemetryOutboxBatchFailed(ctx, db.MarkLiveTelemetryOutboxBatchFailedParams{
		Ids:                     []int64{eventID},
		ExpectedPublishAttempts: []int32{0},
		RetryAfters:             []pgtype.Interval{pgvalue.Interval(-time.Second)},
		PublishErrors:           []string{"redis failed again"},
	}); err != nil || updated != 1 {
		t.Fatalf("mark live failed updated = %d err = %v, want 1", updated, err)
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
	if updated, err := queries.MarkLiveTelemetryOutboxBatchPublished(ctx, db.MarkLiveTelemetryOutboxBatchPublishedParams{
		Ids: []int64{eventID}, ExpectedPublishAttempts: []int32{live[0].Attempts},
	}); err != nil || updated != 1 {
		t.Fatalf("mark live published updated = %d err = %v, want 1", updated, err)
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

	var freshID int64
	if err := pool.QueryRow(ctx, `INSERT INTO telemetry_outbox(environment_id,deployment_id,stream_kind,kind,written_at,published_at) VALUES($1,$2,'event','deployment.ready',now()-interval '23 hours',now()) RETURNING id`, ids.environmentID, ids.deploymentID).Scan(&freshID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `UPDATE telemetry_outbox SET written_at=now()-interval '25 hours' WHERE id=$1`, eventID)
	lifecycle, err := queries.GetTelemetryOutboxLifecycle(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil || lifecycle.OldestGcWrittenAt.Valid {
		t.Fatalf("unpublished event became collectible: %+v %v", lifecycle, err)
	}
	pruned, err := queries.PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 1})
	if err != nil || pruned != 0 {
		t.Fatalf("pruned unpublished or fresh event: %d %v", pruned, err)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET published_at = now() - interval '25 hours'
		 WHERE id = $1
	`, eventID)
	pruned, err = queries.PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{
		RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned after both sinks = %d, want event %d", pruned, eventID)
	}
	var eventRetained int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM telemetry_outbox WHERE id = $1`, eventID).Scan(&eventRetained); err != nil {
		t.Fatal(err)
	}
	if eventRetained != 0 {
		t.Fatalf("published event retained = %d, want 0", eventRetained)
	}
	lifecycle, err = queries.GetTelemetryOutboxLifecycle(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.OldestGcWrittenAt.Valid {
		t.Fatalf("oldest GC row = %v, want none", lifecycle.OldestGcWrittenAt.Time)
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
	lifecycle, err := queries.GetTelemetryOutboxLifecycle(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !lifecycle.OldestRetryCreatedAt.Valid {
		t.Fatal("oldest retry-eligible age is missing")
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
		MaxBatchBytes: 1 << 20,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(ingestClaimed) != 1 || ingestClaimed[0].OutboxID != firstEventID {
		t.Fatalf("ingest claim = %+v err=%v", ingestClaimed, err)
	}
	ingestHeld, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit:      2,
		MaxBatchBytes: 1 << 20,
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
		MaxBatchBytes: 1 << 20,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(ingestReclaimed) != 1 || ingestReclaimed[0].OutboxID != firstEventID {
		t.Fatalf("ingest reclaim = %+v err=%v, want %d", ingestReclaimed, err, firstEventID)
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
	if updated, err := queries.MarkLiveTelemetryOutboxBatchPublished(ctx, db.MarkLiveTelemetryOutboxBatchPublishedParams{
		Ids: []int64{firstEventID}, ExpectedPublishAttempts: []int32{liveReclaimed[0].Attempts},
	}); err != nil || updated != 1 {
		t.Fatalf("mark first live published updated = %d err = %v, want 1", updated, err)
	}
	liveNext, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit:      2,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(liveNext) != 1 || liveNext[0].OutboxID != secondEventID {
		t.Fatalf("live claim after earlier published = %+v err=%v, want %d", liveNext, err, secondEventID)
	}
}

func TestTelemetryOutboxIngestResultsFenceReclaimedGenerations(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)

	insertEvent := func(label string) int64 {
		t.Helper()
		sourceID := uuid.NewV7()
		dbtest.MustExec(t, ctx, pool, `INSERT INTO deployments(environment_id,id,bundle_digest) SELECT environment_id,$2,$3 FROM deployments WHERE id=$1`, ids.deploymentID, sourceID, dbtest.Digest(sourceID.String()))
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				environment_id, deployment_id, stream_kind, kind, message
			) VALUES ($1, $2, 'event', 'deployment.ready', $3)
			RETURNING id
		`, ids.environmentID, sourceID, label).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	eventSuccessID := insertEvent("event-success")
	eventFailureID := insertEvent("event-failure")
	otherEventSuccessID := insertEvent("other event-success")
	otherEventFailureID := insertEvent("other event-failure")
	eventClaims, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 4, MaxBatchBytes: 1 << 20, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(eventClaims) != 4 {
		t.Fatalf("event claims = %+v err=%v", eventClaims, err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET next_retry_at = now() - interval '1 second'
		 WHERE id = ANY($1::bigint[])
	`, []int64{eventSuccessID, eventFailureID, otherEventSuccessID, otherEventFailureID})
	eventReclaims, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 4, MaxBatchBytes: 1 << 20, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(eventReclaims) != 4 {
		t.Fatalf("event reclaims = %+v err=%v", eventReclaims, err)
	}

	type ingestState struct {
		retryCount  int32
		nextRetryAt string
		ingestError string
		written     bool
	}
	readState := func(id int64) ingestState {
		t.Helper()
		var value ingestState
		if err := pool.QueryRow(ctx, `
			SELECT retry_count, COALESCE(next_retry_at::text, ''), ingest_error,
			       written_at IS NOT NULL
			  FROM telemetry_outbox WHERE id = $1
		`, id).Scan(&value.retryCount, &value.nextRetryAt, &value.ingestError, &value.written); err != nil {
			t.Fatal(err)
		}
		return value
	}
	eventSuccessReclaimed := readState(eventSuccessID)
	eventFailureReclaimed := readState(eventFailureID)
	otherEventSuccessReclaimed := readState(otherEventSuccessID)
	otherEventFailureReclaimed := readState(otherEventFailureID)

	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids: []int64{eventSuccessID, otherEventSuccessID},
		ExpectedRetryCounts: []int32{
			eventClaims[0].RetryCount,
			eventClaims[2].RetryCount,
		},
	}); err != nil || updated != 0 {
		t.Fatalf("stale success updated = %d err=%v, want 0", updated, err)
	}
	if got := readState(eventSuccessID); got != eventSuccessReclaimed {
		t.Fatalf("stale success changed reclaimed row: got %+v want %+v", got, eventSuccessReclaimed)
	}
	if got := readState(otherEventSuccessID); got != otherEventSuccessReclaimed {
		t.Fatalf("stale other event success changed reclaimed row: got %+v want %+v", got, otherEventSuccessReclaimed)
	}
	if updated, err := queries.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids: []int64{eventFailureID, otherEventFailureID},
		ExpectedRetryCounts: []int32{
			eventClaims[1].RetryCount,
			eventClaims[3].RetryCount,
		},
		RetryAfter: pgvalue.Interval(time.Minute), IngestError: "stale failure",
	}); err != nil || updated != 0 {
		t.Fatalf("stale failure updated = %d err=%v, want 0", updated, err)
	}
	if got := readState(eventFailureID); got != eventFailureReclaimed {
		t.Fatalf("stale failure changed reclaimed row: got %+v want %+v", got, eventFailureReclaimed)
	}
	if got := readState(otherEventFailureID); got != otherEventFailureReclaimed {
		t.Fatalf("stale other event failure changed reclaimed row: got %+v want %+v", got, otherEventFailureReclaimed)
	}

	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids:                 []int64{eventSuccessID, otherEventSuccessID},
		ExpectedRetryCounts: []int32{eventReclaims[0].RetryCount, eventClaims[2].RetryCount},
	}); err != nil || updated != 1 {
		t.Fatalf("mixed current/stale success updated = %d err=%v, want 1", updated, err)
	}
	if updated, err := queries.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids:                 []int64{eventFailureID, otherEventFailureID},
		ExpectedRetryCounts: []int32{eventReclaims[1].RetryCount, eventClaims[3].RetryCount},
		RetryAfter:          pgvalue.Interval(time.Minute),
		IngestError:         "new event owner failure",
	}); err != nil || updated != 1 {
		t.Fatalf("mixed current/stale failure updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(otherEventSuccessID); got != otherEventSuccessReclaimed {
		t.Fatalf("mixed success changed stale other event row: got %+v want %+v", got, otherEventSuccessReclaimed)
	}
	if got := readState(otherEventFailureID); got != otherEventFailureReclaimed {
		t.Fatalf("mixed failure changed stale other event row: got %+v want %+v", got, otherEventFailureReclaimed)
	}
	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids: []int64{otherEventSuccessID}, ExpectedRetryCounts: []int32{eventReclaims[2].RetryCount},
	}); err != nil || updated != 1 {
		t.Fatalf("current other event success updated = %d err=%v, want 1", updated, err)
	}
	if updated, err := queries.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids: []int64{otherEventFailureID}, ExpectedRetryCounts: []int32{eventReclaims[3].RetryCount},
		RetryAfter: pgvalue.Interval(time.Minute), IngestError: "new other event owner failure",
	}); err != nil || updated != 1 {
		t.Fatalf("current other event failure updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(eventSuccessID); got.retryCount != 0 || !got.written {
		t.Fatalf("current event success state = %+v", got)
	}
	if got := readState(eventFailureID); got.retryCount != eventReclaims[1].RetryCount || got.ingestError != "new event owner failure" {
		t.Fatalf("current event failure state = %+v", got)
	}
	if got := readState(otherEventSuccessID); got.retryCount != 0 || !got.written {
		t.Fatalf("current other event success state = %+v", got)
	}
	if got := readState(otherEventFailureID); got.retryCount != eventReclaims[3].RetryCount || got.ingestError != "new other event owner failure" {
		t.Fatalf("current other event failure state = %+v", got)
	}
}

func TestLiveTelemetryResultsFencePartialReclaim(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)
	outboxIDs := make([]int64, 4)
	for index := range outboxIDs {
		sourceID := uuid.NewV7()
		dbtest.MustExec(t, ctx, pool, `INSERT INTO deployments(environment_id,id,bundle_digest) SELECT environment_id,$2,$3 FROM deployments WHERE id=$1`, ids.deploymentID, sourceID, dbtest.Digest(sourceID.String()))
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				environment_id, deployment_id, stream_kind, source, kind, message
			) VALUES ($1, $2, 'event', 'control', 'deployment.ready', $3)
			RETURNING id
		`, ids.environmentID, sourceID, fmt.Sprintf("partial-%d", index)).Scan(&outboxIDs[index]); err != nil {
			t.Fatal(err)
		}
	}
	firstClaims, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit: 4, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(firstClaims) != 4 {
		t.Fatalf("first live claims = %+v err=%v", firstClaims, err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET publish_locked_until = now() - interval '1 second'
		 WHERE id = ANY($1::bigint[])
	`, []int64{outboxIDs[0], outboxIDs[2]})
	reclaims, err := queries.ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
		RowLimit: 4, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(reclaims) != 2 || reclaims[0].OutboxID != outboxIDs[0] || reclaims[1].OutboxID != outboxIDs[2] {
		t.Fatalf("live reclaims = %+v err=%v", reclaims, err)
	}

	type publishState struct {
		attempts     int32
		lockedUntil  string
		publishError string
		published    bool
	}
	readState := func(id int64) publishState {
		t.Helper()
		var value publishState
		if err := pool.QueryRow(ctx, `
			SELECT publish_attempts, COALESCE(publish_locked_until::text, ''), publish_error,
			       published_at IS NOT NULL
			  FROM telemetry_outbox WHERE id = $1
		`, id).Scan(&value.attempts, &value.lockedUntil, &value.publishError, &value.published); err != nil {
			t.Fatal(err)
		}
		return value
	}
	reclaimedSuccess := readState(outboxIDs[0])
	reclaimedFailure := readState(outboxIDs[2])

	if updated, err := queries.MarkLiveTelemetryOutboxBatchPublished(ctx, db.MarkLiveTelemetryOutboxBatchPublishedParams{
		Ids:                     []int64{outboxIDs[0], outboxIDs[1]},
		ExpectedPublishAttempts: []int32{firstClaims[0].Attempts, firstClaims[1].Attempts},
	}); err != nil || updated != 1 {
		t.Fatalf("stale partial publish updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(outboxIDs[0]); got != reclaimedSuccess {
		t.Fatalf("stale publish changed reclaimed row: got %+v want %+v", got, reclaimedSuccess)
	}
	if updated, err := queries.MarkLiveTelemetryOutboxBatchFailed(ctx, db.MarkLiveTelemetryOutboxBatchFailedParams{
		Ids:                     []int64{outboxIDs[2], outboxIDs[3]},
		ExpectedPublishAttempts: []int32{firstClaims[2].Attempts, firstClaims[3].Attempts},
		RetryAfters:             []pgtype.Interval{pgvalue.Interval(time.Minute), pgvalue.Interval(2 * time.Minute)},
		PublishErrors:           []string{"stale failure", "current failure"},
	}); err != nil || updated != 1 {
		t.Fatalf("stale partial failure updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(outboxIDs[2]); got != reclaimedFailure {
		t.Fatalf("stale failure changed reclaimed row: got %+v want %+v", got, reclaimedFailure)
	}
	if updated, err := queries.MarkLiveTelemetryOutboxBatchPublished(ctx, db.MarkLiveTelemetryOutboxBatchPublishedParams{
		Ids: []int64{outboxIDs[0]}, ExpectedPublishAttempts: []int32{reclaims[0].Attempts},
	}); err != nil || updated != 1 {
		t.Fatalf("current reclaimed publish updated = %d err=%v, want 1", updated, err)
	}
	if updated, err := queries.MarkLiveTelemetryOutboxBatchFailed(ctx, db.MarkLiveTelemetryOutboxBatchFailedParams{
		Ids: []int64{outboxIDs[2]}, ExpectedPublishAttempts: []int32{reclaims[1].Attempts},
		RetryAfters:   []pgtype.Interval{pgvalue.Interval(time.Minute)},
		PublishErrors: []string{"new owner failure"},
	}); err != nil || updated != 1 {
		t.Fatalf("current reclaimed failure updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(outboxIDs[0]); !got.published || got.lockedUntil != "" || got.publishError != "" {
		t.Fatalf("current published state = %+v", got)
	}
	if got := readState(outboxIDs[2]); got.published || got.attempts != reclaims[1].Attempts || got.publishError != "new owner failure" {
		t.Fatalf("current failed state = %+v", got)
	}
}

func TestLiveTelemetryOutboxBatchCompletion(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)
	outboxIDs := make([]int64, 3)
	for index := range outboxIDs {
		sourceID := uuid.NewV7()
		dbtest.MustExec(t, ctx, pool, `INSERT INTO deployments(environment_id,id,bundle_digest) SELECT environment_id,$2,$3 FROM deployments WHERE id=$1`, ids.deploymentID, sourceID, dbtest.Digest(sourceID.String()))
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				environment_id, deployment_id, stream_kind, source, kind, message
			) VALUES ($1, $2, 'event', 'control', 'deployment.ready', $3)
			RETURNING id
		`, ids.environmentID, sourceID, fmt.Sprintf("event-%d", index)).Scan(&outboxIDs[index]); err != nil {
			t.Fatal(err)
		}
	}

	if updated, err := queries.MarkLiveTelemetryOutboxBatchPublished(ctx, db.MarkLiveTelemetryOutboxBatchPublishedParams{
		Ids: []int64{outboxIDs[0], outboxIDs[2]}, ExpectedPublishAttempts: []int32{0, 0},
	}); err != nil || updated != 2 {
		t.Fatalf("mark published updated = %d err = %v, want 2", updated, err)
	}
	retryAfter := pgvalue.Interval(time.Minute)
	if updated, err := queries.MarkLiveTelemetryOutboxBatchFailed(ctx, db.MarkLiveTelemetryOutboxBatchFailedParams{
		Ids:                     []int64{outboxIDs[1]},
		ExpectedPublishAttempts: []int32{0},
		RetryAfters:             []pgtype.Interval{retryAfter},
		PublishErrors:           []string{"redis unavailable"},
	}); err != nil || updated != 1 {
		t.Fatalf("mark failed updated = %d err = %v, want 1", updated, err)
	}

	var published bool
	var publishError string
	var retryScheduled bool
	if err := pool.QueryRow(ctx, `
		SELECT published_at IS NOT NULL, publish_error, publish_locked_until > now()
		  FROM telemetry_outbox WHERE id = $1
	`, outboxIDs[1]).Scan(&published, &publishError, &retryScheduled); err != nil {
		t.Fatal(err)
	}
	if published || publishError != "redis unavailable" || !retryScheduled {
		t.Fatalf("failed row = published %t error %q retry_scheduled %t", published, publishError, retryScheduled)
	}

	if updated, err := queries.MarkLiveTelemetryOutboxBatchFailed(ctx, db.MarkLiveTelemetryOutboxBatchFailedParams{
		Ids:                     []int64{outboxIDs[0]},
		ExpectedPublishAttempts: []int32{0},
		RetryAfters:             []pgtype.Interval{retryAfter},
		PublishErrors:           []string{"late failure"},
	}); err != nil || updated != 0 {
		t.Fatalf("published failure updated = %d err = %v, want 0", updated, err)
	}
	if updated, err := queries.MarkLiveTelemetryOutboxBatchPublished(ctx, db.MarkLiveTelemetryOutboxBatchPublishedParams{
		Ids: []int64{outboxIDs[2] + 1_000_000}, ExpectedPublishAttempts: []int32{0},
	}); err != nil || updated != 0 {
		t.Fatalf("missing published updated = %d err = %v, want 0", updated, err)
	}
}
