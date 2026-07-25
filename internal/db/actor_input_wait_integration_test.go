package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
			actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)
			startTaskCompletionWork(t, ctx, fixture, work)

			var runVersion int64
			if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
				t.Fatal(err)
			}
			waitID := uuid.Must(uuid.NewV7())
			recordID := uuid.Must(uuid.NewV7())
			register := func() RunWait {
				wait, err := fixture.queries.RegisterActorInputRunWait(ctx, RegisterActorInputRunWaitParams{
					ID: pgvalue.UUID(waitID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
					IdleTimeoutMs: pgtype.Int8{Int64: 30_000, Valid: true}, ActorID: pgvalue.UUID(actorID),
					AfterInputSequence:             pgtype.Int8{Int64: 2, Valid: true},
					RegistrationRequestFingerprint: pgvalue.Text(runLeaseTestDigest("actor-input-wait")), AttemptNumber: 1,
					ActorSpeculativeInputSequence: pgtype.Int8{Int64: 2, Valid: true},
					CurrentRunLeaseID:             pgvalue.UUID(work.leaseID),
					CheckpointDueAt:               pgvalue.Timestamptz(time.Now().Add(30 * time.Second)),
					ResumeAttachID:                pgvalue.UUID(uuid.Must(uuid.NewV7())), Metadata: []byte(`{}`), Tags: []string{},
					RunID: pgvalue.UUID(work.runID), ExpectedRunningStateVersion: runVersion,
				})
				if err != nil {
					t.Fatal(err)
				}
				return wait
			}
			appendRecord := func() AppendActorInputRecordRow {
				record, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
					EnvironmentID: pgvalue.UUID(fixture.environmentID), ActorID: pgvalue.UUID(actorID),
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
				EnvironmentID: pgvalue.UUID(fixture.environmentID), ActorID: pgvalue.UUID(actorID),
				RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
				AfterInputSequence: pgtype.Int8{Int64: 2, Valid: true},
			})
			if err != nil || pending.ID != wait.ID {
				t.Fatalf("pending Wait = %+v, %v", pending, err)
			}
			completed, err := fixture.queries.CompleteHotActorInputRunWait(ctx, CompleteHotActorInputRunWaitParams{
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
				completed.CompletedActorRecordID != record.ID || status != RunStatusRunning {
				t.Fatalf("completion = %+v run=%s", completed, status)
			}
		})
	}
}

