package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorInputWaitAppendAndRegistrationOrdersConverge(t *testing.T) {
	for _, appendFirst := range []bool{false, true} {
		name := "registration-first"
		if appendFirst {
			name = "append-first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunLeaseClaimFixture(t, ctx)
			work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
			startTaskCompletionWork(t, ctx, fixture, work)

			var runVersion int64
			if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
				t.Fatal(err)
			}
			waitID := uuid.NewV7()
			recordID := uuid.NewV7()
			register := func() RunWait {
				wait, err := fixture.queries.RegisterActorInputRunWait(ctx, RegisterActorInputRunWaitParams{
					ID: pgvalue.UUID(waitID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
					IdleTimeoutMs: pgtype.Int8{Int64: 30_000, Valid: true}, SessionID: pgvalue.UUID(actorID),
					AfterInputSequence:             pgtype.Int8{Int64: 2, Valid: true},
					RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("actor-input-wait")), AttemptNumber: 1,
					ActorSpeculativeInputSequence: pgtype.Int8{Int64: 2, Valid: true},
					CurrentRunLeaseID:             pgvalue.UUID(work.leaseID),
					CheckpointDueAt:               pgvalue.Timestamptz(time.Now().Add(30 * time.Second)),
					ResumeAttachID:                pgvalue.UUID(uuid.NewV7()), Metadata: []byte(`{}`), Tags: []string{},
					RunID: pgvalue.UUID(work.runID), ExpectedRunningStateVersion: runVersion,
				})
				if err != nil {
					t.Fatal(err)
				}
				return wait
			}
			appendRecord := func() AppendActorInputRecordRow {
				record, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
					EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
					ID: pgvalue.UUID(recordID), Data: []byte(`{"message":"ready"}`), SourceKind: pgvalue.Text("external"),
				})
				if err != nil {
					t.Fatal(err)
				}
				if !record.Appended || record.Sequence != 3 {
					t.Fatalf("append = %+v", record)
				}
				return record
			}

			var wait RunWait
			var record AppendActorInputRecordRow
			if appendFirst {
				record = appendRecord()
				wait = register()
			} else {
				wait = register()
				record = appendRecord()
			}
			pending, err := fixture.queries.GetPendingActorInputRunWait(ctx, GetPendingActorInputRunWaitParams{
				EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
				RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
				AfterInputSequence: pgtype.Int8{Int64: 2, Valid: true},
			})
			if err != nil || pending.ID != wait.ID {
				t.Fatalf("pending Wait = %+v, %v", pending, err)
			}
			completed, err := fixture.queries.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{
				ConditionResult: []byte(`{"value":{"message":"ready"}}`), CompletedActorRecordID: record.ID,
				ID: pending.ID, RunID: pending.RunID, ExpectedRunStateVersion: pending.ExpectedRunStateVersion,
				CurrentRunLeaseID: pending.CurrentRunLeaseID, AttemptNumber: pending.AttemptNumber,
			})
			if err != nil {
				t.Fatal(err)
			}
			var status RunStatus
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, work.runID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if completed.ConditionState != WaitStateCompleted || completed.SuspensionState != RunWaitStateReleased ||
				completed.CompletedActorRecordID != record.ID ||
				completed.CompletedActorRecordDirection.String != "input" ||
				!completed.CompletedActorRecordDirection.Valid ||
				status != RunStatusRunning {
				t.Fatalf("completion = %+v run=%s", completed, status)
			}
		})
	}
}

