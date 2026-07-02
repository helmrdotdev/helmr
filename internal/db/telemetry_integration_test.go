package db_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const testCellID = "us-east-1-cell-1"

func TestAppendRunLogChunkIdempotentUsageFactsAndByteContinuity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	first, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind:             "run.log",
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
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
		Payload:          []byte(`{"stream":"stdout"}`),
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      2,
		Content:          []byte("beta"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != first.Seq+1 {
		t.Fatalf("second seq = %d, first seq = %d", second.Seq, first.Seq)
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
	if !bytes.Equal(joined, []byte("alphabeta")) {
		t.Fatalf("joined log bytes = %q", joined)
	}

	var usageCount int64
	var usageBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(quantity), 0)
		  FROM usage_facts
		 WHERE org_id = $1
		   AND run_id = $2
		   AND meter = 'log_bytes'
	`, ids.orgID, ids.runID).Scan(&usageCount, &usageBytes); err != nil {
		t.Fatal(err)
	}
	if usageCount != 2 || usageBytes != int64(len("alphabeta")) {
		t.Fatalf("usage facts count=%d bytes=%d", usageCount, usageBytes)
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
	if eventCount != 2 {
		t.Fatalf("event count = %d, want 2", eventCount)
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
		OrgID:  pgvalue.UUID(ids.orgID),
		CellID: testCellID,
		RunID:  pgvalue.UUID(ids.runID),
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
	if len(rows) != 1 || rows[0].Seq != second.Seq || !bytes.Equal(rows[0].Content, []byte("beta")) {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedEvents) != 0 {
		t.Fatalf("pruned unpublished events = %v, want none", prunedEvents)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_outbox
		   SET written_at = now()
		 WHERE event_record_id IN (
		       SELECT id
		         FROM event_hot_payloads AS events
		        WHERE org_id = $1
		          AND run_id = $2
		          AND seq = $3
		   )
	`, ids.orgID, ids.runID, first.Seq); err != nil {
		t.Fatal(err)
	}
	prunedEvents, err = queries.PruneEventsPastWatermark(ctx, db.PruneEventsPastWatermarkParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		CellID:      testCellID,
		SubjectType: db.EventSubjectTypeRun,
		SubjectID:   pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedEvents) != 1 || prunedEvents[0] != first.Seq {
		t.Fatalf("pruned events = %v, want [%d]", prunedEvents, first.Seq)
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
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_exec_stream_chunks (
			org_id, cell_id, project_id, environment_id, workspace_id, exec_id, stream, offset_start, offset_end, data
		)
		VALUES
			($1, $2, $3, $4, $5, $6, 'stdout', 0, 5, 'alpha'::bytea),
			($1, $2, $3, $4, $5, $6, 'stdout', 5, 9, 'beta'::bytea)
	`, ids.orgID, testCellID, ids.projectID, ids.environmentID, ids.workspaceID, execID); err != nil {
		t.Fatal(err)
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedExec) != 1 || prunedExec[0] != 5 {
		t.Fatalf("pruned exec offsets = %v, want [5]", prunedExec)
	}

	ptyID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_pty_sessions (
			id, org_id, cell_id, project_id, environment_id, workspace_id,
			cols, rows, state, created_by_subject_type, created_by_subject_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 80, 24, 'running', 'test', 'test')
	`, ptyID, ids.orgID, testCellID, ids.projectID, ids.environmentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_pty_stream_chunks (
			org_id, cell_id, project_id, environment_id, workspace_id, pty_session_id, stream, offset_start, offset_end, data
		)
		VALUES
			($1, $2, $3, $4, $5, $6, 'output', 0, 4, 'echo'::bytea),
			($1, $2, $3, $4, $5, $6, 'output', 4, 10, 'prompt'::bytea)
	`, ids.orgID, testCellID, ids.projectID, ids.environmentID, ids.workspaceID, ptyID); err != nil {
		t.Fatal(err)
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedPty) != 1 || prunedPty[0] != 4 {
		t.Fatalf("pruned pty offsets = %v, want [4]", prunedPty)
	}
}