func TestActorInputAppendConcurrentSequencesAndKeyedReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)

	rows := make(chan AppendActorInputRecordRow, 2)
	errs := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			start.Wait()
			row, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
				EnvironmentID: pgvalue.UUID(fixture.environmentID), ActorID: pgvalue.UUID(actorID),
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Data: []byte(`{"sender":` + string(rune('0'+index)) + `}`),
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

	claimID := uuid.Must(uuid.NewV7())
	fingerprint := bytes.Repeat([]byte{7}, 32)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, scope_hash, key_hash,
			hash_key_version, generation, request_fingerprint, accepted_at, expires_at
		) VALUES ($1, $2, 'actor.input.send', $3, $4, 1, 1, $5, now(), now() + interval '30 days')
	`, claimID, fixture.environmentID, runLeaseTestHash("actor-input-scope"), runLeaseTestHash("actor-input-key"), fingerprint)
	recordID := uuid.Must(uuid.NewV7())
	first, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		ActorID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: fingerprint,
		ID: pgvalue.UUID(recordID), Data: []byte(`{"keyed":true}`), SourceKind: pgvalue.Text("run"),
		SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || !first.Appended || pgvalue.TextValue(first.SourceKind) != "run" ||
		first.SourceRunID != pgvalue.UUID(work.runID) {
		t.Fatalf("keyed append = %+v, %v", first, err)
	}
	claim, err := fixture.queries.CompleteActorInputClaim(ctx, CompleteActorInputClaimParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		RequestFingerprint: fingerprint, ActorID: pgvalue.UUID(actorID), RecordID: first.ID,
	})
	if err != nil || claim.State != "completed" {
		t.Fatalf("claim completion = %+v, %v", claim, err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(claim.Receipt, &receipt); err != nil || receipt["recordId"] != recordID.String() || receipt["sequence"] != float64(first.Sequence) {
		t.Fatalf("claim receipt = %s, %v", claim.Receipt, err)
	}
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE actors SET manual_run_cancelled = true WHERE id = $1
	`, actorID)
	replay, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		ActorID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: fingerprint,
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Data: []byte(`{"keyed":true}`), SourceKind: pgvalue.Text("run"),
		SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || replay.Appended || replay.ID != first.ID || replay.Sequence != first.Sequence ||
		replay.ClaimFingerprintMismatch || replay.SourceRunID != pgvalue.UUID(work.runID) {
		t.Fatalf("keyed replay = %+v, %v", replay, err)
	}
	var manualRunCancelled bool
	if err := fixture.pool.QueryRow(ctx, `SELECT manual_run_cancelled FROM actors WHERE id = $1`, actorID).Scan(&manualRunCancelled); err != nil {
		t.Fatal(err)
	}
	if !manualRunCancelled {
		t.Fatal("keyed replay cleared a later manual Run cancellation")
	}
	mismatch, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		ActorID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: bytes.Repeat([]byte{8}, 32),
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Data: []byte(`{"keyed":false}`), SourceKind: pgvalue.Text("run"),
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
	actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)

	recordID := uuid.Must(uuid.NewV7())
	reconcileID := uuid.Must(uuid.NewV7())
	claimID := uuid.Must(uuid.NewV7())
	fingerprint := bytes.Repeat([]byte{13}, 32)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, scope_hash, key_hash,
			hash_key_version, generation, request_fingerprint, accepted_at, expires_at
		) VALUES ($1, $2, 'actor.input.send', $3, $4, 1, 1, $5, now(), now() + interval '30 days')
	`, claimID, fixture.environmentID, runLeaseTestHash("run-source-rollback-scope"),
		runLeaseTestHash("run-source-rollback-key"), fingerprint)
	queries := New(tx)
	appended, err := queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		ActorID: pgvalue.UUID(actorID), ExpectedRequestFingerprint: fingerprint,
		ID: pgvalue.UUID(recordID), Data: []byte(`{"rollback":true}`),
		SourceKind: pgvalue.Text("run"), SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil || !appended.Appended {
		t.Fatalf("provisional append = %+v, %v", appended, err)
	}
	if _, err := queries.CompleteActorInputClaim(ctx, CompleteActorInputClaimParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: pgvalue.UUID(claimID),
		RequestFingerprint: fingerprint,
		ActorID:            pgvalue.UUID(actorID), RecordID: appended.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateActorInputReconcileOutbox(ctx, CreateActorInputReconcileOutboxParams{
		ID: pgvalue.UUID(reconcileID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		ActorID: pgvalue.UUID(actorID), RecordID: appended.ID,
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
		       (SELECT count(*) FROM actor_records WHERE id = $2),
		       (SELECT count(*) FROM idempotency_claims
		         WHERE environment_id = $3 AND operation = 'actor.input.send'),
		       (SELECT count(*) FROM outbox_messages WHERE id = $4)
		  FROM actors
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

func TestActorInputSendSourceRequiresExactReceiptWithoutGrantingLiveness(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	var params GetActorInputSendSourceParams
	if err := fixture.pool.QueryRow(ctx, `
		SELECT id, run_id, workspace_id, attempt_number, lease_sequence,
		       worker_group_id, worker_instance_id, worker_epoch,
		       worker_protocol_version, runtime_instance_id, network_slot_id,
		       network_slot_generation, runtime_identity_id, requested_cpu_millis,
		       requested_memory_bytes, requested_workload_disk_bytes,
		       requested_scratch_bytes, requested_execution_slots,
		       start_deadline_at, expires_at
		  FROM run_leases
		 WHERE id = $1
	`, work.leaseID).Scan(
		&params.ID, &params.RunID, &params.WorkspaceID, &params.AttemptNumber,
		&params.LeaseSequence, &params.WorkerGroupID, &params.WorkerInstanceID,
		&params.WorkerEpoch, &params.WorkerProtocolVersion, &params.RuntimeInstanceID,
		&params.NetworkSlotID, &params.NetworkSlotGeneration, &params.RuntimeIdentityID,
		&params.RequestedCpuMillis, &params.RequestedMemoryBytes,
		&params.RequestedWorkloadDiskBytes, &params.RequestedScratchBytes,
		&params.RequestedExecutionSlots, &params.StartDeadlineAt, &params.ExpiresAt,
	); err != nil {
		t.Fatal(err)
	}
	source, err := fixture.queries.GetActorInputSendSource(ctx, params)
	if err != nil || source.EnvironmentID != pgvalue.UUID(fixture.environmentID) ||
		source.RunID != pgvalue.UUID(work.runID) {
		t.Fatalf("exact source receipt = %+v, %v", source, err)
	}

	previous := params
	params.ExpiresAt = pgvalue.Timestamptz(params.ExpiresAt.Time.Add(time.Minute))
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'cancelled', expires_at = $2, terminal_at = now(),
		       terminal_reason_code = 'test_stale_actor_input_source'
		 WHERE id = $1
	`, work.leaseID, params.ExpiresAt)
	if _, err := fixture.queries.GetActorInputSendSource(ctx, previous); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("superseded receipt error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetActorInputSendSource(ctx, params); err != nil {
		t.Fatalf("current persisted terminal receipt error = %v", err)
	}
	if _, err := fixture.queries.GetLiveRunLeaseLocators(ctx, GetLiveRunLeaseLocatorsParams{
		ID: params.ID, LeaseSequence: params.LeaseSequence,
		WorkerGroupID: params.WorkerGroupID, WorkerInstanceID: params.WorkerInstanceID,
		WorkerEpoch: params.WorkerEpoch, WorkerProtocolVersion: params.WorkerProtocolVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal source liveness error = %v, want no rows", err)
	}
}