func TestActorInputAppendConcurrentSequencesAndKeyedReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)

	rows := make(chan AppendActorInputRecordRow, 2)
	errs := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for index := range 2 {
		go func() {
			start.Wait()
			row, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
				EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
				ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`{"sender":` + string(rune('0'+index)) + `}`),
				SourceKind: pgvalue.Text("external"),
			})
			if err != nil {
				errs <- err
				return
			}
			rows <- row
		}()
	}
	start.Done()
	sequences := map[int64]bool{}
	for range 2 {
		select {
		case err := <-errs:
			t.Fatal(err)
		case row := <-rows:
			sequences[row.Sequence] = true
		}
	}
	if !sequences[3] || !sequences[4] || len(sequences) != 2 {
		t.Fatalf("concurrent sequences = %+v", sequences)
	}

	claimID := uuid.NewV7()
	fingerprint := bytes.Repeat([]byte{7}, 32)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, slot_hash,
			request_fingerprint, accepted_at, expires_at
		) VALUES ($1, $2, 'session.input.send', $3, $4, now(), now() + interval '30 days')
	`, claimID, fixture.environmentID, dbtest.Hash("actor-input-slot"), fingerprint)
	recordID := uuid.NewV7()
	first, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		SessionID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: fingerprint,
		ID: pgvalue.UUID(recordID), Data: []byte(`{"keyed":true}`), SourceKind: pgvalue.Text("run"),
		SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || !first.Appended || pgvalue.TextValue(first.SourceKind) != "run" ||
		first.SourceRunID != pgvalue.UUID(work.runID) {
		t.Fatalf("keyed append = %+v, %v", first, err)
	}
	claim, err := fixture.queries.CompleteActorInputClaim(ctx, CompleteActorInputClaimParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		RequestFingerprint: fingerprint, SessionID: pgvalue.UUID(actorID), RecordID: first.ID,
	})
	if err != nil || claim.State != "completed" {
		t.Fatalf("claim completion = %+v, %v", claim, err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(claim.Receipt, &receipt); err != nil || receipt["session_record_id"] != recordID.String() || receipt["sequence"] != float64(first.Sequence) {
		t.Fatalf("claim receipt = %s, %v", claim.Receipt, err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE sessions SET manual_run_cancelled = true WHERE id = $1
	`, actorID)
	replay, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		SessionID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: fingerprint,
		ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`{"keyed":true}`), SourceKind: pgvalue.Text("run"),
		SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || replay.Appended || replay.ID != first.ID || replay.Sequence != first.Sequence ||
		replay.ClaimFingerprintMismatch || replay.SourceRunID != pgvalue.UUID(work.runID) {
		t.Fatalf("keyed replay = %+v, %v", replay, err)
	}
	var manualRunCancelled bool
	if err := fixture.pool.QueryRow(ctx, `SELECT manual_run_cancelled FROM sessions WHERE id = $1`, actorID).Scan(&manualRunCancelled); err != nil {
		t.Fatal(err)
	}
	if !manualRunCancelled {
		t.Fatal("keyed replay cleared a later manual Run cancellation")
	}
	mismatch, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		SessionID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: bytes.Repeat([]byte{8}, 32),
		ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`{"keyed":false}`), SourceKind: pgvalue.Text("run"),
		SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || !mismatch.ClaimFingerprintMismatch || mismatch.ID != first.ID {
		t.Fatalf("keyed mismatch = %+v, %v", mismatch, err)
	}
}

