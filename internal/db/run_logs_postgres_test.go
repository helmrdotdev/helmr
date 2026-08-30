package db

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAppendRunLogChunkRequiresExactCurrentReceipt(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	params := fixture.runningRunLogParams(t, ctx)

	first, err := fixture.queries.AppendRunLogChunk(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ReplayMatches || string(first.Content) != "alpha" {
		t.Fatalf("first append = %+v", first)
	}
	replay, err := fixture.queries.AppendRunLogChunk(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.ReplayMatches || replay.Seq != first.Seq {
		t.Fatalf("replay = %+v, want seq %d", replay, first.Seq)
	}
	stored, err := fixture.queries.GetRunLogChunkReplay(ctx, GetRunLogChunkReplayParams{
		RunLeaseID: params.RunLeaseID,
		Stream:     params.Stream,
		ObservedSeq: pgtype.Int8{
			Int64: params.ObservedSeq,
			Valid: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	storedPayload, err := jsoncanon.Transform([]byte(stored.EventPayload))
	if err != nil {
		t.Fatal(err)
	}
	requestPayload, err := jsoncanon.Transform(params.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedPayload, requestPayload) ||
		stored.LeaseFenceFingerprint != params.LeaseFenceFingerprint {
		t.Fatalf("replay event payload = %s, want %s", stored.EventPayload, params.Payload)
	}

	changed := params
	changed.Content = []byte("beta")
	if _, err := fixture.queries.AppendRunLogChunk(ctx, changed); !errors.Is(err, pgx.ErrNoRows) {
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
		mutate func(*AppendRunLogChunkParams)
	}{
		{"Run Lease", func(p *AppendRunLogChunkParams) { p.RunLeaseID = randomPGUUID() }},
		{"lease sequence", func(p *AppendRunLogChunkParams) { p.LeaseSequence++ }},
		{"worker group", func(p *AppendRunLogChunkParams) { p.WorkerGroupID += "-stale" }},
		{"worker", func(p *AppendRunLogChunkParams) { p.WorkerInstanceID = randomPGUUID() }},
		{"worker epoch", func(p *AppendRunLogChunkParams) { p.WorkerEpoch++ }},
	}
	for index, mismatch := range mismatches {
		t.Run(mismatch.name, func(t *testing.T) {
			stale := params
			stale.ObservedSeq = int64(index + 2)
			mismatch.mutate(&stale)
			if _, err := fixture.queries.AppendRunLogChunk(ctx, stale); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("AppendRunLogChunk() error = %v, want no rows", err)
			}
		})
	}
}

func TestAppendRunLogChunkRejectsSupersededAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	params := fixture.runningRunLogParams(t, ctx)
	if _, err := fixture.pool.Exec(ctx, `UPDATE worker_groups SET state = 'draining' WHERE id = $1`, params.WorkerGroupID); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.pool.Exec(ctx, `UPDATE run_leases SET expires_at = expires_at + interval '1 minute' WHERE id = $1`, params.RunLeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.AppendRunLogChunk(ctx, params); err != nil {
		t.Fatalf("stable fence was invalidated by renewal: %v", err)
	}

	if _, err := fixture.pool.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'completed', terminal_at = now(), terminal_reason_code = 'completed'
		 WHERE id = $1
	`, params.RunLeaseID); err != nil {
		t.Fatal(err)
	}
	params.ObservedSeq++
	if _, err := fixture.queries.AppendRunLogChunk(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal lease error = %v, want no rows", err)
	}
}

func TestAppendRunLogChunkRequiresCoherentRunAndLeaseState(t *testing.T) {
	ctx := context.Background()

	t.Run("checkpoint pause", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		params := fixture.runningRunLogParams(t, ctx)
		var runID, workspaceID pgtype.UUID
		var attemptNumber int32
		var leaseSequence, runStateVersion int64
		if err := fixture.pool.QueryRow(ctx, `
			SELECT rl.run_id, rl.workspace_id, rl.attempt_number, rl.lease_sequence, r.state_version
			  FROM run_leases rl
			  JOIN runs r ON r.id = rl.run_id
			 WHERE rl.id = $1
		`, params.RunLeaseID).Scan(
			&runID,
			&workspaceID,
			&attemptNumber,
			&leaseSequence,
			&runStateVersion,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.queries.RegisterTimerRunWait(ctx, RegisterTimerRunWaitParams{
			ID:                             randomPGUUID(),
			EnvironmentID:                  pgvalue.UUID(fixture.environmentID),
			DueAt:                          pgvalue.Timestamptz(time.Now().Add(time.Minute)),
			IdleTimeoutMs:                  pgtype.Int8{Int64: 30_000, Valid: true},
			RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("checkpoint-log-wait")),
			AttemptNumber:                  attemptNumber,
			CurrentRunLeaseID:              params.RunLeaseID,
			CheckpointDueAt:                pgvalue.Timestamptz(time.Now().Add(-time.Millisecond)),
			ResumeAttachID:                 randomPGUUID(),
			Metadata:                       []byte(`{}`),
			Tags:                           []string{},
			RunID:                          runID,
			ExpectedRunningStateVersion:    runStateVersion,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.queries.BeginRunLeaseCheckpoint(
			ctx,
			BeginRunLeaseCheckpointParams{
				ID:            params.RunLeaseID,
				RunID:         runID,
				WorkspaceID:   workspaceID,
				AttemptNumber: attemptNumber,
				LeaseSequence: leaseSequence,
			},
		); err != nil {
			t.Fatal(err)
		}

		first, err := fixture.queries.AppendRunLogChunk(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := fixture.queries.AppendRunLogChunk(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if !first.ReplayMatches || !replay.ReplayMatches || replay.Seq != first.Seq {
			t.Fatalf("checkpoint replay first=%+v replay=%+v", first, replay)
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
				"checkpoint replay side effects = chunks %d events %d meter outbox %d meters %d",
				chunks,
				events,
				meterOutbox,
				meters,
			)
		}
	})

	tests := []struct {
		name       string
		runStatus  string
		leaseState string
	}{
		{name: "waiting Run with running lease", runStatus: "waiting", leaseState: "running"},
		{name: "running Run with checkpointing lease", runStatus: "running", leaseState: "checkpointing"},
		{name: "waiting Run with finalizing lease", runStatus: "waiting", leaseState: "finalizing"},
		{name: "running Run with finalizing lease", runStatus: "running", leaseState: "finalizing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunLeaseClaimFixture(t, ctx)
			params := fixture.runningRunLogParams(t, ctx)
			if _, err := fixture.pool.Exec(
				ctx,
				`UPDATE runs SET status = $2 WHERE current_run_lease_id = $1`,
				params.RunLeaseID,
				test.runStatus,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.pool.Exec(ctx, `
				UPDATE run_leases
				   SET state = $2,
				       finalization_operation_id = CASE WHEN $2 = 'finalizing' THEN $3::uuid ELSE NULL END,
				       finalization_kind = CASE WHEN $2 = 'finalizing' THEN 'capture' ELSE NULL END,
				       finalization_started_at = CASE WHEN $2 = 'finalizing' THEN now() ELSE NULL END,
				       finalization_request_fingerprint = CASE WHEN $2 = 'finalizing' THEN 'fixture-finalization' ELSE NULL END
				 WHERE id = $1
			`, params.RunLeaseID, test.leaseState, randomPGUUID()); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.queries.AppendRunLogChunk(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("AppendRunLogChunk() error = %v, want no rows", err)
			}
		})
	}
}

func TestGetRunMetadataClaimScopeUsesStableAttemptAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	logParams := fixture.runningRunLogParams(t, ctx)
	params := GetRunMetadataClaimScopeParams{
		RunLeaseID: logParams.RunLeaseID, LeaseSequence: logParams.LeaseSequence,
		WorkerGroupID:    logParams.WorkerGroupID,
		WorkerInstanceID: logParams.WorkerInstanceID,
		WorkerEpoch:      logParams.WorkerEpoch,
	}

	scope, err := fixture.queries.GetRunMetadataClaimScope(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.RunID.Valid || scope.AttemptNumber <= 0 {
		t.Fatalf("claim scope = %+v", scope)
	}

	if _, err := fixture.pool.Exec(
		ctx,
		`UPDATE runs SET active_elapsed_ms = active_elapsed_ms + 1 WHERE id = $1`,
		scope.RunID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetRunMetadataClaimScope(ctx, params); err != nil {
		t.Fatalf("mutable Run accounting invalidated stable claim scope: %v", err)
	}

	stale := params
	stale.WorkerEpoch++
	if _, err := fixture.queries.GetRunMetadataClaimScope(ctx, stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale worker authority error = %v, want no rows", err)
	}
}

func TestAppendRunLogChunkConcurrentReplay(t *testing.T) {
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
			row, err := fixture.queries.AppendRunLogChunk(ctx, params)
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
) AppendRunLogChunkParams {
	t.Helper()
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	locators := fixture.freshRunStartLocators(t, ctx, work)
	if _, err := fixture.queries.MarkRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, locators)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.MarkRunRunning(ctx, MarkRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: locators.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.TouchRunWorkspaceActivity(ctx, TouchRunWorkspaceActivityParams{
		ID: locators.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}

	params := AppendRunLogChunkParams{
		Kind: "log.stdout", Payload: []byte(`{"stream":"stdout"}`),
		LeaseFenceFingerprint: "fixture-receipt-fingerprint",
		RunLeaseID:            workUUID(work.leaseID),
		LeaseSequence:         1, WorkerGroupID: runLeaseTestWorkerGroup,
		WorkerInstanceID: workUUID(fixture.workerID), WorkerEpoch: 1,
		Stream: "stdout", ObservedSeq: 1, Content: []byte("alpha"),
	}
	return params
}

func randomPGUUID() pgtype.UUID {
	return pgvalue.UUID(uuid.NewV7())
}
