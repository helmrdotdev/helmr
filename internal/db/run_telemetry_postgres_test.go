package db_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTelemetryOutboxGCScaleBudget(t *testing.T) {
	if os.Getenv("HELMR_TEST_TELEMETRY_GC_SCALE") != "1" {
		t.Skip("HELMR_TEST_TELEMETRY_GC_SCALE is not set")
	}
	ctx := t.Context()
	fixture := runtest.New(t)
	pool := fixture.Pool
	queries := db.New(pool)
	const replayLeaseCount = 64
	leaseIDs := make([]pgtype.UUID, replayLeaseCount)
	runIDs := make([]pgtype.UUID, replayLeaseCount)
	for index := range replayLeaseCount {
		work := fixture.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
		leaseIDs[index] = pgvalue.UUID(work.LeaseID)
		runIDs[index] = pgvalue.UUID(work.RunID)
	}
	orgID := fixture.OrgID
	projectID := fixture.ProjectID
	environmentID := fixture.EnvironmentID

	start := time.Now()
	dbtest.MustExec(t, ctx, pool, `
		WITH replay_leases AS (
			SELECT run_lease_id, run_id, ordinality
			  FROM unnest($4::uuid[], $5::uuid[]) WITH ORDINALITY
			       AS leases(run_lease_id, run_id, ordinality)
		)
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, run_lease_id, attempt_number, stream_name, content, size_bytes,
			observed_seq, source, kind, message, state, written_at
		)
		SELECT $1, 'run_log', 'run', replay_leases.run_id, $2, $3,
		       replay_leases.run_id, replay_leases.run_lease_id, 1,
		       (ARRAY['stdout', 'stderr', 'structured'])[
		           ((generated - 1) / $6::bigint) % 3 + 1
		       ],
		       '\x00', 1, ((generated - 1) / ($6::bigint * 3)) + 1,
		       'worker', 'run.log',
		       'run.log', 'written',
		       CASE WHEN generated <= 100000
		            THEN now() - interval '25 hours'
		            ELSE now() - interval '23 hours'
		       END
		  FROM generate_series(1, 1000000) AS generated
		  JOIN replay_leases
		    ON replay_leases.ordinality = ((generated - 1) % $6::bigint) + 1
	`, orgID, projectID, environmentID, leaseIDs, runIDs, replayLeaseCount)
	t.Logf(
		"seeded 1,000,000 rows (100,000 eligible; %d real Run Leases; 3 streams; 5,208-5,209 observed sequences per lease/stream) in %s",
		replayLeaseCount,
		time.Since(start),
	)
	measureTelemetryReplayIndexes(t, ctx, pool, telemetryReplayScaleFixture{
		leaseIDs: leaseIDs,
	})
	measureTelemetryUnusedIndexes(t, ctx, pool, pgvalue.UUID(orgID), runIDs[0])

	var tableBytes, indexBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT pg_table_size('telemetry_outbox'), pg_relation_size('telemetry_outbox_written_gc_idx')
	`).Scan(&tableBytes, &indexBytes); err != nil {
		t.Fatal(err)
	}
	var tempFilesBefore, tempBytesBefore int64
	if err := pool.QueryRow(ctx, `
		SELECT temp_files, temp_bytes FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&tempFilesBefore, &tempBytesBefore); err != nil {
		t.Fatal(err)
	}

	planRows, err := pool.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT TEXT)
		WITH eligible AS (
			SELECT id FROM telemetry_outbox
			 WHERE written_at < now() - interval '24 hours'
			   AND ((stream_kind = 'event' AND published_at IS NOT NULL) OR stream_kind = 'run_log')
			 ORDER BY written_at ASC, id ASC
			 LIMIT 2500
			 FOR UPDATE SKIP LOCKED
		)
		DELETE FROM telemetry_outbox USING eligible
		 WHERE telemetry_outbox.id = eligible.id
	`)
	if err != nil {
		t.Fatal(err)
	}
	var planLines []string
	for planRows.Next() {
		var line string
		if err := planRows.Scan(&line); err != nil {
			planRows.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := planRows.Err(); err != nil {
		planRows.Close()
		t.Fatal(err)
	}
	planRows.Close()
	plan := strings.Join(planLines, "\n")
	if !strings.Contains(plan, "telemetry_outbox_written_gc_idx") {
		t.Fatalf("GC plan did not use telemetry_outbox_written_gc_idx:\n%s", plan)
	}
	t.Logf("GC explain:\n%s", plan)

	durations := make([]time.Duration, 0, 40)
	walBytes := make([]int64, 0, 40)
	totalDeleted := int64(2500)
	for totalDeleted < 100000 {
		var walBefore string
		if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&walBefore); err != nil {
			t.Fatal(err)
		}
		statementStart := time.Now()
		deleted, err := queries.PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{
			RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 2500,
		})
		durations = append(durations, time.Since(statementStart))
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 2500 {
			t.Fatalf("deleted = %d, want 2500", deleted)
		}
		totalDeleted += deleted
		var walAfter string
		var wal int64
		if err := pool.QueryRow(ctx, `
			SELECT pg_current_wal_insert_lsn()::text,
			       pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint
		`, walBefore).Scan(&walAfter, &wal); err != nil {
			t.Fatal(err)
		}
		walBytes = append(walBytes, wal)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Slice(walBytes, func(i, j int) bool { return walBytes[i] < walBytes[j] })
	p95 := durations[(len(durations)*95+99)/100-1]
	maxWAL := walBytes[len(walBytes)-1]
	var tempFilesAfter, tempBytesAfter int64
	if err := pool.QueryRow(ctx, `
		SELECT temp_files, temp_bytes FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&tempFilesAfter, &tempBytesAfter); err != nil {
		t.Fatal(err)
	}
	var eligible int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM telemetry_outbox
		 WHERE written_at < now() - interval '24 hours'
		   AND ((stream_kind = 'event' AND published_at IS NOT NULL) OR stream_kind = 'run_log')
	`).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	t.Logf("GC budget: p95=%s max_wal=%d temp_files=%d temp_bytes=%d table_bytes=%d index_bytes=%d",
		p95, maxWAL, tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore, tableBytes, indexBytes)
	if p95 > 100*time.Millisecond {
		t.Fatalf("GC statement p95 = %s, budget <= 100ms", p95)
	}
	if maxWAL > 8<<20 {
		t.Fatalf("GC max WAL = %d, budget <= %d", maxWAL, 8<<20)
	}
	if tempFilesAfter != tempFilesBefore || tempBytesAfter != tempBytesBefore {
		t.Fatalf("GC created temp files/bytes: files=%d bytes=%d", tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore)
	}
	if eligible != 0 {
		t.Fatalf("eligible rows after 40 batches = %d, want 0", eligible)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET written_at = NULL,
		       state = 'pending',
		       size_bytes = 192 * 1024,
		       next_retry_at = NULL
		 WHERE id IN (SELECT id FROM telemetry_outbox ORDER BY id ASC LIMIT 100000)
	`)
	dbtest.MustExec(t, ctx, pool, `ANALYZE telemetry_outbox`)
	claimPlanRows, err := pool.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
		WITH candidates AS (
			SELECT telemetry_outbox.id, telemetry_outbox.size_bytes
			  FROM telemetry_outbox
			 WHERE telemetry_outbox.stream_kind = 'run_log'
			   AND telemetry_outbox.written_at IS NULL
			   AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
			   AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
			 ORDER BY telemetry_outbox.id ASC
			 LIMIT 250
			 FOR UPDATE SKIP LOCKED
		),
		claimed AS (
			SELECT sized.id
			  FROM (
				SELECT candidates.id,
				       SUM(candidates.size_bytes) OVER (ORDER BY candidates.id ASC) AS cumulative_size_bytes
				  FROM candidates
			  ) AS sized
			 WHERE sized.cumulative_size_bytes <= 8 * 1024 * 1024
		),
		updated AS (
			UPDATE telemetry_outbox
			   SET state = 'claimed',
			       retry_count = telemetry_outbox.retry_count + 1,
			       next_retry_at = now() + interval '30 seconds',
			       updated_at = now()
			  FROM claimed
			 WHERE telemetry_outbox.id = claimed.id
			RETURNING telemetry_outbox.id
		)
		SELECT id FROM updated ORDER BY id ASC
	`)
	if err != nil {
		t.Fatal(err)
	}
	planLines = planLines[:0]
	for claimPlanRows.Next() {
		var line string
		if err := claimPlanRows.Scan(&line); err != nil {
			claimPlanRows.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := claimPlanRows.Err(); err != nil {
		claimPlanRows.Close()
		t.Fatal(err)
	}
	claimPlanRows.Close()
	claimPlan := strings.Join(planLines, "\n")
	if !strings.Contains(claimPlan, "telemetry_outbox_ingest_claim_idx") {
		t.Fatalf("run-log claim plan did not use telemetry_outbox_ingest_claim_idx:\n%s", claimPlan)
	}
	var claimedRows, claimedBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(size_bytes), 0)
		  FROM telemetry_outbox
		 WHERE state = 'claimed'
	`).Scan(&claimedRows, &claimedBytes); err != nil {
		t.Fatal(err)
	}
	if claimedRows != 42 || claimedBytes != 42*(192<<10) {
		t.Fatalf("scale claim = %d rows / %d bytes, want 42 / %d", claimedRows, claimedBytes, 42*(192<<10))
	}
	t.Logf("run-log claim explain:\n%s", claimPlan)

	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET stream_kind = 'event',
		       source_kind = 'run',
		       source_id = run_id,
		       kind = 'run.test',
		       message = '',
		       payload = '{}'::jsonb,
		       content = NULL,
		       size_bytes = NULL,
		       observed_seq = NULL,
		       state = 'pending',
		       next_retry_at = NULL
		 WHERE written_at IS NULL
	`)
	dbtest.MustExec(t, ctx, pool, `ANALYZE telemetry_outbox`)
	eventPlanRows, err := pool.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
		WITH candidates AS (
			SELECT telemetry_outbox.id,
			       octet_length(telemetry_outbox.message)::bigint
			           + octet_length(telemetry_outbox.payload::text)::bigint AS size_bytes
			  FROM telemetry_outbox
			 WHERE telemetry_outbox.stream_kind = 'event'
			   AND telemetry_outbox.written_at IS NULL
			   AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
			   AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
			 ORDER BY telemetry_outbox.id ASC
			 LIMIT 250
			 FOR UPDATE SKIP LOCKED
		),
		claimed AS (
			SELECT sized.id
			  FROM (
				SELECT candidates.id,
				       SUM(candidates.size_bytes) OVER (ORDER BY candidates.id ASC) AS cumulative_size_bytes
				  FROM candidates
			  ) AS sized
			 WHERE sized.cumulative_size_bytes <= 8 * 1024 * 1024
		),
		updated AS (
			UPDATE telemetry_outbox
			   SET state = 'claimed',
			       retry_count = telemetry_outbox.retry_count + 1,
			       next_retry_at = now() + interval '30 seconds',
			       updated_at = now()
			  FROM claimed
			 WHERE telemetry_outbox.id = claimed.id
			RETURNING telemetry_outbox.id
		)
		SELECT id FROM updated ORDER BY id ASC
	`)
	if err != nil {
		t.Fatal(err)
	}
	planLines = planLines[:0]
	for eventPlanRows.Next() {
		var line string
		if err := eventPlanRows.Scan(&line); err != nil {
			eventPlanRows.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := eventPlanRows.Err(); err != nil {
		eventPlanRows.Close()
		t.Fatal(err)
	}
	eventPlanRows.Close()
	eventPlan := strings.Join(planLines, "\n")
	if !strings.Contains(eventPlan, "telemetry_outbox_ingest_claim_idx") {
		t.Fatalf("event claim plan did not use telemetry_outbox_ingest_claim_idx:\n%s", eventPlan)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM telemetry_outbox
		 WHERE stream_kind = 'event' AND state = 'claimed'
	`).Scan(&claimedRows); err != nil {
		t.Fatal(err)
	}
	if claimedRows != 250 {
		t.Fatalf("small event scale claim = %d rows, want row ceiling 250", claimedRows)
	}
	t.Logf("event claim explain:\n%s", eventPlan)
}

func TestRunLogClaimUsesLongestByteBoundedPrefix(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	runID := uuid.NewV7()
	const (
		maxContentBytes = int64(192 << 10)
		maxBatchBytes   = int64(8 << 20)
	)

	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, stream_name, content, size_bytes, observed_seq, source, kind, message
		)
		SELECT $1, 'run_log', 'run', $2, $3, $4,
		       $2, 'stdout', '\x00',
		       CASE WHEN generated <= 43 THEN $5 ELSE 1 END,
		       generated, 'worker', 'run.log', 'run.log'
		  FROM generate_series(1, 44) AS generated
	`, orgID, runID, projectID, environmentID, maxContentBytes)

	var orderedIDs []int64
	rows, err := pool.Query(ctx, `
		SELECT id FROM telemetry_outbox WHERE run_id = $1 ORDER BY id ASC
	`, runID)
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

	claimed, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit: 250, MaxBatchBytes: maxBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 42 {
		t.Fatalf("claimed rows = %d, want 42 maximum-size rows", len(claimed))
	}
	for idx, row := range claimed {
		if row.OutboxID != orderedIDs[idx] {
			t.Fatalf("claimed row %d id = %d, want source-order id %d", idx, row.OutboxID, orderedIDs[idx])
		}
	}

	claimed, err = queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit: 250, MaxBatchBytes: maxBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].OutboxID != orderedIDs[42] || claimed[1].OutboxID != orderedIDs[43] {
		t.Fatalf("second claim = %+v, want remaining source-order rows", claimed)
	}

	smallRunID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, stream_name, content, size_bytes, observed_seq, source, kind, message
		)
		SELECT $1, 'run_log', 'run', $2, $3, $4,
		       $2, 'stdout', '\x00', 1, generated, 'worker', 'run.log', 'run.log'
		  FROM generate_series(1, 300) AS generated
	`, orgID, smallRunID, projectID, environmentID)
	claimed, err = queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit: 250, MaxBatchBytes: maxBatchBytes, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 250 {
		t.Fatalf("small run-log claim rows = %d, want row ceiling 250", len(claimed))
	}
}

func TestEventClaimUsesLongestByteBoundedPrefix(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	runID := uuid.NewV7()
	const maxBatchBytes = int64(8 << 20)
	prefix := `{"data":"`
	suffix := `"}`
	payload := prefix + strings.Repeat("x", (64<<10)-len(prefix)-len(suffix)) + suffix

	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, kind, message, payload
		)
		SELECT $1, 'event', 'run', $2, $3, $4,
		       $2, 'run.test', repeat('m', 4 * 1024), $5::jsonb
		  FROM generate_series(1, 121)
	`, orgID, runID, projectID, environmentID, payload)

	var orderedIDs []int64
	rows, err := pool.Query(ctx, `
		SELECT id FROM telemetry_outbox WHERE run_id = $1 ORDER BY id ASC
	`, runID)
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

	smallRunID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			run_id, kind, message, payload
		)
		SELECT $1, 'event', 'run', $2, $3, $4,
		       $2, 'run.test', '', '{}'::jsonb
		  FROM generate_series(1, 300)
	`, orgID, smallRunID, projectID, environmentID)
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
	lifecycle, err := queries.GetTelemetryOutboxLifecycle(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !lifecycle.OldestGcWrittenAt.Valid {
		t.Fatal("oldest GC-eligible age is missing")
	}

	pruned, err := queries.PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{
		RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want one 25h run log %d", pruned, runLogStaleID)
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
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox SET stream_name = 'alternate' WHERE id = $1
	`, secondEventID)
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
		MaxBatchBytes: 1,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(runLogClaimed) != 1 || runLogClaimed[0].OutboxID != runLogID {
		t.Fatalf("run log ingest claim = %+v err=%v", runLogClaimed, err)
	}
	runLogHeld, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      1,
		MaxBatchBytes: 1,
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
		MaxBatchBytes: 1,
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
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				org_id, stream_kind, source_kind, source_id, project_id,
				environment_id, deployment_id, kind, message
			) VALUES ($1, 'event', 'deployment', $2, $3, $4, $2, 'deployment.ready', $5)
			RETURNING id
		`, ids.orgID, sourceID, ids.projectID, ids.environmentID, label).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insertRunLog := func(label string) int64 {
		t.Helper()
		runID := uuid.NewV7()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				org_id, stream_kind, source_kind, source_id, project_id,
				environment_id, run_id, stream_name, content, size_bytes,
				observed_seq, source, kind, message
			) VALUES (
				$1, 'run_log', 'run', $2, $3, $4, $2, 'stdout', '\x00', 1,
				1, 'worker', 'run.log', $5
			)
			RETURNING id
		`, ids.orgID, runID, ids.projectID, ids.environmentID, label).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	eventSuccessID := insertEvent("event-success")
	eventFailureID := insertEvent("event-failure")
	runLogSuccessID := insertRunLog("run-log-success")
	runLogFailureID := insertRunLog("run-log-failure")
	eventClaims, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 2, MaxBatchBytes: 1 << 20, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(eventClaims) != 2 {
		t.Fatalf("event claims = %+v err=%v", eventClaims, err)
	}
	runLogClaims, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit: 2, MaxBatchBytes: 2, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(runLogClaims) != 2 {
		t.Fatalf("run-log claims = %+v err=%v", runLogClaims, err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE telemetry_outbox
		   SET next_retry_at = now() - interval '1 second'
		 WHERE id = ANY($1::bigint[])
	`, []int64{eventSuccessID, eventFailureID, runLogSuccessID, runLogFailureID})
	eventReclaims, err := queries.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
		RowLimit: 2, MaxBatchBytes: 1 << 20, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(eventReclaims) != 2 {
		t.Fatalf("event reclaims = %+v err=%v", eventReclaims, err)
	}
	runLogReclaims, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit: 2, MaxBatchBytes: 2, LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil || len(runLogReclaims) != 2 {
		t.Fatalf("run-log reclaims = %+v err=%v", runLogReclaims, err)
	}

	type ingestState struct {
		state       string
		retryCount  int32
		nextRetryAt string
		ingestError string
		written     bool
	}
	readState := func(id int64) ingestState {
		t.Helper()
		var value ingestState
		if err := pool.QueryRow(ctx, `
			SELECT state, retry_count, COALESCE(next_retry_at::text, ''), ingest_error,
			       written_at IS NOT NULL
			  FROM telemetry_outbox WHERE id = $1
		`, id).Scan(&value.state, &value.retryCount, &value.nextRetryAt, &value.ingestError, &value.written); err != nil {
			t.Fatal(err)
		}
		return value
	}
	eventSuccessReclaimed := readState(eventSuccessID)
	eventFailureReclaimed := readState(eventFailureID)
	runLogSuccessReclaimed := readState(runLogSuccessID)
	runLogFailureReclaimed := readState(runLogFailureID)

	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids: []int64{eventSuccessID, runLogSuccessID},
		ExpectedRetryCounts: []int32{
			eventClaims[0].RetryCount,
			runLogClaims[0].RetryCount,
		},
	}); err != nil || updated != 0 {
		t.Fatalf("stale success updated = %d err=%v, want 0", updated, err)
	}
	if got := readState(eventSuccessID); got != eventSuccessReclaimed {
		t.Fatalf("stale success changed reclaimed row: got %+v want %+v", got, eventSuccessReclaimed)
	}
	if got := readState(runLogSuccessID); got != runLogSuccessReclaimed {
		t.Fatalf("stale run-log success changed reclaimed row: got %+v want %+v", got, runLogSuccessReclaimed)
	}
	if updated, err := queries.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids: []int64{eventFailureID, runLogFailureID},
		ExpectedRetryCounts: []int32{
			eventClaims[1].RetryCount,
			runLogClaims[1].RetryCount,
		},
		RetryAfter: pgvalue.Interval(time.Minute), IngestError: "stale failure",
	}); err != nil || updated != 0 {
		t.Fatalf("stale failure updated = %d err=%v, want 0", updated, err)
	}
	if got := readState(eventFailureID); got != eventFailureReclaimed {
		t.Fatalf("stale failure changed reclaimed row: got %+v want %+v", got, eventFailureReclaimed)
	}
	if got := readState(runLogFailureID); got != runLogFailureReclaimed {
		t.Fatalf("stale run-log failure changed reclaimed row: got %+v want %+v", got, runLogFailureReclaimed)
	}

	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids:                 []int64{eventSuccessID, runLogSuccessID},
		ExpectedRetryCounts: []int32{eventReclaims[0].RetryCount, runLogClaims[0].RetryCount},
	}); err != nil || updated != 1 {
		t.Fatalf("mixed current/stale success updated = %d err=%v, want 1", updated, err)
	}
	if updated, err := queries.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids:                 []int64{eventFailureID, runLogFailureID},
		ExpectedRetryCounts: []int32{eventReclaims[1].RetryCount, runLogClaims[1].RetryCount},
		RetryAfter:          pgvalue.Interval(time.Minute),
		IngestError:         "new event owner failure",
	}); err != nil || updated != 1 {
		t.Fatalf("mixed current/stale failure updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(runLogSuccessID); got != runLogSuccessReclaimed {
		t.Fatalf("mixed success changed stale run-log row: got %+v want %+v", got, runLogSuccessReclaimed)
	}
	if got := readState(runLogFailureID); got != runLogFailureReclaimed {
		t.Fatalf("mixed failure changed stale run-log row: got %+v want %+v", got, runLogFailureReclaimed)
	}
	if updated, err := queries.MarkTelemetryOutboxWritten(ctx, db.MarkTelemetryOutboxWrittenParams{
		Ids: []int64{runLogSuccessID}, ExpectedRetryCounts: []int32{runLogReclaims[0].RetryCount},
	}); err != nil || updated != 1 {
		t.Fatalf("current run-log success updated = %d err=%v, want 1", updated, err)
	}
	if updated, err := queries.MarkTelemetryOutboxBatchFailed(ctx, db.MarkTelemetryOutboxBatchFailedParams{
		Ids: []int64{runLogFailureID}, ExpectedRetryCounts: []int32{runLogReclaims[1].RetryCount},
		RetryAfter: pgvalue.Interval(time.Minute), IngestError: "new run-log owner failure",
	}); err != nil || updated != 1 {
		t.Fatalf("current run-log failure updated = %d err=%v, want 1", updated, err)
	}
	if got := readState(eventSuccessID); got.state != "written" || got.retryCount != 0 || !got.written {
		t.Fatalf("current event success state = %+v", got)
	}
	if got := readState(eventFailureID); got.state != "failed" || got.retryCount != eventReclaims[1].RetryCount || got.ingestError != "new event owner failure" {
		t.Fatalf("current event failure state = %+v", got)
	}
	if got := readState(runLogSuccessID); got.state != "written" || got.retryCount != 0 || !got.written {
		t.Fatalf("current run-log success state = %+v", got)
	}
	if got := readState(runLogFailureID); got.state != "failed" || got.retryCount != runLogReclaims[1].RetryCount || got.ingestError != "new run-log owner failure" {
		t.Fatalf("current run-log failure state = %+v", got)
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
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				org_id, stream_kind, source_kind, source_id, project_id,
				environment_id, deployment_id, source, kind, message
			) VALUES ($1, 'event', 'deployment', $2, $3, $4, $2, 'control', 'deployment.ready', $5)
			RETURNING id
		`, ids.orgID, sourceID, ids.projectID, ids.environmentID, fmt.Sprintf("partial-%d", index)).Scan(&outboxIDs[index]); err != nil {
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
		if err := pool.QueryRow(ctx, `
			INSERT INTO telemetry_outbox (
				org_id, stream_kind, source_kind, source_id, project_id,
				environment_id, deployment_id, source, kind, message
			) VALUES ($1, 'event', 'deployment', $2, $3, $4, $2, 'control', 'deployment.ready', $5)
			RETURNING id
		`, ids.orgID, sourceID, ids.projectID, ids.environmentID, fmt.Sprintf("event-%d", index)).Scan(&outboxIDs[index]); err != nil {
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