func TestActorInputAppendRejectsOversizedRetainedJSONAtomically(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)

	// The submitted representation is small, but PostgreSQL expands each
	// scientific-notation number when converting jsonb back to text.
	data := []byte(`[` + strings.Repeat(`5e-324,`, 4_000) + `0]`)
	if len(data) >= 1<<20 {
		t.Fatalf("submitted JSON size = %d, want below 1 MiB", len(data))
	}
	recordID := uuid.Must(uuid.NewV7())
	_, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID),
		ActorID:       pgvalue.UUID(actorID),
		ID:            pgvalue.UUID(recordID),
		Data:          data,
		SourceKind:    pgvalue.Text("external"),
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) ||
		pgErr.Code != "23514" ||
		pgErr.ConstraintName != "actor_records_data_size_check" {
		t.Fatalf("append error = %v, want actor record data size check violation", err)
	}

	var nextInputSequence int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT next_input_sequence FROM actors WHERE id = $1
	`, actorID).Scan(&nextInputSequence); err != nil {
		t.Fatal(err)
	}
	if nextInputSequence != 3 {
		t.Fatalf("next input sequence = %d, want 3 after failed append", nextInputSequence)
	}
	var recordCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM actor_records WHERE id = $1
	`, recordID).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != 0 {
		t.Fatalf("oversized record count = %d, want 0", recordCount)
	}
}

