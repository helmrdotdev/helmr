package db_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const testWorkerGroupID = "us-east-1-worker-group-1"

func TestAppendRunLogChunkWritesSelfContainedOutboxAndUsage(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	first := appendRunLog(t, ctx, queries, ids, runLeaseID, workerID, db.RunLogStreamStdout, 1, []byte("alpha"))
	duplicate := appendRunLog(t, ctx, queries, ids, runLeaseID, workerID, db.RunLogStreamStdout, 1, []byte("different"))
	if duplicate.Seq != first.Seq || pgvalue.Int8Value(duplicate.SizeBytes) != pgvalue.Int8Value(first.SizeBytes) || !bytes.Equal(duplicate.Content, first.Content) {
		t.Fatalf("duplicate chunk = %+v, want existing first chunk %+v", duplicate, first)
	}
	second := appendRunLog(t, ctx, queries, ids, runLeaseID, workerID, db.RunLogStreamStderr, 2, []byte("beta"))
	third := appendRunLog(t, ctx, queries, ids, runLeaseID, workerID, db.RunLogStreamStdout, 3, []byte("gamma"))
	if !(first.Seq < second.Seq && second.Seq < third.Seq) {
		t.Fatalf("run log seqs = %d,%d,%d, want run-wide monotonic seq", first.Seq, second.Seq, third.Seq)
	}

	var outboxCount int64
	var meterOutboxCount int64
	var usageCount int64
	var usageBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM telemetry_outbox WHERE org_id = $1 AND worker_group_id = $2 AND stream_kind = 'run_log' AND source_kind = 'run' AND source_id = $3),
			(SELECT count(*) FROM telemetry_outbox WHERE org_id = $1 AND worker_group_id = $2 AND stream_kind = 'meter_event' AND run_id = $3 AND kind = 'log_bytes'),
			(SELECT count(*) FROM meter_events WHERE org_id = $1 AND run_id = $3 AND meter = 'log_bytes'),
			(SELECT COALESCE(SUM(quantity), 0) FROM meter_events WHERE org_id = $1 AND run_id = $3 AND meter = 'log_bytes')
	`, ids.orgID, testWorkerGroupID, ids.runID).Scan(&outboxCount, &meterOutboxCount, &usageCount, &usageBytes); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 3 || meterOutboxCount != 3 || usageCount != 3 || usageBytes != int64(len("alphabetagamma")) {
		t.Fatalf("outbox=%d meterOutbox=%d usageCount=%d usageBytes=%d", outboxCount, meterOutboxCount, usageCount, usageBytes)
	}

	claimed, err := queries.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claimed run log rows = %d, want 3", len(claimed))
	}
	var joined []byte
	for _, row := range claimed {
		if row.ProjectID != pgvalue.UUID(ids.projectID) || row.EnvironmentID != pgvalue.UUID(ids.environmentID) || row.RunLeaseID != pgvalue.UUID(runLeaseID) {
			t.Fatalf("claimed row missing placement payload: %+v", row)
		}
		joined = append(joined, row.Content...)
	}
	if !bytes.Equal(joined, []byte("alphabetagamma")) {
		t.Fatalf("claimed bytes = %q", joined)
	}

	claimedMeterEvents, err := queries.ClaimMeterEventIngestBatch(ctx, db.ClaimMeterEventIngestBatchParams{
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimedMeterEvents) != 3 {
		t.Fatalf("claimed meter event rows = %d, want 3", len(claimedMeterEvents))
	}
	var claimedBytes int64
	for _, row := range claimedMeterEvents {
		if row.WorkerGroupID != testWorkerGroupID || row.ProjectID != pgvalue.UUID(ids.projectID) || row.EnvironmentID != pgvalue.UUID(ids.environmentID) {
			t.Fatalf("claimed meter event missing placement payload: %+v", row)
		}
		if row.SourceType != "run_log" || row.RunID != pgvalue.UUID(ids.runID) || row.Meter != "log_bytes" || row.Unit != "bytes" {
			t.Fatalf("claimed meter event identity = %+v", row)
		}
		if string(row.Details) == "" || !bytes.Contains(row.Details, []byte(`"stream"`)) {
			t.Fatalf("claimed meter event details = %s", row.Details)
		}
		if !row.Quantity.Valid || row.Quantity.Int == nil || row.Quantity.Exp != 0 {
			t.Fatalf("claimed meter event quantity = %+v, want integer numeric", row.Quantity)
		}
		claimedBytes += row.Quantity.Int.Int64()
	}
	if claimedBytes != int64(len("alphabetagamma")) {
		t.Fatalf("claimed meter event bytes = %d", claimedBytes)
	}
}

func TestMeterEventsSurviveRunDeletion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	appendRunLog(t, ctx, queries, ids, runLeaseID, workerID, db.RunLogStreamStdout, 1, []byte("billable"))
	if _, err := pool.Exec(ctx, `
		DELETE FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	var usageCount int64
	var usageBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(quantity), 0)
		  FROM meter_events
		 WHERE org_id = $1
		   AND run_id = $2
		   AND meter = 'log_bytes'
	`, ids.orgID, ids.runID).Scan(&usageCount, &usageBytes); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1 || usageBytes != int64(len("billable")) {
		t.Fatalf("meter events after run deletion count=%d bytes=%d", usageCount, usageBytes)
	}
}

func TestMeterEventIdempotencyRejectsDuplicateScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	sourceID := uuid.Must(uuid.NewV7())

	insert := `
		INSERT INTO meter_events (
			org_id,
			worker_group_id,
			project_id,
			environment_id,
			source_type,
			source_id,
			run_id,
			meter,
			quantity,
			unit,
			idempotency_key
		)
		VALUES ($1, $2, $3, $4, 'run_log', $5, $6, 'log_bytes', 7, 'bytes', 'duplicate-key')
	`
	if _, err := pool.Exec(ctx, insert, ids.orgID, testWorkerGroupID, ids.projectID, ids.environmentID, sourceID, ids.runID); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, insert, ids.orgID, testWorkerGroupID, ids.projectID, ids.environmentID, sourceID, ids.runID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate meter event error = %v, want unique_violation", err)
	}
}

func TestWorkerTelemetryAppendRejectsDisabledWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)

	_, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    testWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("wrong-worker-group"),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AppendRunLogChunk disabled worker group error = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.AppendRunEventForExecution(ctx, db.AppendRunEventForExecutionParams{
		Kind:             "run.event",
		Payload:          []byte(`{"ok":true}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    testWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AppendRunEventForExecution disabled worker group error = %v, want pgx.ErrNoRows", err)
	}

	var outboxCount, usageCount int64
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM telemetry_outbox WHERE org_id = $1 AND source_id = $2),
			(SELECT count(*) FROM meter_events WHERE org_id = $1 AND run_id = $2)
	`, ids.orgID, ids.runID).Scan(&outboxCount, &usageCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 0 || usageCount != 0 {
		t.Fatalf("disabled worker group append mutated outbox=%d usage=%d", outboxCount, usageCount)
	}
}

func TestWorkerTelemetryAppendAllowsStaleWorkerGroupHealthForInFlightLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE id = $1
	`, testWorkerGroupID); err != nil {
		t.Fatal(err)
	}

	chunk := appendRunLog(t, ctx, queries, ids, runLeaseID, workerID, db.RunLogStreamStdout, 1, []byte("still-running"))
	if chunk.Seq != 1 {
		t.Fatalf("chunk seq = %d, want 1", chunk.Seq)
	}
}

