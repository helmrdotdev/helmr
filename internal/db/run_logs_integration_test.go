package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAppendReceiptRunLogChunkRequiresExactCurrentReceipt(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	params := fixture.runningRunLogParams(t, ctx)

	first, err := fixture.queries.AppendReceiptRunLogChunk(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ReplayMatches || string(first.Content) != "alpha" {
		t.Fatalf("first append = %+v", first)
	}
	replay, err := fixture.queries.AppendReceiptRunLogChunk(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.ReplayMatches || replay.Seq != first.Seq {
		t.Fatalf("replay = %+v, want seq %d", replay, first.Seq)
	}

	changed := params
	changed.Content = []byte("beta")
	if _, err := fixture.queries.AppendReceiptRunLogChunk(ctx, changed); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("changed replay error = %v, want no rows", err)
	}
	var chunks, events, meterOutbox, meters int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE stream_kind = 'run_log'),
		       count(*) FILTER (WHERE stream_kind = 'event'),
		       count(*) FILTER (WHERE stream_kind = 'meter_event')
		  FROM telemetry_outbox
		 WHERE run_lease_id = $1
	`, params.RunLeaseID).Scan(&chunks, &events, &meterOutbox); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM meter_events WHERE run_lease_id = $1 AND meter = 'log_bytes'
	`, params.RunLeaseID).Scan(&meters); err != nil {
		t.Fatal(err)
	}
	if chunks != 1 || events != 1 || meterOutbox != 1 || meters != 1 {
		t.Fatalf(
			"replay side effects = chunks %d events %d meter outbox %d meters %d",
			chunks,
			events,
			meterOutbox,
			meters,
		)
	}

	mismatches := []struct {
		name   string
		mutate func(*AppendReceiptRunLogChunkParams)
	}{
		{"Workspace Mount", func(p *AppendReceiptRunLogChunkParams) { p.WorkspaceMountID = randomPGUUID() }},
		{"Workspace Lease", func(p *AppendReceiptRunLogChunkParams) { p.WorkspaceLeaseID = randomPGUUID() }},
		{"Run Lease", func(p *AppendReceiptRunLogChunkParams) { p.RunLeaseID = randomPGUUID() }},
		{"Run", func(p *AppendReceiptRunLogChunkParams) { p.RunID = randomPGUUID() }},
		{"attempt", func(p *AppendReceiptRunLogChunkParams) { p.AttemptNumber++ }},
		{"lease sequence", func(p *AppendReceiptRunLogChunkParams) { p.LeaseSequence++ }},
		{"worker group", func(p *AppendReceiptRunLogChunkParams) { p.WorkerGroupID += "-stale" }},
		{"worker", func(p *AppendReceiptRunLogChunkParams) { p.WorkerInstanceID = randomPGUUID() }},
		{"worker epoch", func(p *AppendReceiptRunLogChunkParams) { p.WorkerEpoch++ }},
		{"worker protocol", func(p *AppendReceiptRunLogChunkParams) { p.WorkerProtocolVersion += "-stale" }},
		{"Runtime", func(p *AppendReceiptRunLogChunkParams) { p.RuntimeInstanceID = randomPGUUID() }},
		{"Runtime identity", func(p *AppendReceiptRunLogChunkParams) { p.RuntimeIdentityID += "-stale" }},
		{"network slot", func(p *AppendReceiptRunLogChunkParams) { p.NetworkSlotID = randomPGUUID() }},
		{"network generation", func(p *AppendReceiptRunLogChunkParams) { p.NetworkSlotGeneration++ }},
		{"Workspace", func(p *AppendReceiptRunLogChunkParams) { p.WorkspaceID = randomPGUUID() }},
		{"base Workspace version", func(p *AppendReceiptRunLogChunkParams) { p.BaseWorkspaceVersionID = randomPGUUID() }},
		{"mount fence", func(p *AppendReceiptRunLogChunkParams) { p.MountFencingGeneration++ }},
		{"ownership generation", func(p *AppendReceiptRunLogChunkParams) { p.OwnershipGeneration++ }},
		{"writer generation", func(p *AppendReceiptRunLogChunkParams) { p.WriterGeneration++ }},
		{"CPU", func(p *AppendReceiptRunLogChunkParams) { p.RequestedCpuMillis++ }},
		{"memory", func(p *AppendReceiptRunLogChunkParams) { p.RequestedMemoryBytes++ }},
		{"workload disk", func(p *AppendReceiptRunLogChunkParams) { p.RequestedWorkloadDiskBytes++ }},
		{"scratch", func(p *AppendReceiptRunLogChunkParams) { p.RequestedScratchBytes++ }},
		{"execution slots", func(p *AppendReceiptRunLogChunkParams) { p.RequestedExecutionSlots++ }},
		{"max active duration", func(p *AppendReceiptRunLogChunkParams) { p.MaxActiveDurationMs++ }},
		{"active elapsed", func(p *AppendReceiptRunLogChunkParams) { p.ActiveElapsedMs++ }},
		{"trace ID", func(p *AppendReceiptRunLogChunkParams) { p.TraceID.String += "stale" }},
		{"span ID", func(p *AppendReceiptRunLogChunkParams) { p.SpanID.String += "stale" }},
		{"traceparent", func(p *AppendReceiptRunLogChunkParams) { p.Traceparent.String += "stale" }},
		{"start deadline", func(p *AppendReceiptRunLogChunkParams) {
			p.StartDeadlineAt.Time = p.StartDeadlineAt.Time.Add(time.Second)
		}},
		{"expiry", func(p *AppendReceiptRunLogChunkParams) { p.ExpiresAt.Time = p.ExpiresAt.Time.Add(time.Second) }},
	}
	for index, mismatch := range mismatches {
		t.Run(mismatch.name, func(t *testing.T) {
			stale := params
			stale.ObservedSeq = int64(index + 2)
			mismatch.mutate(&stale)
			if _, err := fixture.queries.AppendReceiptRunLogChunk(ctx, stale); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("AppendReceiptRunLogChunk() error = %v, want no rows", err)
			}
		})
	}
}