func TestActorInputRunSourceTransactionRollbackLeavesNoResidue(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)

	recordID := uuid.NewV7()
	reconcileID := uuid.NewV7()
	claimID := uuid.NewV7()
	fingerprint := bytes.Repeat([]byte{13}, 32)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, slot_hash,
			request_fingerprint, accepted_at, expires_at
		) VALUES ($1, $2, 'session.input.send', $3, $4, now(), now() + interval '30 days')
	`, claimID, fixture.environmentID, dbtest.Hash("run-source-rollback-scope"),
		fingerprint)
	queries := New(tx)
	appended, err := queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		SessionID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: fingerprint,
		ID: pgvalue.UUID(recordID), Data: []byte(`{"rollback":true}`),
		SourceKind: pgvalue.Text("run"), SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || !appended.Appended {
		t.Fatalf("provisional append = %+v, %v", appended, err)
	}
	if _, err := queries.CompleteActorInputClaim(ctx, CompleteActorInputClaimParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		RequestFingerprint: fingerprint,
		SessionID:          pgvalue.UUID(actorID), RecordID: appended.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateActorInputReconcileOutbox(ctx, CreateActorInputReconcileOutboxParams{
		ID: pgvalue.UUID(reconcileID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		SessionID: pgvalue.UUID(actorID), RecordID: appended.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// This models the control transaction aborting when source authority turns
	// stale after its provisional durable writes.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var nextInputSequence int64
	var recordCount, claimCount, outboxCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT next_input_sequence,
		       (SELECT count(*) FROM session_records WHERE id = $2),
		       (SELECT count(*) FROM idempotency_claims
		         WHERE environment_id = $3 AND operation = 'session.input.send'),
		       (SELECT count(*) FROM control_outbox WHERE id = $4)
		  FROM sessions
		 WHERE id = $1
	`, actorID, recordID, fixture.environmentID, reconcileID).Scan(
		&nextInputSequence, &recordCount, &claimCount, &outboxCount,
	); err != nil {
		t.Fatal(err)
	}
	if nextInputSequence != 3 || recordCount != 0 || claimCount != 0 || outboxCount != 0 {
		t.Fatalf("rollback residue = next %d records %d claims %d outbox %d, want 3/0/0/0",
			nextInputSequence, recordCount, claimCount, outboxCount)
	}
}

func TestActorInputSendSourceRequiresCurrentLeaseFence(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	locators := fixture.freshRunStartLocators(t, ctx, work)
	if _, err := fixture.queries.MarkRunLeaseRunning(
		ctx,
		fixture.freshRunLeaseRunningParams(work, locators),
	); err != nil {
		t.Fatal(err)
	}
	var params GetActorInputSendSourceParams
	if err := fixture.pool.QueryRow(ctx, `
		SELECT id, lease_sequence, worker_group_id, worker_instance_id,
		       worker_epoch
		  FROM run_leases
		 WHERE id = $1
	`, work.leaseID).Scan(
		&params.ID, &params.LeaseSequence, &params.WorkerGroupID,
		&params.WorkerInstanceID, &params.WorkerEpoch,
	); err != nil {
		t.Fatal(err)
	}
	source, err := fixture.queries.GetActorInputSendSource(ctx, params)
	if err != nil || source.EnvironmentID != pgvalue.UUID(fixture.environmentID) ||
		source.RunID != pgvalue.UUID(work.runID) {
		t.Fatalf("exact source receipt = %+v, %v", source, err)
	}

	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'cancelled', terminal_at = now(),
		       terminal_reason_code = 'test_stale_actor_input_source'
		 WHERE id = $1
	`, work.leaseID)
	if _, err := fixture.queries.GetActorInputSendSource(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal source error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetLiveRunLeaseLocators(
		ctx,
		GetLiveRunLeaseLocatorsParams(params),
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal source liveness error = %v, want no rows", err)
	}
}

func TestActorInputSequenceSafeIntegerBoundaryPreservesCompletedReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)

	const maxSafeSequence int64 = 9_007_199_254_740_991
	const exhaustedSentinel int64 = maxSafeSequence + 1
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE sessions SET next_input_sequence = $2 WHERE id = $1
	`, actorID, maxSafeSequence)

	claimID := uuid.NewV7()
	fingerprint := bytes.Repeat([]byte{9}, 32)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, slot_hash,
			request_fingerprint, accepted_at, expires_at
		) VALUES ($1, $2, 'session.input.send', $3, $4, now(), now() + interval '30 days')
	`, claimID, fixture.environmentID, dbtest.Hash("actor-input-max-slot"), fingerprint)
	recordID := uuid.NewV7()
	first, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID:              pgvalue.UUID(fixture.environmentID),
		ClaimID:                    pgvalue.UUID(claimID),
		SessionID:                  pgvalue.UUID(actorID),
		ExpectedRequestFingerprint: fingerprint,
		ID:                         pgvalue.UUID(recordID),
		Data:                       []byte(`{"at":"maximum"}`),
		SourceKind:                 pgvalue.Text("external"),
	})
	if err != nil || !first.Appended || first.Sequence != maxSafeSequence {
		t.Fatalf("maximum append = %+v, %v", first, err)
	}
	claim, err := fixture.queries.CompleteActorInputClaim(ctx, CompleteActorInputClaimParams{
		EnvironmentID:      pgvalue.UUID(fixture.environmentID),
		ClaimID:            pgvalue.UUID(claimID),
		RequestFingerprint: fingerprint,
		SessionID:          pgvalue.UUID(actorID),
		RecordID:           first.ID,
	})
	if err != nil || claim.State != "completed" {
		t.Fatalf("claim completion = %+v, %v", claim, err)
	}

	_, err = fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID),
		SessionID:     pgvalue.UUID(actorID),
		ID:            pgvalue.UUID(uuid.NewV7()),
		Data:          []byte(`{"after":"maximum"}`),
		SourceKind:    pgvalue.Text("external"),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new append error = %v, want sequence exhaustion", err)
	}

	replay, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID:              pgvalue.UUID(fixture.environmentID),
		ClaimID:                    pgvalue.UUID(claimID),
		SessionID:                  pgvalue.UUID(actorID),
		ExpectedRequestFingerprint: fingerprint,
		ID:                         pgvalue.UUID(uuid.NewV7()),
		Data:                       []byte(`{"at":"maximum"}`),
		SourceKind:                 pgvalue.Text("external"),
	})
	if err != nil || replay.Appended || replay.ID != first.ID || replay.Sequence != maxSafeSequence {
		t.Fatalf("completed replay = %+v, %v", replay, err)
	}

	var nextInputSequence int64
	var recordCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT next_input_sequence,
		       (SELECT count(*) FROM session_records
		         WHERE session_id = sessions.id AND direction = 'input' AND sequence = $2)
		  FROM sessions
		 WHERE id = $1
	`, actorID, maxSafeSequence).Scan(&nextInputSequence, &recordCount); err != nil {
		t.Fatal(err)
	}
	if nextInputSequence != exhaustedSentinel || recordCount != 1 {
		t.Fatalf("Actor boundary = next %d records %d, want next %d records 1",
			nextInputSequence, recordCount, exhaustedSentinel)
	}
}

