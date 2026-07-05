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
)

const testCellID = "us-east-1-cell-1"

func TestAppendRunLogChunkIdempotentUsageLedgerAndByteContinuity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	first, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("alpha"),
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("different"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Seq != first.Seq || duplicate.SizeBytes != first.SizeBytes || !bytes.Equal(duplicate.Content, first.Content) {
		t.Fatalf("duplicate chunk = %+v, want existing first chunk %+v", duplicate, first)
	}
	second, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stderr"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStderr,
		ObservedSeq:      2,
		Content:          []byte("beta"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != first.Seq+1 {
		t.Fatalf("second seq = %d, first seq = %d", second.Seq, first.Seq)
	}
	third, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      3,
		Content:          []byte("gamma"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Seq != second.Seq+1 {
		t.Fatalf("third seq = %d, second seq = %d", third.Seq, second.Seq)
	}

	rows, err := queries.ListRunLogChunksAfter(ctx, db.ListRunLogChunksAfterParams{
		OrgID:    pgvalue.UUID(ids.orgID),
		RunID:    pgvalue.UUID(ids.runID),
		Seq:      0,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, row := range rows {
		joined = append(joined, row.Content...)
	}
	if !bytes.Equal(joined, []byte("alphabetagamma")) {
		t.Fatalf("joined log bytes = %q", joined)
	}

	var usageCount int64
	var usageBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(quantity), 0)
		  FROM usage_ledger_entries
		 WHERE org_id = $1
		   AND run_id = $2
		   AND meter = 'log_bytes'
	`, ids.orgID, ids.runID).Scan(&usageCount, &usageBytes); err != nil {
		t.Fatal(err)
	}
	if usageCount != 3 || usageBytes != int64(len("alphabetagamma")) {
		t.Fatalf("usage ledger entries count=%d bytes=%d", usageCount, usageBytes)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET written_at = CASE WHEN seq IN ($2, $4) THEN now() ELSE NULL END
		 WHERE stream_kind = 'run_log'
		   AND source_kind = 'run'
		   AND source_id = $1
		   AND seq IN ($2, $3, $4)
	`, ids.runID, first.Seq, second.Seq, third.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRunLogWatermark(ctx, db.UpsertRunLogWatermarkParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		CellID:       testCellID,
		RunID:        pgvalue.UUID(ids.runID),
		StreamName:   string(db.RunLogStreamStdout),
		WatermarkSeq: third.Seq,
	}); err != nil {
		t.Fatal(err)
	}
	watermark, err := queries.GetRunLogWatermark(ctx, db.GetRunLogWatermarkParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		CellID: testCellID,
		RunID:  pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if watermark != first.Seq {
		t.Fatalf("run log read watermark = %d, want %d before unwritten chunk", watermark, first.Seq)
	}

	var eventCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM event_hot_payloads AS events
		 WHERE org_id = $1
		   AND run_id = $2
		   AND kind = 'run.log'
	`, ids.orgID, ids.runID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 3 {
		t.Fatalf("event count = %d, want 3", eventCount)
	}

	if _, err := queries.UpsertRunLogWatermark(ctx, db.UpsertRunLogWatermarkParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		CellID:       testCellID,
		RunID:        pgvalue.UUID(ids.runID),
		StreamName:   string(db.RunLogStreamStdout),
		WatermarkSeq: first.Seq,
	}); err != nil {
		t.Fatal(err)
	}
	prunedLogs, err := queries.PruneRunLogChunksPastWatermark(ctx, db.PruneRunLogChunksPastWatermarkParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     testCellID,
		RunID:      pgvalue.UUID(ids.runID),
		PruneGrace: pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedLogs) != 1 || prunedLogs[0] != first.Seq {
		t.Fatalf("pruned logs = %v, want [%d]", prunedLogs, first.Seq)
	}
	rows, err = queries.ListRunLogChunksAfter(ctx, db.ListRunLogChunksAfterParams{
		OrgID:    pgvalue.UUID(ids.orgID),
		RunID:    pgvalue.UUID(ids.runID),
		Seq:      0,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != second.Seq || rows[1].Seq != third.Seq ||
		!bytes.Equal(rows[0].Content, []byte("beta")) || !bytes.Equal(rows[1].Content, []byte("gamma")) {
		t.Fatalf("remaining log rows = %+v", rows)
	}

	if _, err := queries.UpsertEventWatermark(ctx, db.UpsertEventWatermarkParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		CellID:       testCellID,
		SubjectType:  db.EventSubjectTypeRun,
		SubjectID:    pgvalue.UUID(ids.runID),
		WatermarkSeq: first.Seq,
	}); err != nil {
		t.Fatal(err)
	}
	prunedEvents, err := queries.PruneEventsPastWatermark(ctx, db.PruneEventsPastWatermarkParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		CellID:      testCellID,
		SubjectType: db.EventSubjectTypeRun,
		SubjectID:   pgvalue.UUID(ids.runID),
		PruneGrace:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedEvents) != 0 {
		t.Fatalf("pruned unpublished events = %v, want none", prunedEvents)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET published_at = now(),
		       written_at = now()
			 WHERE stream_kind = 'event'
			   AND source_kind = 'run'
			   AND org_id = $1
			   AND source_id = $2
			   AND seq = $3
	`, ids.orgID, ids.runID, first.Seq); err != nil {
		t.Fatal(err)
	}
	prunedEvents, err = queries.PruneEventsPastWatermark(ctx, db.PruneEventsPastWatermarkParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		CellID:      testCellID,
		SubjectType: db.EventSubjectTypeRun,
		SubjectID:   pgvalue.UUID(ids.runID),
		PruneGrace:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedEvents) != 1 || prunedEvents[0] != first.Seq {
		t.Fatalf("pruned events = %v, want [%d]", prunedEvents, first.Seq)
	}
}

func TestUsageLedgerEntriesSurviveRunDeletion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("billable"),
	}); err != nil {
		t.Fatal(err)
	}
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
		  FROM usage_ledger_entries
		 WHERE org_id = $1
		   AND run_id = $2
		   AND meter = 'log_bytes'
	`, ids.orgID, ids.runID).Scan(&usageCount, &usageBytes); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1 || usageBytes != int64(len("billable")) {
		t.Fatalf("usage ledger entries after run deletion count=%d bytes=%d", usageCount, usageBytes)
	}
}

func TestUsageLedgerEntryIdempotencyRejectsDuplicateScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	sourceID := uuid.Must(uuid.NewV7())

	insert := `
		INSERT INTO usage_ledger_entries (
			org_id,
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
		VALUES ($1, $2, $3, 'run_log', $4, $5, 'log_bytes', 7, 'bytes', 'duplicate-key')
	`
	if _, err := pool.Exec(ctx, insert, ids.orgID, ids.projectID, ids.environmentID, sourceID, ids.runID); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, insert, ids.orgID, ids.projectID, ids.environmentID, sourceID, ids.runID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate usage ledger entry error = %v, want unique_violation", err)
	}
}

func TestWorkerTelemetryAppendRejectsDisabledSourceRoute(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	disableDefaultEnvironmentRoute(t, ctx, pool, ids)

	_, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("wrong-cell"),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AppendRunLogChunk disabled route error = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.AppendRunEventForExecution(ctx, db.AppendRunEventForExecutionParams{
		Kind:             "run.event",
		Payload:          []byte(`{"ok":true}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AppendRunEventForExecution route mismatch error = %v, want pgx.ErrNoRows", err)
	}

	var chunkCount, eventCount, outboxCount, usageCount int64
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM run_log_hot_chunks WHERE org_id = $1 AND run_id = $2),
			(SELECT count(*) FROM event_hot_payloads WHERE org_id = $1 AND run_id = $2),
			(SELECT count(*) FROM telemetry_outbox WHERE org_id = $1 AND source_id = $2),
			(SELECT count(*) FROM usage_ledger_entries WHERE org_id = $1 AND run_id = $2)
	`, ids.orgID, ids.runID).Scan(&chunkCount, &eventCount, &outboxCount, &usageCount); err != nil {
		t.Fatal(err)
	}
	if chunkCount != 0 || eventCount != 0 || outboxCount != 0 || usageCount != 0 {
		t.Fatalf("wrong-cell append mutated chunks=%d events=%d outbox=%d usage=%d", chunkCount, eventCount, outboxCount, usageCount)
	}
}

func TestWorkerTelemetryAppendAllowsStaleCellHealthForInFlightLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE cell_health
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE cell_id = $1
	`, testCellID); err != nil {
		t.Fatal(err)
	}

	chunk, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("still-running"),
	})
	if err != nil {
		t.Fatal(err)
	}
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
				CellID:           testCellID,
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
		if row.Seq != 1 || row.SizeBytes != int64(len("alpha")) || !bytes.Equal(row.Content, []byte("alpha")) {
			t.Fatalf("duplicate row = %+v", row)
		}
	}

	var headSeq, chunkCount, outboxCount, usageCount int64
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT seq FROM run_log_cursors WHERE org_id = $1 AND cell_id = $2 AND run_id = $3 AND stream_name = '__run__'),
			(SELECT count(*) FROM run_log_hot_chunks WHERE org_id = $1 AND cell_id = $2 AND run_id = $3),
			(SELECT count(*) FROM telemetry_outbox WHERE org_id = $1 AND cell_id = $2 AND source_kind = 'run' AND source_id = $3 AND stream_kind = 'run_log'),
			(SELECT count(*) FROM usage_ledger_entries WHERE org_id = $1 AND run_id = $3 AND meter = 'log_bytes')
	`, ids.orgID, testCellID, ids.runID).Scan(&headSeq, &chunkCount, &outboxCount, &usageCount); err != nil {
		t.Fatal(err)
	}
	if headSeq != 1 || chunkCount != 1 || outboxCount != 1 || usageCount != 1 {
		t.Fatalf("headSeq=%d chunks=%d outbox=%d usage=%d, want headSeq=1 and all counts 1", headSeq, chunkCount, outboxCount, usageCount)
	}
}

func TestTerminalOutputHotBuffersPruneOnlyPastWatermark(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	execID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_execs (
			id, org_id, cell_id, project_id, environment_id, workspace_id,
			command, state, detached, created_by_subject_type, created_by_subject_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, '["true"]'::jsonb, 'running', true, 'test', 'test')
	`, execID, ids.orgID, testCellID, ids.projectID, ids.environmentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertWorkspaceExecOutputStreamChunk(ctx, db.InsertWorkspaceExecOutputStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        testCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ExecID:        pgvalue.UUID(execID),
		Stream:        db.WorkspaceExecStreamStdout,
		OffsetStart:   0,
		OffsetEnd:     5,
		Data:          []byte("alpha"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertWorkspaceExecOutputStreamChunk(ctx, db.InsertWorkspaceExecOutputStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        testCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ExecID:        pgvalue.UUID(execID),
		Stream:        db.WorkspaceExecStreamStdout,
		OffsetStart:   5,
		OffsetEnd:     9,
		Data:          []byte("beta"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET written_at = CASE WHEN seq = 0 THEN now() ELSE NULL END
		 WHERE stream_kind = 'terminal_output'
		   AND source_kind = 'workspace_exec'
		   AND source_id = $1
	`, execID); err != nil {
		t.Fatal(err)
	}
	execFrontier, err := queries.GetTerminalOutputIngestFrontier(ctx, db.GetTerminalOutputIngestFrontierParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		SourceKind:       "workspace_exec",
		SourceID:         pgvalue.UUID(execID),
		StreamName:       "stdout",
		MaxWrittenOffset: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if execFrontier != 5 {
		t.Fatalf("exec terminal frontier = %d, want 5", execFrontier)
	}
	if err := queries.DeleteWorkspaceExecStreamChunksBefore(ctx, db.DeleteWorkspaceExecStreamChunksBeforeParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ExecID:            pgvalue.UUID(execID),
		Stream:            db.WorkspaceExecStreamStdout,
		RetainAfterOffset: 9,
	}); err != nil {
		t.Fatal(err)
	}
	var execHotRows int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_exec_stream_chunks
		 WHERE org_id = $1
		   AND cell_id = $2
		   AND exec_id = $3
	`, ids.orgID, testCellID, execID).Scan(&execHotRows); err != nil {
		t.Fatal(err)
	}
	if execHotRows != 2 {
		t.Fatalf("exec hot rows after pre-watermark retention = %d, want 2", execHotRows)
	}
	if _, err := queries.UpsertTerminalOutputWatermark(ctx, db.UpsertTerminalOutputWatermarkParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          testCellID,
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		ResourceKind:    "workspace_exec",
		ResourceID:      pgvalue.UUID(execID),
		StreamName:      "stdout",
		WatermarkOffset: 5,
	}); err != nil {
		t.Fatal(err)
	}
	prunedExec, err := queries.PruneWorkspaceExecStreamChunksPastWatermark(ctx, db.PruneWorkspaceExecStreamChunksPastWatermarkParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		CellID:      testCellID,
		WorkspaceID: pgvalue.UUID(ids.workspaceID),
		ExecID:      pgvalue.UUID(execID),
		PruneGrace:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedExec) != 1 || prunedExec[0] != 5 {
		t.Fatalf("pruned exec offsets = %v, want [5]", prunedExec)
	}
	remainingExec, err := queries.ListWorkspaceExecStreamChunksAfterWatermark(ctx, db.ListWorkspaceExecStreamChunksAfterWatermarkParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          testCellID,
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		ExecID:          pgvalue.UUID(execID),
		Stream:          db.WorkspaceExecStreamStdout,
		WatermarkOffset: 5,
		CursorOffset:    0,
		LimitCount:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingExec) != 1 || remainingExec[0].OffsetStart != 5 || remainingExec[0].OffsetEnd != 9 {
		t.Fatalf("remaining exec chunks above watermark = %+v", remainingExec)
	}

	ptyID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
			INSERT INTO workspace_pty_sessions (
				id, org_id, cell_id, project_id, environment_id, workspace_id,
				cols, rows, state, created_by_subject_type, created_by_subject_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, 80, 24, 'open', 'test', 'test')
	`, ptyID, ids.orgID, testCellID, ids.projectID, ids.environmentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertWorkspacePtyOutputStreamChunk(ctx, db.InsertWorkspacePtyOutputStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        testCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		PtySessionID:  pgvalue.UUID(ptyID),
		Stream:        db.WorkspacePtyStreamOutput,
		OffsetStart:   0,
		OffsetEnd:     4,
		Data:          []byte("echo"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertWorkspacePtyOutputStreamChunk(ctx, db.InsertWorkspacePtyOutputStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        testCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		PtySessionID:  pgvalue.UUID(ptyID),
		Stream:        db.WorkspacePtyStreamOutput,
		OffsetStart:   4,
		OffsetEnd:     10,
		Data:          []byte("prompt"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET written_at = CASE WHEN seq = 0 THEN now() ELSE NULL END
		 WHERE stream_kind = 'terminal_output'
		   AND source_kind = 'workspace_pty'
		   AND source_id = $1
	`, ptyID); err != nil {
		t.Fatal(err)
	}
	ptyFrontier, err := queries.GetTerminalOutputIngestFrontier(ctx, db.GetTerminalOutputIngestFrontierParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		SourceKind:       "workspace_pty",
		SourceID:         pgvalue.UUID(ptyID),
		StreamName:       "output",
		MaxWrittenOffset: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ptyFrontier != 4 {
		t.Fatalf("pty terminal frontier = %d, want 4", ptyFrontier)
	}
	if err := queries.DeleteWorkspacePtyStreamChunksBefore(ctx, db.DeleteWorkspacePtyStreamChunksBeforeParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		PtySessionID:      pgvalue.UUID(ptyID),
		Stream:            db.WorkspacePtyStreamOutput,
		RetainAfterOffset: 10,
	}); err != nil {
		t.Fatal(err)
	}
	var ptyHotRows int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_pty_stream_chunks
		 WHERE org_id = $1
		   AND cell_id = $2
		   AND pty_session_id = $3
	`, ids.orgID, testCellID, ptyID).Scan(&ptyHotRows); err != nil {
		t.Fatal(err)
	}
	if ptyHotRows != 2 {
		t.Fatalf("pty hot rows after pre-watermark retention = %d, want 2", ptyHotRows)
	}
	if _, err := queries.UpsertTerminalOutputWatermark(ctx, db.UpsertTerminalOutputWatermarkParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          testCellID,
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		ResourceKind:    "workspace_pty",
		ResourceID:      pgvalue.UUID(ptyID),
		StreamName:      "output",
		WatermarkOffset: 4,
	}); err != nil {
		t.Fatal(err)
	}
	prunedPty, err := queries.PruneWorkspacePtyStreamChunksPastWatermark(ctx, db.PruneWorkspacePtyStreamChunksPastWatermarkParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		CellID:       testCellID,
		WorkspaceID:  pgvalue.UUID(ids.workspaceID),
		PtySessionID: pgvalue.UUID(ptyID),
		PruneGrace:   pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedPty) != 1 || prunedPty[0] != 4 {
		t.Fatalf("pruned pty offsets = %v, want [4]", prunedPty)
	}
	remainingPty, err := queries.ListWorkspacePtyStreamChunksAfterWatermark(ctx, db.ListWorkspacePtyStreamChunksAfterWatermarkParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          testCellID,
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		PtySessionID:    pgvalue.UUID(ptyID),
		Stream:          db.WorkspacePtyStreamOutput,
		WatermarkOffset: 4,
		CursorOffset:    0,
		LimitCount:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingPty) != 1 || remainingPty[0].OffsetStart != 4 || remainingPty[0].OffsetEnd != 10 {
		t.Fatalf("remaining pty chunks above watermark = %+v", remainingPty)
	}
}

func TestDeadLetteredTelemetryDoesNotFreezeFrontiers(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	runID := uuid.Must(uuid.NewV7())
	eventSubjectID := uuid.Must(uuid.NewV7())
	terminalID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, stream_name, seq, idempotency_key, state, written_at)
		VALUES
			($1, $2, 'run_log', 'run', $3, 'stdout', 1, 'run-log:dead', 'dead_lettered', NULL),
			($1, $2, 'run_log', 'run', $3, 'stderr', 2, 'run-log:written', 'written', now()),
			($1, $2, 'event', 'run', $4, '', 1, 'event:dead', 'dead_lettered', NULL),
			($1, $2, 'terminal_output', 'workspace_exec', $5, 'stdout', 0, 'terminal:dead', 'dead_lettered', NULL)
	`, ids.orgID, testCellID, runID, eventSubjectID, terminalID); err != nil {
		t.Fatal(err)
	}
	runFrontier, err := queries.GetRunLogIngestFrontier(ctx, db.GetRunLogIngestFrontierParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        testCellID,
		RunID:         pgvalue.UUID(runID),
		MaxWrittenSeq: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runFrontier != 2 {
		t.Fatalf("run log frontier = %d, want 2", runFrontier)
	}
	if _, err := queries.UpsertEventWatermark(ctx, db.UpsertEventWatermarkParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		CellID:       testCellID,
		SubjectType:  db.EventSubjectTypeRun,
		SubjectID:    pgvalue.UUID(eventSubjectID),
		WatermarkSeq: 7,
	}); err != nil {
		t.Fatal(err)
	}
	eventWatermark, err := queries.GetEventWatermark(ctx, db.GetEventWatermarkParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		CellID:      testCellID,
		SubjectType: string(db.EventSubjectTypeRun),
		SubjectID:   pgvalue.UUID(eventSubjectID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if eventWatermark != 7 {
		t.Fatalf("event watermark = %d, want 7", eventWatermark)
	}
	terminalFrontier, err := queries.GetTerminalOutputIngestFrontier(ctx, db.GetTerminalOutputIngestFrontierParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		SourceKind:       "workspace_exec",
		SourceID:         pgvalue.UUID(terminalID),
		StreamName:       "stdout",
		MaxWrittenOffset: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminalFrontier != 5 {
		t.Fatalf("terminal frontier = %d, want 5", terminalFrontier)
	}
}

func TestOrphanedTelemetryOutboxDeadLettersAndPrunes(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	runID := uuid.Must(uuid.NewV7())
	eventSubjectID := uuid.Must(uuid.NewV7())
	terminalID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, stream_name, seq, idempotency_key)
		VALUES
			($1, $2, 'run_log', 'run', $3, 'stdout', 1, 'run-log:orphan'),
			($1, $2, 'event', 'run', $4, '', 1, 'event:orphan'),
			($1, $2, 'terminal_output', 'workspace_exec', $5, 'stdout', 0, 'terminal:orphan')
	`, ids.orgID, testCellID, runID, eventSubjectID, terminalID); err != nil {
		t.Fatal(err)
	}

	deadLettered, err := queries.DeadLetterOrphanedTelemetryOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deadLettered) != 3 {
		t.Fatalf("dead-lettered orphan outbox rows = %v, want 3 rows", deadLettered)
	}
	var deadLetteredCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM telemetry_outbox
		 WHERE org_id = $1
		   AND cell_id = $2
		   AND state = 'dead_lettered'
	`, ids.orgID, testCellID).Scan(&deadLetteredCount); err != nil {
		t.Fatal(err)
	}
	if deadLetteredCount != 3 {
		t.Fatalf("dead-lettered count = %d, want 3", deadLetteredCount)
	}
	runFrontier, err := queries.GetRunLogIngestFrontier(ctx, db.GetRunLogIngestFrontierParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        testCellID,
		RunID:         pgvalue.UUID(runID),
		MaxWrittenSeq: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runFrontier != 4 {
		t.Fatalf("run frontier with dead-lettered orphan = %d, want 4", runFrontier)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET updated_at = now() - interval '2 days'
		 WHERE org_id = $1
		   AND cell_id = $2
		   AND state = 'dead_lettered'
	`, ids.orgID, testCellID); err != nil {
		t.Fatal(err)
	}
	pruned, err := queries.PruneTelemetryOutboxWritten(ctx, pgvalue.Interval(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 3 {
		t.Fatalf("pruned dead-lettered outbox rows = %v, want 3 rows", pruned)
	}
}

func TestDeadLetteredUnpublishedEventDoesNotBlockLaterPublish(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	first, err := queries.AppendRunEvent(ctx, db.AppendRunEventParams{
		Kind:    "run.first",
		Payload: []byte(`{"ok":true}`),
		OrgID:   pgvalue.UUID(ids.orgID),
		RunID:   pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.AppendRunEvent(ctx, db.AppendRunEventParams{
		Kind:    "run.second",
		Payload: []byte(`{"ok":true}`),
		OrgID:   pgvalue.UUID(ids.orgID),
		RunID:   pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET state = 'dead_lettered'
		 WHERE stream_kind = 'event'
		   AND source_kind = 'run'
		   AND source_id = $1
		   AND seq = $2
	`, ids.runID, first.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM event_hot_payloads
		 WHERE org_id = $1
		   AND cell_id = $2
		   AND subject_type = 'run'
		   AND subject_id = $3
		   AND seq = $4
	`, ids.orgID, testCellID, ids.runID, first.Seq); err != nil {
		t.Fatal(err)
	}

	claimed, err := queries.ClaimEventOutbox(ctx, db.ClaimEventOutboxParams{
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Seq != second.Seq {
		t.Fatalf("claimed event outbox = %+v, want seq %d only", claimed, second.Seq)
	}
}

func TestRunLogPruneRequiresGracePastWatermark(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	chunk, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           testCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("alpha"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET written_at = now()
		 WHERE stream_kind = 'run_log'
		   AND source_kind = 'run'
		   AND source_id = $1
		   AND seq = $2
	`, ids.runID, chunk.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRunLogWatermark(ctx, db.UpsertRunLogWatermarkParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		CellID:       testCellID,
		RunID:        pgvalue.UUID(ids.runID),
		StreamName:   "__run__",
		WatermarkSeq: chunk.Seq,
	}); err != nil {
		t.Fatal(err)
	}
	pruned, err := queries.PruneRunLogChunksPastWatermark(ctx, db.PruneRunLogChunksPastWatermarkParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     testCellID,
		RunID:      pgvalue.UUID(ids.runID),
		PruneGrace: pgvalue.Interval(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("fresh hot chunk pruned with grace = %v, want none", pruned)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_log_hot_chunks
		   SET created_at = now() - interval '2 hours'
		 WHERE org_id = $1
		   AND cell_id = $2
		   AND run_id = $3
		   AND seq = $4
	`, ids.orgID, testCellID, ids.runID, chunk.Seq); err != nil {
		t.Fatal(err)
	}
	pruned, err = queries.PruneRunLogChunksPastWatermark(ctx, db.PruneRunLogChunksPastWatermarkParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     testCellID,
		RunID:      pgvalue.UUID(ids.runID),
		PruneGrace: pgvalue.Interval(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != chunk.Seq {
		t.Fatalf("stale hot chunk pruned with grace = %v, want [%d]", pruned, chunk.Seq)
	}
}