func TestAppendRunLogChunkConcurrentDuplicateDoesNotBurnSeq(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan db.AppendRunLogChunkRow, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			<-start
			row, err := db.New(pool).AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
				Kind:             "run.log",
				Payload:          []byte(`{"stream":"stdout"}`),
				OrgID:            pgvalue.UUID(ids.orgID),
				WorkerGroupID:    testWorkerGroupID,
				RunID:            pgvalue.UUID(ids.runID),
				RunLeaseID:       pgvalue.UUID(runLeaseID),
				WorkerInstanceID: pgvalue.UUID(workerID),
				Stream:           db.RunLogStreamStdout,
				ObservedSeq:      1,
				Content:          []byte("alpha"),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- row
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("append duplicate: %v", err)
	}
	for row := range results {
		if row.Seq != 1 || pgvalue.Int8Value(row.SizeBytes) != int64(len("alpha")) || !bytes.Equal(row.Content, []byte("alpha")) {
			t.Fatalf("duplicate row = %+v", row)
		}
	}

	var outboxCount, usageCount int64
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM telemetry_outbox WHERE org_id = $1 AND worker_group_id = $2 AND source_kind = 'run' AND source_id = $3 AND stream_kind = 'run_log'),
			(SELECT count(*) FROM meter_events WHERE org_id = $1 AND run_id = $3 AND meter = 'log_bytes')
	`, ids.orgID, testWorkerGroupID, ids.runID).Scan(&outboxCount, &usageCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 || usageCount != 1 {
		t.Fatalf("outbox=%d usage=%d, want 1 each", outboxCount, usageCount)
	}
}

func TestTerminalOutputWritesSelfContainedOutboxAndLiveChunksCanBeTrimmed(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	execID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_processes (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id,
			kind, command, state, detached, created_by_subject_type, created_by_subject_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'command', '["true"]'::jsonb, 'running', true, 'test', 'test')
	`, execID, ids.orgID, testWorkerGroupID, ids.projectID, ids.environmentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertWorkspaceExecOutputStreamChunk(ctx, db.InsertWorkspaceExecOutputStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: testWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ProcessID:     pgvalue.UUID(execID),
		StreamName:    "stdout",
		OffsetStart:   0,
		OffsetEnd:     5,
		Data:          []byte("alpha"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertWorkspaceExecOutputStreamChunk(ctx, db.InsertWorkspaceExecOutputStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: testWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ProcessID:     pgvalue.UUID(execID),
		StreamName:    "stdout",
		OffsetStart:   5,
		OffsetEnd:     9,
		Data:          []byte("beta"),
	}); err != nil {
		t.Fatal(err)
	}

	claimed, err := queries.ClaimWorkspaceProcessTerminalOutputIngestBatch(ctx, db.ClaimWorkspaceProcessTerminalOutputIngestBatchParams{
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || string(claimed[0].Data)+string(claimed[1].Data) != "alphabeta" {
		t.Fatalf("claimed exec terminal rows = %+v", claimed)
	}
	for _, row := range claimed {
		if row.ProjectID != pgvalue.UUID(ids.projectID) || row.EnvironmentID != pgvalue.UUID(ids.environmentID) || row.ResourceKind != "workspace_process" || row.ResourceID != pgvalue.UUID(execID) {
			t.Fatalf("terminal outbox row missing payload: %+v", row)
		}
	}

	if err := queries.DeleteWorkspaceExecStreamChunksBefore(ctx, db.DeleteWorkspaceExecStreamChunksBeforeParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ProcessID:         pgvalue.UUID(execID),
		StreamName:        "stdout",
		RetainAfterOffset: 5,
	}); err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_process_stream_chunks
		 WHERE org_id = $1
		   AND process_id = $2
	`, ids.orgID, execID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining exec stream chunks = %d, want 1", remaining)
	}
}

func TestDeadLetteredUnpublishedEventDoesNotBlockLaterPublish(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := queries.AppendRunEvent(ctx, db.AppendRunEventParams{
		Kind:    "run.first",
		Payload: []byte(`{"ok":true}`),
		OrgID:   pgvalue.UUID(ids.orgID),
		RunID:   pgvalue.UUID(ids.runID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendRunEvent(ctx, db.AppendRunEventParams{
		Kind:    "run.second",
		Payload: []byte(`{"ok":true}`),
		OrgID:   pgvalue.UUID(ids.orgID),
		RunID:   pgvalue.UUID(ids.runID),
	}); err != nil {
		t.Fatal(err)
	}
	var firstSeq, secondSeq int64
	if err := pool.QueryRow(ctx, `
		SELECT min(id), max(id)
		  FROM telemetry_outbox
		 WHERE org_id = $1
		   AND stream_kind = 'event'
		   AND source_kind = 'run'
		   AND source_id = $2
	`, ids.orgID, ids.runID).Scan(&firstSeq, &secondSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET state = 'dead_lettered'
		 WHERE stream_kind = 'event'
		   AND source_kind = 'run'
		   AND source_id = $1
		   AND id = $2
	`, ids.runID, firstSeq); err != nil {
		t.Fatal(err)
	}

	claimed, err := queries.ClaimEventOutbox(ctx, db.ClaimEventOutboxParams{
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Seq != secondSeq {
		t.Fatalf("claimed event outbox = %+v, want seq %d only", claimed, secondSeq)
	}
}

func appendRunLog(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, runLeaseID uuid.UUID, workerID uuid.UUID, stream db.RunLogStream, observedSeq int64, content []byte) db.AppendRunLogChunkRow {
	t.Helper()
	row, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"` + string(stream) + `"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    testWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           stream,
		ObservedSeq:      observedSeq,
		Content:          content,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.AttemptNumber != (pgtype.Int4{Int32: 1, Valid: true}) {
		t.Fatalf("attempt number = %+v, want 1", row.AttemptNumber)
	}
	return row
}