func TestActorInputSequenceSafeIntegerBoundaryPreservesCompletedReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)

	const maxSafeSequence int64 = 9_007_199_254_740_991
	const exhaustedSentinel int64 = maxSafeSequence + 1
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE actors SET next_input_sequence = $2 WHERE id = $1
	`, actorID, maxSafeSequence)

	claimID := uuid.Must(uuid.NewV7())
	fingerprint := bytes.Repeat([]byte{9}, 32)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, scope_hash, key_hash,
			hash_key_version, generation, request_fingerprint, accepted_at, expires_at
		) VALUES ($1, $2, 'actor.input.send', $3, $4, 1, 1, $5, now(), now() + interval '30 days')
	`, claimID, fixture.environmentID, runLeaseTestHash("actor-input-max-scope"), runLeaseTestHash("actor-input-max-key"), fingerprint)
	recordID := uuid.Must(uuid.NewV7())
	first, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID:              pgvalue.UUID(fixture.environmentID),
		ClaimID:                    pgvalue.UUID(claimID),
		ActorID:                    pgvalue.UUID(actorID),
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
		ActorID:            pgvalue.UUID(actorID),
		RecordID:           first.ID,
	})
	if err != nil || claim.State != "completed" {
		t.Fatalf("claim completion = %+v, %v", claim, err)
	}

	_, err = fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID),
		ActorID:       pgvalue.UUID(actorID),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Data:          []byte(`{"after":"maximum"}`),
		SourceKind:    pgvalue.Text("external"),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new append error = %v, want sequence exhaustion", err)
	}

	replay, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID:              pgvalue.UUID(fixture.environmentID),
		ClaimID:                    pgvalue.UUID(claimID),
		ActorID:                    pgvalue.UUID(actorID),
		ExpectedRequestFingerprint: fingerprint,
		ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
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
		       (SELECT count(*) FROM actor_records
		         WHERE actor_id = actors.id AND direction = 'input' AND sequence = $2)
		  FROM actors
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
	actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)
	startTaskCompletionWork(t, ctx, fixture, work)
	var runVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	wait, err := fixture.queries.RegisterActorInputRunWait(ctx, RegisterActorInputRunWaitParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		TimeoutAt:     pgvalue.Timestamptz(time.Now().Add(-time.Millisecond)),
		IdleTimeoutMs: pgtype.Int8{Int64: 30_000, Valid: true}, ActorID: pgvalue.UUID(actorID),
		AfterInputSequence:             pgtype.Int8{Int64: 2, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(runLeaseTestDigest("actor-input-timeout")), AttemptNumber: 1,
		ActorSpeculativeInputSequence: pgtype.Int8{Int64: 2, Valid: true}, CurrentRunLeaseID: pgvalue.UUID(work.leaseID),
		CheckpointDueAt: pgvalue.Timestamptz(time.Now().Add(30 * time.Second)),
		ResumeAttachID:  pgvalue.UUID(uuid.Must(uuid.NewV7())), Metadata: []byte(`{}`), Tags: []string{},
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
	actorID := convertTokenWaitWorkToActor(t, ctx, fixture, work, `{"enabled":false}`)
	var workspaceID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM actors WHERE id = $1`, actorID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases
		   SET state = 'released', released_at = now(), terminal_at = now()
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'cancelled', terminal_at = now(), terminal_reason_code = 'test_idle'
		 WHERE id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'failed', current_run_lease_id = NULL,
		       terminal_at = now(), terminal_reason_code = 'test_idle'
		 WHERE id = $1
	`, work.runID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE actors
		   SET current_run_id = NULL, committed_input_sequence = 2,
		       manual_run_cancelled = true
		 WHERE id = $1
	`, actorID)
	input, err := fixture.queries.AppendActorInputRecord(ctx, AppendActorInputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ActorID: pgvalue.UUID(actorID),
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Data: []byte(`{"wake":true}`), SourceKind: pgvalue.Text("external"),
	})
	if err != nil || input.Sequence != 3 {
		t.Fatalf("wake input = %+v, %v", input, err)
	}
	var manualRunCancelled bool
	if err := fixture.pool.QueryRow(ctx, `SELECT manual_run_cancelled FROM actors WHERE id = $1`, actorID).Scan(&manualRunCancelled); err != nil {
		t.Fatal(err)
	}
	if manualRunCancelled {
		t.Fatal("new input did not clear the manual Run cancellation hold")
	}
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE actors
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
			publicIDValue, err := publicid.New(publicid.Run)
			if err != nil {
				results <- result{err: err}
				return
			}
			run, err := fixture.queries.CreateActorContinuationRun(ctx, CreateActorContinuationRunParams{
				RunID: pgvalue.UUID(uuid.Must(uuid.NewV7())), PublicID: publicIDValue,
				QueueOriginAt: pgvalue.Timestamptz(time.Now().UTC()),
				TraceID:       pgvalue.Text("11111111111111111111111111111111"), RootSpanID: "2222222222222222",
				EnvironmentID: pgvalue.UUID(fixture.environmentID), ActorID: pgvalue.UUID(actorID),
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
		created.ActorStartInputSequence.Int64 != 2 || created.ActorStartInputHighWatermark.Int64 != 3 {
		t.Fatalf("continuation CAS = created %d no-rows %d run %+v", createdCount, noRowsCount, created)
	}
	var actorCurrentRun uuid.UUID
	var runCount, attemptCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT actors.current_run_id,
		       (SELECT count(*) FROM runs WHERE actor_id = actors.id AND cause_kind = 'continuation'),
		       (SELECT count(*) FROM run_attempts WHERE run_id = actors.current_run_id)
		  FROM actors WHERE actors.id = $1
	`, actorID).Scan(&actorCurrentRun, &runCount, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if actorCurrentRun != pgvalue.MustUUIDValue(created.ID) || runCount != 1 || attemptCount != 1 {
		t.Fatalf("durable continuation = current %s runs %d attempts %d", actorCurrentRun, runCount, attemptCount)
	}
}