func TestActorInputWaitTimeoutReleasesHotRun(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	startTaskCompletionWork(t, ctx, fixture, work)
	var runVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	wait, err := fixture.queries.RegisterActorInputRunWait(ctx, RegisterActorInputRunWaitParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		TimeoutAt:     pgvalue.Timestamptz(time.Now().Add(-time.Millisecond)),
		IdleTimeoutMs: pgtype.Int8{Int64: 30_000, Valid: true}, SessionID: pgvalue.UUID(actorID),
		AfterInputSequence:             pgtype.Int8{Int64: 2, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("actor-input-timeout")), AttemptNumber: 1,
		ActorSpeculativeInputSequence: pgtype.Int8{Int64: 2, Valid: true}, CurrentRunLeaseID: pgvalue.UUID(work.leaseID),
		CheckpointDueAt: pgvalue.Timestamptz(time.Now().Add(30 * time.Second)),
		ResumeAttachID:  pgvalue.UUID(uuid.NewV7()), Metadata: []byte(`{}`), Tags: []string{},
		RunID: pgvalue.UUID(work.runID), ExpectedRunningStateVersion: runVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := fixture.queries.ListPendingActorInputWaitTimeouts(ctx, 10)
	if err != nil || len(candidates) != 1 || candidates[0].ID != wait.ID {
		t.Fatalf("timeout candidates = %+v, %v", candidates, err)
	}
	failed, err := fixture.queries.FailHotRunWait(ctx, FailHotRunWaitParams{
		ReasonCode: pgvalue.Text("wait_timeout"), ConditionError: []byte(`{"code":"wait_timeout","retryable":false}`),
		ID: wait.ID, RunID: wait.RunID, ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
		CurrentRunLeaseID: wait.CurrentRunLeaseID, AttemptNumber: wait.AttemptNumber,
	})
	if err != nil {
		t.Fatal(err)
	}
	var status RunStatus
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, work.runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if failed.ConditionState != WaitStateFailed || failed.SuspensionState != RunWaitStateReleased ||
		pgvalue.TextValue(failed.ConditionReasonCode) != "wait_timeout" || status != RunStatusRunning {
		t.Fatalf("timeout completion = %+v run=%s", failed, status)
	}
}

func TestActorInputClosingContinuationCASCreatesOneRun(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	var workspaceID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM sessions WHERE id = $1`, actorID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases
		   SET state = 'released', released_at = now(), terminal_at = now()
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'cancelled', terminal_at = now(), terminal_reason_code = 'test_idle'
		 WHERE id = $1
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'failed', current_run_lease_id = NULL,
		       terminal_at = now(),
		       failure = '{"code":"test_idle","message":"Test run failed","details":{}}'::jsonb
		 WHERE id = $1
	`, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE sessions
		   SET current_run_id = NULL, committed_input_sequence = 2,
		       manual_run_cancelled = true
		 WHERE id = $1
	`, actorID)
	input, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
		ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`{"wake":true}`), SourceKind: pgvalue.Text("external"),
	})
	if err != nil || input.Sequence != 3 {
		t.Fatalf("wake input = %+v, %v", input, err)
	}
	var manualRunCancelled bool
	if err := fixture.pool.QueryRow(ctx, `SELECT manual_run_cancelled FROM sessions WHERE id = $1`, actorID).Scan(&manualRunCancelled); err != nil {
		t.Fatal(err)
	}
	if manualRunCancelled {
		t.Fatal("new input did not clear the manual Run cancellation hold")
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE sessions
		   SET state = 'closing', close_sequence = 3
		 WHERE id = $1
	`, actorID)

	type result struct {
		run CreateActorContinuationRunRow
		err error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			run, err := fixture.queries.CreateActorContinuationRun(ctx, CreateActorContinuationRunParams{
				RunID:         pgvalue.UUID(uuid.NewV7()),
				QueueOriginAt: pgvalue.Timestamptz(time.Now().UTC()),
				TraceID:       pgvalue.Text("11111111111111111111111111111111"), RootSpanID: "2222222222222222",
				EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
				WorkspaceID: pgvalue.UUID(workspaceID), ExpectedRunGeneration: 1,
			})
			results <- result{run: run, err: err}
		}()
	}
	start.Done()
	var created CreateActorContinuationRunRow
	createdCount, noRowsCount := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			createdCount++
			created = result.run
		case errors.Is(result.err, pgx.ErrNoRows):
			noRowsCount++
		default:
			t.Fatal(result.err)
		}
	}
	if createdCount != 1 || noRowsCount != 1 || created.CauseKind != "continuation" ||
		created.SessionInputStartSequence.Int64 != 2 || created.SessionInputHighWatermark.Int64 != 3 {
		t.Fatalf("continuation CAS = created %d no-rows %d run %+v", createdCount, noRowsCount, created)
	}
	var actorCurrentRun uuid.UUID
	var runCount, attemptCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT sessions.current_run_id,
		       (SELECT count(*) FROM runs WHERE session_id = sessions.id AND cause_kind = 'continuation'),
		       (SELECT count(*) FROM run_attempts WHERE run_id = sessions.current_run_id)
		  FROM sessions WHERE sessions.id = $1
	`, actorID).Scan(&actorCurrentRun, &runCount, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if actorCurrentRun != pgvalue.MustUUIDValue(created.ID) || runCount != 1 || attemptCount != 1 {
		t.Fatalf("durable continuation = current %s runs %d attempts %d", actorCurrentRun, runCount, attemptCount)
	}
}