func TestAppendReceiptRunLogChunkRejectsSupersededAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	params := fixture.runningRunLogParams(t, ctx)
	if _, err := fixture.pool.Exec(ctx, `UPDATE worker_groups SET state = 'draining' WHERE id = $1`, params.WorkerGroupID); err != nil {
		t.Fatal(err)
	}

	newExpiry := params.ExpiresAt.Time.Add(time.Minute)
	if _, err := fixture.pool.Exec(ctx, `UPDATE run_leases SET expires_at = $2 WHERE id = $1`, params.RunLeaseID, newExpiry); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.AppendReceiptRunLogChunk(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale renewal receipt error = %v, want no rows", err)
	}
	params.ExpiresAt.Time = newExpiry
	if _, err := fixture.queries.AppendReceiptRunLogChunk(ctx, params); err != nil {
		t.Fatalf("renewed receipt error = %v", err)
	}

	if _, err := fixture.pool.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'completed', terminal_at = now(), terminal_reason_code = 'completed'
		 WHERE id = $1
	`, params.RunLeaseID); err != nil {
		t.Fatal(err)
	}
	params.ObservedSeq++
	if _, err := fixture.queries.AppendReceiptRunLogChunk(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal lease error = %v, want no rows", err)
	}
}

func TestAppendReceiptRunLogChunkConcurrentReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	params := fixture.runningRunLogParams(t, ctx)

	const writers = 8
	errorsByWriter := make([]error, writers)
	sequences := make([]int64, writers)
	var group sync.WaitGroup
	group.Add(writers)
	for index := range writers {
		go func() {
			defer group.Done()
			row, err := fixture.queries.AppendReceiptRunLogChunk(ctx, params)
			errorsByWriter[index] = err
			sequences[index] = row.Seq
		}()
	}
	group.Wait()
	for index := range writers {
		if errorsByWriter[index] != nil {
			t.Fatalf("writer %d error = %v", index, errorsByWriter[index])
		}
		if sequences[index] != sequences[0] {
			t.Fatalf("writer %d seq = %d, want %d", index, sequences[index], sequences[0])
		}
	}
	var chunks, events, meters int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE stream_kind = 'run_log'),
		       count(*) FILTER (WHERE stream_kind = 'event'),
		       count(*) FILTER (WHERE stream_kind = 'meter_event')
		  FROM telemetry_outbox
		 WHERE run_lease_id = $1
	`, params.RunLeaseID).Scan(&chunks, &events, &meters); err != nil {
		t.Fatal(err)
	}
	if chunks != 1 || events != 1 || meters != 1 {
		t.Fatalf("concurrent replay side effects = chunks %d events %d meters %d", chunks, events, meters)
	}
}

func (fixture runLeaseClaimFixture) runningRunLogParams(
	t *testing.T,
	ctx context.Context,
) AppendReceiptRunLogChunkParams {
	t.Helper()
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	locators := fixture.freshRunStartLocators(t, ctx, work)
	if _, err := fixture.queries.MarkFreshRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, locators)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.MarkFreshRunRunning(ctx, MarkFreshRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: locators.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.TouchFreshRunWorkspace(ctx, TouchFreshRunWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 1, RunID: workUUID(work.runID),
	}); err != nil {
		t.Fatal(err)
	}

	params := AppendReceiptRunLogChunkParams{
		Kind: "log.stdout", Payload: []byte(`{"stream":"stdout"}`),
		WorkspaceMountID: locators.WorkspaceMountID, WorkspaceLeaseID: locators.WorkspaceLeaseID,
		RunLeaseID: workUUID(work.leaseID), RunID: workUUID(work.runID), AttemptNumber: 1,
		LeaseSequence: 1, WorkerGroupID: runLeaseTestWorkerGroup,
		WorkerInstanceID: workUUID(fixture.workerID), WorkerEpoch: 1,
		WorkerProtocolVersion: runLeaseTestProtocol, RuntimeInstanceID: locators.RuntimeInstanceID,
		RuntimeIdentityID: fixture.runtimeIdentityID, NetworkSlotID: locators.NetworkSlotID,
		NetworkSlotGeneration: locators.NetworkSlotGeneration, WorkspaceID: locators.WorkspaceID,
		Stream: "stdout", ObservedSeq: 1, Content: []byte("alpha"),
	}
	var traceID, spanID, traceparent string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT workspace_leases.base_version_id,
		       workspace_leases.mount_fencing_generation,
		       workspace_leases.ownership_generation,
		       workspace_leases.writer_generation,
		       run_leases.requested_cpu_millis,
		       run_leases.requested_memory_bytes,
		       run_leases.requested_workload_disk_bytes,
		       run_leases.requested_scratch_bytes,
		       run_leases.requested_execution_slots,
		       runs.max_active_duration_ms,
		       runs.active_elapsed_ms,
		       COALESCE(run_leases.trace_id, ''),
		       COALESCE(run_leases.span_id, ''),
		       COALESCE(run_leases.traceparent, ''),
		       run_leases.start_deadline_at,
		       run_leases.expires_at
		  FROM run_leases
		  JOIN runs ON runs.id = run_leases.run_id
		  JOIN workspace_leases ON workspace_leases.id = $2
		 WHERE run_leases.id = $1
	`, params.RunLeaseID, params.WorkspaceLeaseID).Scan(
		&params.BaseWorkspaceVersionID,
		&params.MountFencingGeneration,
		&params.OwnershipGeneration,
		&params.WriterGeneration,
		&params.RequestedCpuMillis,
		&params.RequestedMemoryBytes,
		&params.RequestedWorkloadDiskBytes,
		&params.RequestedScratchBytes,
		&params.RequestedExecutionSlots,
		&params.MaxActiveDurationMs,
		&params.ActiveElapsedMs,
		&traceID,
		&spanID,
		&traceparent,
		&params.StartDeadlineAt,
		&params.ExpiresAt,
	); err != nil {
		t.Fatal(err)
	}
	params.TraceID = pgtype.Text{String: traceID, Valid: true}
	params.SpanID = pgtype.Text{String: spanID, Valid: true}
	params.Traceparent = pgtype.Text{String: traceparent, Valid: true}
	return params
}

func randomPGUUID() pgtype.UUID {
	return pgvalue.UUID(uuid.Must(uuid.NewV7()))
}
