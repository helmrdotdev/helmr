package token

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTokenWaitRegistrationImmediatelyMatchesTerminalTokenAfterEmptyReconcile(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture, tokenID, "sha256:late-registration", `{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Examined != 0 {
		t.Fatalf("empty reconcile = %+v, %v", batch, err)
	}
	var expectedRunVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT revision FROM runs WHERE id = $1`, work.runID).Scan(&expectedRunVersion); err != nil {
		t.Fatal(err)
	}
	waitID := uuid.NewV7()
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ConditionStatus != db.WaitStatusCompleted || registered.SuspensionStatus != db.RunWaitStatusReleased ||
		registered.RunRevision != expectedRunVersion+2 || string(registered.Result) != `{"approved": true}` {
		t.Fatalf("registration = %+v", registered)
	}
	replayed, err := reconciler.RegisterWait(ctx, registration)
	if err != nil || replayed.WaitID != registered.WaitID || replayed.ConditionStatus != registered.ConditionStatus ||
		replayed.SuspensionStatus != registered.SuspensionStatus || string(replayed.Result) != string(registered.Result) {
		t.Fatalf("registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	var runStatus db.RunStatus
	var runVersion int64
	var condition db.WaitStatus
	var suspension db.RunWaitStatus
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.revision, run_waits.condition_status, run_waits.suspension_status
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&runStatus, &runVersion, &condition, &suspension); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusRunning || runVersion != expectedRunVersion+2 || condition != db.WaitStatusCompleted || suspension != db.RunWaitStatusReleased {
		t.Fatalf("durable registration = run %s/%d condition %s suspension %s workspace %s", runStatus, runVersion, condition, suspension, authority.workspaceID)
	}
}

func TestTokenWaitRegistrationBeforeCompletionIsReconciled(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	var runVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT revision FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	waitID := uuid.NewV7()
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ConditionStatus != db.WaitStatusPending || registered.SuspensionStatus != db.RunWaitStatusHot ||
		registered.RunRevision != runVersion+1 {
		t.Fatalf("pending registration = %+v", registered)
	}
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture, tokenID, "sha256:registration-first", `{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Examined != 1 || batch.Resolved != 1 {
		t.Fatalf("registration-first reconcile = %+v, %v", batch, err)
	}
	var status db.RunStatus
	var condition db.WaitStatus
	var suspension db.RunWaitStatus
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_waits.condition_status, run_waits.suspension_status
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&status, &condition, &suspension); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusRunning || condition != db.WaitStatusCompleted || suspension != db.RunWaitStatusReleased {
		t.Fatalf("registration-first state = run %s condition %s suspension %s", status, condition, suspension)
	}
}

func TestTokenReconcileConvergesAfterControlOutboxPrune(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	waitID := uuid.NewV7()
	if _, err := reconciler.RegisterWait(ctx, tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture, tokenID, "sha256:prune-converge", `{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Examined != 1 || batch.Resolved != 1 {
		t.Fatalf("first reconcile = %+v, %v", batch, err)
	}

	claimed, err := fixture.queries.ClaimControlOutbox(ctx, db.ClaimControlOutboxParams{
		ClaimedBy:      pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Topics:         []string{"token.reconcile"},
		RowLimit:       8,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if _, err := fixture.queries.DeliverControlOutbox(ctx, db.DeliverControlOutboxParams{
		ID: claimed[0].ID, ClaimedBy: claimed[0].ClaimedBy, ClaimAttempt: claimed[0].Attempts,
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE control_outbox
		   SET delivered_at = now() - interval '25 hours'
		 WHERE id = $1
	`, claimed[0].ID)
	pruned, err := fixture.queries.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(24 * time.Hour),
		RowLimit:  32,
	})
	if err != nil || pruned != 1 {
		t.Fatalf("prune = %v, %v", pruned, err)
	}

	var runVersion int64
	var status db.RunStatus
	var condition db.WaitStatus
	var suspension db.RunWaitStatus
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.revision, run_waits.condition_status, run_waits.suspension_status
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&status, &runVersion, &condition, &suspension); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"environmentId":"` + fixture.environmentID.String() + `","tokenId":"` + tokenID.String() + `"}`)
	if _, err := fixture.queries.CreateControlOutbox(ctx, db.CreateControlOutboxParams{
		ID: pgvalue.UUID(uuid.NewV7()), Topic: "token.reconcile", Payload: payload,
		AvailableAt: pgvalue.Timestamptz(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || replay.Examined != 0 || replay.Resolved != 0 {
		t.Fatalf("re-enqueue reconcile = %+v, %v", replay, err)
	}

	var replayVersion int64
	var replayStatus db.RunStatus
	var replayCondition db.WaitStatus
	var replaySuspension db.RunWaitStatus
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.revision, run_waits.condition_status, run_waits.suspension_status
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&replayStatus, &replayVersion, &replayCondition, &replaySuspension); err != nil {
		t.Fatal(err)
	}
	if replayStatus != status || replayVersion != runVersion ||
		replayCondition != condition || replaySuspension != suspension {
		t.Fatalf(
			"re-enqueue mutated authority: before %s/%d/%s/%s after %s/%d/%s/%s",
			status, runVersion, condition, suspension,
			replayStatus, replayVersion, replayCondition, replaySuspension,
		)
	}
}

func TestTokenCompletionReconcilesEveryWaitingRunInBoundedBatches(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	first := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	second := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, first)
	startTaskCompletionWork(t, ctx, fixture, second)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, work := range []runLeaseWork{first, second} {
		request := tokenWaitRegistrationRequest(
			t,
			ctx,
			fixture,
			work,
			tokenID,
			uuid.NewV7(),
		)
		registered, err := reconciler.RegisterWait(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if registered.ConditionStatus != db.WaitStatusPending ||
			registered.SuspensionStatus != db.RunWaitStatusHot {
			t.Fatalf("pending registration = %+v", registered)
		}
	}
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture,
		tokenID,
		"sha256:multi-run-fan-out",
		`{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}

	for batchNumber := 1; batchNumber <= 2; batchNumber++ {
		batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 0 {
			t.Fatalf("batch %d = %+v", batchNumber, batch)
		}
	}
	replay, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Examined != 0 || replay.Resolved != 0 || replay.Deferred != 0 {
		t.Fatalf("replay batch = %+v", replay)
	}

	var completedWaits, runningRuns int
	if err := fixture.pool.QueryRow(ctx, `
SELECT count(*),
       count(*) FILTER (WHERE runs.status = 'running')
  FROM run_waits
  JOIN runs ON runs.id = run_waits.run_id
 WHERE run_waits.environment_id = $1
   AND run_waits.token_id = $2
   AND run_waits.condition_status = 'completed'
   AND run_waits.suspension_status = 'released'
   AND run_waits.condition_result = '{"approved":true}'::jsonb
`, fixture.environmentID, tokenID).Scan(&completedWaits, &runningRuns); err != nil {
		t.Fatal(err)
	}
	if completedWaits != 2 || runningRuns != 2 {
		t.Fatalf("fan-out = completed Waits %d, running Runs %d", completedWaits, runningRuns)
	}
}

func TestTokenWaitSchemaRejectsCrossEnvironmentReference(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	otherEnvironmentID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO environments (
		    id, org_id, project_id, slug, name, color_hex
		) VALUES ($1, $2, $3, $4, 'Other Environment', '#3366ff')
	`, otherEnvironmentID, fixture.orgID, fixture.projectID,
		"other-"+dbtest.ShortID(otherEnvironmentID))
	otherTokenID := uuid.NewV7()
	if _, err := fixture.queries.CreateToken(ctx, db.CreateTokenParams{
		ID:    pgvalue.UUID(otherTokenID),
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID:             pgvalue.UUID(otherEnvironmentID),
		ExpiresAt:                 pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		CallbackSecretFingerprint: make([]byte, 32),
		Metadata:                  []byte(`{}`), Tags: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	var workspaceID uuid.UUID
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT workspace_id FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO run_waits (
		    id, environment_id, run_id, workspace_id, kind, token_id,
		    token_registration_run_revision, expected_run_revision,
		    attempt_number, current_run_lease_id, resume_attach_id
		) VALUES ($1, $2, $3, $4, 'token', $5, 0, 1, 1, $6, $7)
	`, uuid.NewV7(), fixture.environmentID, work.runID, workspaceID,
		otherTokenID, work.leaseID, uuid.NewV7()); err == nil {
		t.Fatal("cross-Environment Token Wait reference was accepted")
	}
}

func TestPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t *testing.T) {
	testPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t, false)
}

func TestPendingRootActorTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t *testing.T) {
	testPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t, true)
}

func testPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t *testing.T, actor bool) {
	t.Helper()
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	if actor {
		fixture.base.ConvertToActor(
			t,
			ctx,
			runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
			`{"enabled":false}`,
		)
	}
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	if actor {
		registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 1, Valid: true}
	}
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE run_waits SET checkpoint_due_at = transaction_timestamp() WHERE id = $1`, registered.WaitID); err != nil {
		t.Fatal(err)
	}
	checkpointID := uuid.NewV7()
	privateVersionID := uuid.NewV7()
	workspaceArtifactID := uuid.NewV7()
	workspaceDigest := dbtest.Digest("checkpoint-workspace-artifact-" + checkpointID.String())
	workspaceTreeDigest := dbtest.Digest("checkpoint-workspace-tree-" + checkpointID.String())
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	queries := db.New(tx)
	if _, err := queries.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{
		ID:    pgvalue.UUID(checkpointID),
		RunID: pgvalue.UUID(work.runID), AttemptNumber: int32(1),
		RunWaitID: pgvalue.UUID(registered.WaitID), SourceRunLeaseID: pgvalue.UUID(work.leaseID),
		SourceWorkspaceLeaseID: pgvalue.UUID(authority.workspaceLeaseID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		BaseWorkspaceVersionID:        pgvalue.UUID(authority.physicalVersionID),
		ActorSpeculativeInputSequence: registration.ActorSpeculativeInputSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BeginRunLeaseCheckpoint(ctx, db.BeginRunLeaseCheckpointParams{
		ID: pgvalue.UUID(work.leaseID), RunID: pgvalue.UUID(work.runID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: int32(1),
		LeaseSequence: registration.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	wait, err := queries.RequestRunWaitCheckpoint(ctx, db.RequestRunWaitCheckpointParams{
		SuspendCheckpointID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(work.runID),
		AttemptNumber: int32(1), ID: pgvalue.UUID(registered.WaitID),
		CurrentRunLeaseID: pgvalue.UUID(work.leaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID: pgvalue.UUID(fixture.orgID), Digest: workspaceDigest, SizeBytes: 10,
		MediaType: "application/vnd.helmr.workspace.v0.tar",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(workspaceArtifactID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		Digest: workspaceDigest, Kind: db.ArtifactKindWorkspaceVersion, SizeBytes: 10,
		MediaType: "application/vnd.helmr.workspace.v0.tar",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePrivateCheckpointWorkspaceVersion(ctx, db.CreatePrivateCheckpointWorkspaceVersionParams{
		ID:            pgvalue.UUID(privateVersionID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		ParentVersionID: pgvalue.UUID(authority.physicalVersionID),
		RootPackDigest:  pgvalue.Text(workspaceTreeDigest), LogicalBytes: 10,
		SourceWorkspaceLeaseID: pgvalue.UUID(authority.workspaceLeaseID), OwnershipGeneration: 1, WriterGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RegisterCheckpointManifest(ctx, db.RegisterCheckpointManifestParams{ID: pgvalue.UUID(checkpointID), Manifest: []byte(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`)}); err != nil {
		t.Fatal(err)
	}
	checkpointArtifacts := dbtest.InsertCheckpointArtifacts(t, ctx, fixture.pool, work.runID, checkpointID.String())
	wrongEnvironmentID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Wrong checkpoint environment', '#000000')
	`, wrongEnvironmentID, fixture.orgID, fixture.projectID, "wrong-checkpoint-"+dbtest.ShortID(wrongEnvironmentID))
	dbtest.MustExec(t, ctx, fixture.pool, `UPDATE artifacts SET environment_id = $1 WHERE id = $2`,
		wrongEnvironmentID, checkpointArtifacts.Memory)
	if _, err := queries.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
		PrivateWorkspaceVersionID: pgvalue.UUID(privateVersionID),
		RuntimeConfigArtifactID:   pgvalue.UUID(checkpointArtifacts.RuntimeConfig),
		VMStateArtifactID:         pgvalue.UUID(checkpointArtifacts.VMState),
		MemoryArtifactID:          pgvalue.UUID(checkpointArtifacts.Memory),
		ScratchDiskArtifactID:     pgvalue.UUID(checkpointArtifacts.ScratchDisk),
		Manifest:                  []byte(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		ReadyRequestFingerprint:   pgvalue.Text(dbtest.Digest("checkpoint-ready-wrong-environment-" + checkpointID.String())),
		RunID:                     pgvalue.UUID(work.runID), AttemptNumber: int32(1), ID: pgvalue.UUID(checkpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-environment checkpoint artifact error = %v, want no rows", err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `UPDATE artifacts SET environment_id = $1 WHERE id = $2`,
		fixture.environmentID, checkpointArtifacts.Memory)
	if _, err := queries.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
		PrivateWorkspaceVersionID: pgvalue.UUID(privateVersionID),
		RuntimeConfigArtifactID:   pgvalue.UUID(checkpointArtifacts.VMState),
		VMStateArtifactID:         pgvalue.UUID(checkpointArtifacts.RuntimeConfig),
		MemoryArtifactID:          pgvalue.UUID(checkpointArtifacts.Memory),
		ScratchDiskArtifactID:     pgvalue.UUID(checkpointArtifacts.ScratchDisk),
		Manifest:                  []byte(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		ReadyRequestFingerprint:   pgvalue.Text(dbtest.Digest("checkpoint-ready-wrong-kinds-" + checkpointID.String())),
		RunID:                     pgvalue.UUID(work.runID), AttemptNumber: int32(1), ID: pgvalue.UUID(checkpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong checkpoint artifact kinds error = %v, want no rows", err)
	}
	if _, err := queries.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
		PrivateWorkspaceVersionID: pgvalue.UUID(privateVersionID),
		RuntimeConfigArtifactID:   pgvalue.UUID(checkpointArtifacts.RuntimeConfig),
		VMStateArtifactID:         pgvalue.UUID(checkpointArtifacts.VMState),
		MemoryArtifactID:          pgvalue.UUID(checkpointArtifacts.Memory),
		ScratchDiskArtifactID:     pgvalue.UUID(checkpointArtifacts.ScratchDisk),
		Manifest:                  []byte(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		ReadyRequestFingerprint:   pgvalue.Text(dbtest.Digest("checkpoint-ready-" + checkpointID.String())),
		RunID:                     pgvalue.UUID(work.runID), AttemptNumber: int32(1), ID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `SAVEPOINT missing_checkpoint_artifact`)
	if _, err := tx.Exec(ctx, `UPDATE run_checkpoints SET memory_artifact_id = NULL WHERE id = $1`, checkpointID); err == nil {
		t.Fatal("database accepted a ready checkpoint with a missing artifact")
	}
	dbtest.MustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT missing_checkpoint_artifact`)
	if _, err := tx.Exec(ctx, `UPDATE run_checkpoints SET runtime_config_artifact_id = $1 WHERE id = $2`,
		checkpointArtifacts.VMState, checkpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetReadyRunCheckpoint(ctx, db.GetReadyRunCheckpointParams{
		RunID: pgvalue.UUID(work.runID), AttemptNumber: 1, ID: pgvalue.UUID(checkpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong-kind persisted checkpoint read error = %v, want no rows", err)
	}
	if _, err := queries.LockRestorableRunCheckpoint(ctx, db.LockRestorableRunCheckpointParams{
		ID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
		RunWaitID: pgvalue.UUID(registered.WaitID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong-kind restore lock error = %v, want no rows", err)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE run_checkpoints SET runtime_config_artifact_id = $1 WHERE id = $2`,
		checkpointArtifacts.RuntimeConfig, checkpointID)
	dbtest.MustExec(t, ctx, tx, `UPDATE artifacts SET environment_id = $1 WHERE id = $2`,
		wrongEnvironmentID, checkpointArtifacts.Memory)
	if _, err := queries.GetReadyRunCheckpoint(ctx, db.GetReadyRunCheckpointParams{
		RunID: pgvalue.UUID(work.runID), AttemptNumber: 1, ID: pgvalue.UUID(checkpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-environment persisted checkpoint read error = %v, want no rows", err)
	}
	if _, err := queries.LockRestorableRunCheckpoint(ctx, db.LockRestorableRunCheckpointParams{
		ID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
		RunWaitID: pgvalue.UUID(registered.WaitID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-environment restore lock error = %v, want no rows", err)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE artifacts SET environment_id = $1 WHERE id = $2`,
		fixture.environmentID, checkpointArtifacts.Memory)
	if _, err := queries.LockRestorableRunCheckpoint(ctx, db.LockRestorableRunCheckpointParams{
		ID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
		RunWaitID: pgvalue.UUID(registered.WaitID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
	}); err != nil {
		t.Fatalf("corrected restore lock: %v", err)
	}
	dbtest.MustExec(t, ctx, tx, `SAVEPOINT referenced_checkpoint_cas`)
	if _, err := tx.Exec(ctx, `DELETE FROM cas_objects WHERE org_id = $1 AND digest = $2`,
		fixture.orgID, dbtest.Digest(checkpointID.String()+"-runtime-config")); err == nil {
		t.Fatal("CAS deletion bypassed a ready checkpoint artifact reference")
	}
	dbtest.MustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT referenced_checkpoint_cas`)
	unattachedSeed := "unattached-checkpoint-artifact-" + checkpointID.String()
	unattached := dbtest.InsertCheckpointArtifacts(t, ctx, tx, work.runID, unattachedSeed)
	dbtest.MustExec(t, ctx, tx, `DELETE FROM cas_objects WHERE org_id = $1 AND digest = $2`,
		fixture.orgID, dbtest.Digest(unattachedSeed+"-runtime-config"))
	var unattachedArtifactCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM artifacts WHERE id = $1`, unattached.RuntimeConfig).Scan(&unattachedArtifactCount); err != nil {
		t.Fatal(err)
	}
	if unattachedArtifactCount != 0 {
		t.Fatal("unreferenced Artifact did not cascade with its CAS object")
	}
	if _, err := queries.CloseRunActiveIntervalForCheckpoint(ctx, db.CloseRunActiveIntervalForCheckpointParams{
		ID: pgvalue.UUID(work.runID), OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		AttemptNumber: int32(1), RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	checkpointedAt := pgvalue.Timestamptz(time.Now().UTC())
	if _, err := queries.UpdateTaskWorkspaceMountFrontier(ctx, db.UpdateTaskWorkspaceMountFrontierParams{
		NewVersionID: pgvalue.UUID(privateVersionID), CompletedAt: checkpointedAt,
		ID: pgvalue.UUID(authority.mountID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), RuntimeInstanceID: pgvalue.UUID(authority.runtimeID),
		BaseWorkspaceVersionID: pgvalue.UUID(authority.physicalVersionID), MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CheckpointRunLease(ctx, db.CheckpointRunLeaseParams{
		CheckpointedAt: checkpointedAt, ID: pgvalue.UUID(work.leaseID),
		RunID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		AttemptNumber: int32(1), LeaseSequence: registration.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseCheckpointWorkspaceLease(ctx, db.ReleaseCheckpointWorkspaceLeaseParams{
		CheckpointedAt: checkpointedAt, ID: pgvalue.UUID(authority.workspaceLeaseID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), WorkspaceMountID: pgvalue.UUID(authority.mountID),
		RuntimeInstanceID: pgvalue.UUID(authority.runtimeID), OwnerRunLeaseID: pgvalue.UUID(work.leaseID),
		BaseWorkspaceVersionID: pgvalue.UUID(authority.physicalVersionID), OwnershipGeneration: 1,
		WriterGeneration: 1, MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	var runtimeDesiredVersion, runtimeObservedVersion int64
	if err := tx.QueryRow(ctx, `
SELECT desired_version, observed_version
  FROM runtime_instances
 WHERE id = $1`, authority.runtimeID).Scan(&runtimeDesiredVersion, &runtimeObservedVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DetachCheckpointSource(ctx, db.DetachCheckpointSourceParams{
		CheckpointedAt:          checkpointedAt,
		WorkspaceMountID:        pgvalue.UUID(authority.mountID),
		RuntimeInstanceID:       pgvalue.UUID(authority.runtimeID),
		WorkerInstanceID:        pgvalue.UUID(fixture.workerID),
		WorkerEpoch:             1,
		MountFencingGeneration:  2,
		ExpectedDesiredVersion:  runtimeDesiredVersion,
		ExpectedObservedVersion: runtimeObservedVersion,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CommitPendingCheckpointReady(ctx, db.CommitPendingCheckpointReadyParams{
		CheckpointedAt: checkpointedAt, RunID: pgvalue.UUID(work.runID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: int32(1),
		RunLeaseID: pgvalue.UUID(work.leaseID), ExpectedRunRevision: wait.ExpectedRunRevision,
		CheckpointRequestVersion: wait.CheckpointRequestVersion, RunWaitID: pgvalue.UUID(registered.WaitID),
		CheckpointID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var runStatus db.RunStatus
	var currentLease pgtype.UUID
	var leaseStatus db.RunLeaseStatus
	var workspaceLeaseStatus db.WorkspaceLeaseStatus
	var suspension db.RunWaitStatus
	var priorLease pgtype.UUID
	var checkpointStatus db.RunCheckpointStatus
	var mountVersion uuid.UUID
	var mountStatus db.WorkspaceMountStatus
	var runtimeDesiredState, runtimeObservedState string
	var reservedRunID pgtype.UUID
	var mountTerminalReason string
	var runtimeTerminalReason pgtype.Text
	var reclaimEvidence []byte
	if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, runs.current_run_lease_id, run_leases.status, workspace_leases.status,
       run_waits.suspension_status, run_waits.prior_run_lease_id, run_checkpoints.status,
       workspace_mounts.materialized_version_id, workspace_mounts.status,
       runtime_instances.desired_state, runtime_instances.observed_state,
       runtime_instances.reserved_run_id, workspace_mounts.terminal_reason_code,
       runtime_instances.terminal_reason_code, runtime_instances.reclaim_evidence
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.id = $3
  JOIN run_waits ON run_waits.id = $4
  JOIN run_checkpoints ON run_checkpoints.id = $5
  JOIN workspace_mounts ON workspace_mounts.id = $6
  JOIN runtime_instances ON runtime_instances.id = workspace_mounts.runtime_instance_id
 WHERE runs.id = $1`, work.runID, work.leaseID, authority.workspaceLeaseID,
		registered.WaitID, checkpointID, authority.mountID,
	).Scan(&runStatus, &currentLease, &leaseStatus, &workspaceLeaseStatus, &suspension, &priorLease,
		&checkpointStatus, &mountVersion, &mountStatus, &runtimeDesiredState, &runtimeObservedState,
		&reservedRunID, &mountTerminalReason, &runtimeTerminalReason, &reclaimEvidence); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusWaiting || currentLease.Valid || leaseStatus != db.RunLeaseStatusCheckpointed ||
		workspaceLeaseStatus != db.WorkspaceLeaseStatusReleased || suspension != db.RunWaitStatusParked ||
		!priorLease.Valid || uuid.UUID(priorLease.Bytes) != work.leaseID ||
		checkpointStatus != db.RunCheckpointStatusReady || mountVersion != privateVersionID ||
		mountStatus != db.WorkspaceMountStatusUnmounted ||
		runtimeDesiredState != "closed" || runtimeObservedState != "ready" ||
		reservedRunID.Valid || mountTerminalReason != "checkpointed" || runtimeTerminalReason.Valid ||
		len(reclaimEvidence) != 0 {
		t.Fatalf("ready checkpoint state = run=%s/%v lease=%s workspace_lease=%s wait=%s/%v checkpoint=%s mount=%s/%s/%s runtime=%s/%s/%s reserved=%v cleanup=%+v",
			runStatus, currentLease, leaseStatus, workspaceLeaseStatus, suspension, priorLease, checkpointStatus,
			mountVersion, mountStatus, mountTerminalReason, runtimeDesiredState, runtimeObservedState,
			runtimeTerminalReason.String, reservedRunID, reclaimEvidence)
	}

}

func TestTokenWaitRegistrationConcurrentReplayConverges(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	type registrationOutcome struct {
		result WaitRegistrationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan registrationOutcome, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			result, err := reconciler.RegisterWait(ctx, request)
			outcomes <- registrationOutcome{result: result, err: err}
		})
	}
	close(start)
	workers.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if outcome.err != nil || outcome.result.WaitID != request.WaitID ||
			outcome.result.ConditionStatus != db.WaitStatusPending ||
			outcome.result.SuspensionStatus != db.RunWaitStatusHot {
			t.Fatalf("concurrent registration = %+v, %v", outcome.result, outcome.err)
		}
	}
	var waitCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM run_waits WHERE id = $1`, request.WaitID).Scan(&waitCount); err != nil {
		t.Fatal(err)
	}
	if waitCount != 1 {
		t.Fatalf("concurrent registration created %d Waits", waitCount)
	}
}

func TestTokenWaitRegistrationReplaySurvivesParkedCompletion(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var workspaceID, workspaceLeaseID, baseWorkspaceVersionID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, workspace_leases.id, runs.base_workspace_version_id
		  FROM runs
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = runs.current_run_lease_id
		 WHERE runs.id = $1
	`, work.runID).Scan(&workspaceID, &workspaceLeaseID, &baseWorkspaceVersionID); err != nil {
		t.Fatal(err)
	}
	checkpointID := uuid.NewV7()
	checkpointArtifacts := dbtest.InsertCheckpointArtifacts(t, ctx, fixture.pool, work.runID, checkpointID.String())
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO run_checkpoints (
		    id, run_id, attempt_number, run_wait_id,
		    source_run_lease_id, source_workspace_lease_id, workspace_id,
		    base_workspace_version_id, private_workspace_version_id,
		    runtime_config_artifact_id, vm_state_artifact_id,
		    memory_artifact_id, scratch_disk_artifact_id,
		    status, manifest, ready_request_fingerprint, ready_at
		) VALUES (
		    $1, $2, 1, $3, $4, $5, $6, $7, $7,
		    $8, $9, $10, $11,
		    'ready', '{"test":true}'::jsonb, 'sha256:70ac3c8c49385651ccc368788f78f79e99cd6f3094c74f1eb89fa896cfce3863', transaction_timestamp()
		)
	`, checkpointID, work.runID, request.WaitID, work.leaseID, workspaceLeaseID, workspaceID, baseWorkspaceVersionID,
		checkpointArtifacts.RuntimeConfig, checkpointArtifacts.VMState, checkpointArtifacts.Memory, checkpointArtifacts.ScratchDisk)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'checkpointed', checkpointed_at = transaction_timestamp(),
		       terminal_at = transaction_timestamp(), terminal_reason_code = 'checkpointed'
		 WHERE id = $1
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases
		   SET status = 'released', released_at = transaction_timestamp(), terminal_at = transaction_timestamp()
		 WHERE id = $1
	`, workspaceLeaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs SET current_run_lease_id = NULL, active_started_at = NULL WHERE id = $1
	`, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_waits
		   SET suspension_status = 'parked', current_run_lease_id = NULL,
		       prior_run_lease_id = $1, suspend_checkpoint_id = $2
		 WHERE id = $3
	`, work.leaseID, checkpointID, request.WaitID)
	if _, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, tokenID)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Resolved != 1 {
		t.Fatalf("parked completion = %+v, %v", batch, err)
	}
	replayed, err := reconciler.RegisterWait(ctx, request)
	if err != nil || replayed.WaitID != request.WaitID || replayed.ConditionStatus != db.WaitStatusCancelled ||
		replayed.SuspensionStatus != db.RunWaitStatusResumePending ||
		replayed.ReasonCode != "token_cancelled" ||
		replayed.RunRevision != registered.RunRevision+1 {
		t.Fatalf("parked registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	recomputed := request
	recomputed.TimeoutAt = pgvalue.Timestamptz(time.Now().Add(10 * time.Minute))
	recomputed.CheckpointDueAt = pgvalue.Timestamptz(time.Now().Add(time.Minute))
	if replayed, err := reconciler.RegisterWait(ctx, recomputed); err != nil || replayed.WaitID != request.WaitID {
		t.Fatalf("recomputed-deadline registration replay = %+v, %v", replayed, err)
	}
	changed := request
	changed.RequestFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := reconciler.RegisterWait(ctx, changed); !errors.Is(err, ErrWaitAuthority) ||
		err.Error() != ErrWaitAuthority.Error()+": token wait registration replay does not match" {
		t.Fatalf("changed registration replay error = %v", err)
	}
}

func TestTokenWaitRegistrationAllowsDrainingInFlightWorker(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	dbtest.MustExec(t, ctx, fixture.pool, `UPDATE worker_groups SET status = 'draining' WHERE id = $1`, request.WorkerGroupID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE worker_instances SET status = 'draining', draining_at = transaction_timestamp() WHERE id = $1
	`, request.WorkerInstanceID)
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil || registered.WaitID != request.WaitID || registered.ConditionStatus != db.WaitStatusPending {
		t.Fatalf("draining worker registration = %+v, %v", registered, err)
	}
}

func TestTokenWaitRegistrationRejectsExpiredPhysicalAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	waitID := uuid.NewV7()
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET start_deadline_at = transaction_timestamp() - interval '2 seconds',
		       expires_at = transaction_timestamp() - interval '1 second'
		 WHERE id = $1
	`, work.leaseID)
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.RegisterWait(ctx, request); !errors.Is(err, ErrWaitAuthority) {
		t.Fatalf("expired registration error = %v", err)
	}
	var status db.RunStatus
	var waitCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, (SELECT count(*) FROM run_waits WHERE id = $2)
		  FROM runs WHERE id = $1
	`, work.runID, waitID).Scan(&status, &waitCount); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusRunning || waitCount != 0 {
		t.Fatalf("expired authority mutated run=%s waits=%d", status, waitCount)
	}
}

func TestTokenWaitRegistrationAcceptsChildRun(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	parent := fixture.addWork(t, ctx, "starting", time.Now().Add(-2*time.Minute))
	child := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, child)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET cause_kind = 'child', parent_run_id = $1, parent_owns_lifecycle = false
		 WHERE id = $2
	`, parent.runID, child.runID)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	waitID := uuid.NewV7()
	request := tokenWaitRegistrationRequest(t, ctx, fixture, child, tokenID, waitID)
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil {
		t.Fatalf("register child Run Wait: %v", err)
	}
	var count int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM run_waits WHERE id = $1`, waitID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if registered.WaitID != waitID || registered.ConditionStatus != db.WaitStatusPending || count != 1 {
		t.Fatalf("child registration = %+v, waits=%d", registered, count)
	}
}

func tokenWaitRegistrationRequest(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
	tokenID uuid.UUID,
	waitID uuid.UUID,
) WaitRegistration {
	t.Helper()
	request := WaitRegistration{
		TokenID: tokenID, WaitID: waitID, ResumeAttachID: uuid.NewV7(),
		RunLeaseID:         work.leaseID,
		RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT run_leases.lease_sequence, run_leases.worker_group_id,
		       run_leases.worker_instance_id, run_leases.worker_epoch
		  FROM run_leases
		 WHERE run_leases.id = $1
	`, work.leaseID).Scan(
		&request.LeaseSequence, &request.WorkerGroupID,
		&request.WorkerInstanceID, &request.WorkerEpoch,
	); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestTokenWaitReconcilerTransitionsHotCheckpointingAndParkedWaits(t *testing.T) {
	ctx := context.Background()

	t.Run("hot completion releases without Lease churn", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStatusHot, time.Now().Add(time.Hour))
		if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
			fixture,
			setup.tokenID,
			"sha256:hot",
			`{"approved":true}`,
		)); err != nil {
			t.Fatal(err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 0 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: db.RunStatusRunning, runVersion: 3,
			conditionStatus: db.WaitStatusCompleted, suspensionStatus: db.RunWaitStatusReleased,
			currentLeaseID: pgvalue.UUID(setup.leaseID), priorLeaseID: pgtype.UUID{},
			result: `{"approved": true}`, reasonCode: "", resumeVersion: 0,
		})

		batch = reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 0 || batch.Resolved != 0 {
			t.Fatalf("replay batch = %+v", batch)
		}
	})

	t.Run("checkpointing expiry records only terminal condition", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStatusCheckpointing, time.Now().Add(-time.Minute))
		expired, err := fixture.queries.ExpireDueTokens(ctx, db.ExpireDueTokensParams{
			ControlOutboxIds: pgvalue.NewUUIDv7Batch(100),
			LimitCount:       100,
		})
		if err != nil || len(expired) != 1 {
			t.Fatalf("expire Token = rows %d error %v", len(expired), err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: db.RunStatusWaiting, runVersion: 2,
			conditionStatus: db.WaitStatusFailed, suspensionStatus: db.RunWaitStatusCheckpointing,
			currentLeaseID: pgvalue.UUID(setup.leaseID), priorLeaseID: pgtype.UUID{},
			reasonCode: "token_expired", resumeVersion: 0,
		})

		batch = reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 0 || batch.Deferred != 1 {
			t.Fatalf("deferred replay batch = %+v", batch)
		}
	})

	t.Run("parked cancellation makes the Run dispatchable", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStatusParked, time.Now().Add(time.Hour))
		if _, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, setup.tokenID)); err != nil {
			t.Fatal(err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: db.RunStatusQueued, runVersion: 3,
			conditionStatus: db.WaitStatusCancelled, suspensionStatus: db.RunWaitStatusResumePending,
			currentLeaseID: pgtype.UUID{}, priorLeaseID: pgvalue.UUID(setup.leaseID),
			reasonCode: "token_cancelled", resumeVersion: 1,
		})

		batch = reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 0 || batch.Resolved != 0 {
			t.Fatalf("replay batch = %+v", batch)
		}
	})
}

func TestTokenWaitReconcilerAppliesWaitTimeoutAcrossSuspensionStatuses(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name           string
		suspension     db.RunWaitStatus
		runStatus      db.RunStatus
		runVersion     int64
		resultState    db.RunWaitStatus
		currentLeaseID func(tokenWaitReconcileSetup) pgtype.UUID
		priorLeaseID   func(tokenWaitReconcileSetup) pgtype.UUID
		resumeVersion  int64
	}{
		{
			name: "hot", suspension: db.RunWaitStatusHot,
			runStatus: db.RunStatusRunning, runVersion: 3,
			resultState: db.RunWaitStatusReleased,
			currentLeaseID: func(setup tokenWaitReconcileSetup) pgtype.UUID {
				return pgvalue.UUID(setup.leaseID)
			},
			priorLeaseID: func(tokenWaitReconcileSetup) pgtype.UUID { return pgtype.UUID{} },
		},
		{
			name: "checkpointing", suspension: db.RunWaitStatusCheckpointing,
			runStatus: db.RunStatusWaiting, runVersion: 2,
			resultState: db.RunWaitStatusCheckpointing,
			currentLeaseID: func(setup tokenWaitReconcileSetup) pgtype.UUID {
				return pgvalue.UUID(setup.leaseID)
			},
			priorLeaseID: func(tokenWaitReconcileSetup) pgtype.UUID { return pgtype.UUID{} },
		},
		{
			name: "parked", suspension: db.RunWaitStatusParked,
			runStatus: db.RunStatusQueued, runVersion: 3,
			resultState:    db.RunWaitStatusResumePending,
			currentLeaseID: func(tokenWaitReconcileSetup) pgtype.UUID { return pgtype.UUID{} },
			priorLeaseID: func(setup tokenWaitReconcileSetup) pgtype.UUID {
				return pgvalue.UUID(setup.leaseID)
			},
			resumeVersion: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunLeaseClaimFixture(t, ctx)
			setup := newTokenWaitReconcileSetup(
				t, ctx, fixture, test.suspension, time.Now().Add(time.Hour),
			)
			dbtest.MustExec(t, ctx, fixture.pool, `
				UPDATE run_waits
				   SET timeout_at = transaction_timestamp() - interval '1 millisecond'
				 WHERE id = $1
			`, setup.waitID)
			reconciler, err := NewWaitReconciler(fixture.pool)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reconciler.ReconcileTimeouts(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			if resolved != 1 {
				t.Fatalf("resolved = %d", resolved)
			}
			assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
				runStatus: test.runStatus, runVersion: test.runVersion,
				conditionStatus: db.WaitStatusFailed, suspensionStatus: test.resultState,
				currentLeaseID: test.currentLeaseID(setup),
				priorLeaseID:   test.priorLeaseID(setup),
				reasonCode:     "wait_timeout", resumeVersion: test.resumeVersion,
			})
			resolved, err = reconciler.ReconcileTimeouts(ctx, 100)
			if err != nil || resolved != 0 {
				t.Fatalf("replay resolved = %d error=%v", resolved, err)
			}
		})
	}
}

type tokenWaitReconcileSetup struct {
	tokenID      uuid.UUID
	waitID       uuid.UUID
	runID        uuid.UUID
	workspaceID  uuid.UUID
	leaseID      uuid.UUID
	checkpointID pgtype.UUID
}

func newTokenWaitReconcileSetup(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	suspension db.RunWaitStatus,
	tokenTimeout time.Time,
) tokenWaitReconcileSetup {
	t.Helper()
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	setup := tokenWaitReconcileSetup{
		tokenID: createTokenTerminalTestToken(t, ctx, fixture, tokenTimeout),
		waitID:  uuid.NewV7(), runID: work.runID, leaseID: work.leaseID,
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, work.runID).Scan(&setup.workspaceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'running',
		       started_at = claimed_at
		 WHERE id = $1
	`, setup.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'waiting',
		       revision = 2,
		       started_at = transaction_timestamp(),
		       active_started_at = transaction_timestamp()
		 WHERE id = $1
	`, setup.runID)
	insertTokenWaitFixture(t, ctx, fixture, setup.waitID, setup.runID, setup.workspaceID, setup.tokenID, setup.leaseID, 2)

	switch suspension {
	case db.RunWaitStatusHot:
	case db.RunWaitStatusCheckpointing:
		dbtest.MustExec(t, ctx, fixture.pool, `
			UPDATE run_waits
			   SET suspension_status = 'checkpointing',
			       checkpoint_request_version = 1
			 WHERE id = $1
		`, setup.waitID)
	case db.RunWaitStatusParked:
		var workspaceLeaseID, baseWorkspaceVersionID uuid.UUID
		if err := fixture.pool.QueryRow(ctx, `
			SELECT workspace_leases.id, runs.base_workspace_version_id
			  FROM workspace_leases
			  JOIN runs ON runs.id = $1
			 WHERE workspace_leases.owner_run_lease_id = $2
		`, setup.runID, setup.leaseID).Scan(&workspaceLeaseID, &baseWorkspaceVersionID); err != nil {
			t.Fatal(err)
		}
		checkpointID := uuid.NewV7()
		setup.checkpointID = pgvalue.UUID(checkpointID)
		checkpointArtifacts := dbtest.InsertCheckpointArtifacts(t, ctx, fixture.pool, setup.runID, checkpointID.String())
		dbtest.MustExec(t, ctx, fixture.pool, `
			INSERT INTO run_checkpoints (
			    id, run_id, attempt_number, run_wait_id,
			    source_run_lease_id, source_workspace_lease_id, workspace_id,
			    base_workspace_version_id, private_workspace_version_id,
			    runtime_config_artifact_id, vm_state_artifact_id,
			    memory_artifact_id, scratch_disk_artifact_id,
			    status, manifest, ready_request_fingerprint, ready_at
			) VALUES (
			    $1, $2, 1, $3, $4, $5, $6, $7, $7,
			    $8, $9, $10, $11,
			    'ready', '{"test":true}'::jsonb, 'sha256:70ac3c8c49385651ccc368788f78f79e99cd6f3094c74f1eb89fa896cfce3863', transaction_timestamp()
			)
		`, checkpointID, setup.runID, setup.waitID, setup.leaseID, workspaceLeaseID, setup.workspaceID, baseWorkspaceVersionID,
			checkpointArtifacts.RuntimeConfig, checkpointArtifacts.VMState, checkpointArtifacts.Memory, checkpointArtifacts.ScratchDisk)
		dbtest.MustExec(t, ctx, fixture.pool, `
			UPDATE run_leases
			   SET status = 'checkpointed',
			       checkpointed_at = transaction_timestamp(),
			       terminal_at = transaction_timestamp(),
			       terminal_reason_code = 'checkpointed'
			 WHERE id = $1
		`, setup.leaseID)
		dbtest.MustExec(t, ctx, fixture.pool, `
			UPDATE workspace_leases
			   SET status = 'released',
			       released_at = transaction_timestamp(),
			       terminal_at = transaction_timestamp()
			 WHERE id = $1
		`, workspaceLeaseID)
		dbtest.MustExec(t, ctx, fixture.pool, `
			UPDATE runs
			   SET current_run_lease_id = NULL,
			       active_started_at = NULL
			 WHERE id = $1
		`, setup.runID)
		dbtest.MustExec(t, ctx, fixture.pool, `
			UPDATE run_waits
			   SET suspension_status = 'parked',
			       current_run_lease_id = NULL,
			       prior_run_lease_id = $1,
			       suspend_checkpoint_id = $2
			 WHERE id = $3
		`, setup.leaseID, checkpointID, setup.waitID)
	default:
		t.Fatalf("unsupported test suspension %s", suspension)
	}
	return setup
}

func insertTokenWaitFixture(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	waitID uuid.UUID,
	runID uuid.UUID,
	workspaceID uuid.UUID,
	tokenID uuid.UUID,
	leaseID uuid.UUID,
	expectedRunRevision int64,
) {
	t.Helper()
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, token_id,
			token_registration_run_revision, expected_run_revision,
			attempt_number, current_run_lease_id, resume_attach_id
		) VALUES ($1, $2, $3, $4, 'token', $5, $6 - 1, $6, 1, $7, $8)
	`, waitID, fixture.environmentID, runID, workspaceID, tokenID,
		expectedRunRevision, leaseID, uuid.NewV7())
}

func reconcileTokenWaitBatch(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	tokenID uuid.UUID,
) WaitBatch {
	t.Helper()
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

type tokenWaitReconcileWant struct {
	runStatus        db.RunStatus
	runVersion       int64
	conditionStatus  db.WaitStatus
	suspensionStatus db.RunWaitStatus
	currentLeaseID   pgtype.UUID
	priorLeaseID     pgtype.UUID
	result           string
	reasonCode       string
	resumeVersion    int64
}

func assertTokenWaitReconcileState(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	setup tokenWaitReconcileSetup,
	want tokenWaitReconcileWant,
) {
	t.Helper()
	var runStatus db.RunStatus
	var runVersion int64
	var runLeaseID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, revision, current_run_lease_id
		  FROM runs
		 WHERE id = $1
	`, setup.runID).Scan(&runStatus, &runVersion, &runLeaseID); err != nil {
		t.Fatal(err)
	}
	var conditionStatus db.WaitStatus
	var suspensionStatus db.RunWaitStatus
	var currentLeaseID, priorLeaseID pgtype.UUID
	var result []byte
	var reasonCode pgtype.Text
	var resumeVersion int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT condition_status, suspension_status, current_run_lease_id,
		       prior_run_lease_id, condition_result, condition_reason_code,
		       resume_request_version
		  FROM run_waits
		 WHERE id = $1
	`, setup.waitID).Scan(
		&conditionStatus,
		&suspensionStatus,
		&currentLeaseID,
		&priorLeaseID,
		&result,
		&reasonCode,
		&resumeVersion,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != want.runStatus || runVersion != want.runVersion || runLeaseID != want.currentLeaseID ||
		conditionStatus != want.conditionStatus || suspensionStatus != want.suspensionStatus ||
		currentLeaseID != want.currentLeaseID || priorLeaseID != want.priorLeaseID ||
		string(result) != want.result || reasonCode.String != want.reasonCode ||
		resumeVersion != want.resumeVersion {
		t.Fatalf("state = run %s/v%d/lease %v wait %s/%s/current %v/prior %v/result %s/reason %q/resume v%d; want %+v",
			runStatus, runVersion, runLeaseID, conditionStatus, suspensionStatus,
			currentLeaseID, priorLeaseID, result, reasonCode.String, resumeVersion, want)
	}
}

func TestTokenWaitReconcilerRejectsPendingTokenAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStatusHot, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reconciler.ReconcileBatch(ctx, fixture.environmentID, setup.tokenID, 100)
	if !errors.Is(err, ErrWaitAuthority) {
		t.Fatalf("pending Token authority error = %v", err)
	}
	var condition db.WaitStatus
	if scanErr := fixture.pool.QueryRow(ctx, `SELECT condition_status FROM run_waits WHERE id = $1`, setup.waitID).Scan(&condition); scanErr != nil {
		t.Fatal(scanErr)
	}
	if condition != db.WaitStatusPending {
		t.Fatalf("pending Token changed Wait to %s", condition)
	}
}

type runLeaseClaimFixture struct {
	pool          *pgxpool.Pool
	queries       *db.Queries
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	workerID      uuid.UUID
	base          runtest.Fixture
}

type runLeaseWork struct {
	leaseID uuid.UUID
	runID   uuid.UUID
}

type taskCompletionWork struct {
	workspaceID            uuid.UUID
	baseWorkspaceVersionID uuid.UUID
	physicalVersionID      uuid.UUID
	runtimeID              uuid.UUID
	mountID                uuid.UUID
	workspaceLeaseID       uuid.UUID
}

func newRunLeaseClaimFixture(t *testing.T, _ context.Context) runLeaseClaimFixture {
	t.Helper()
	base := runtest.New(t)
	return runLeaseClaimFixture{
		pool:          base.Pool,
		queries:       db.New(base.Pool),
		orgID:         base.OrgID,
		projectID:     base.ProjectID,
		environmentID: base.EnvironmentID,
		deploymentID:  base.DeploymentID,
		workerID:      base.WorkerID,
		base:          base,
	}
}

func (fixture runLeaseClaimFixture) addWork(
	t *testing.T,
	_ context.Context,
	state string,
	assignedAt time.Time,
) runLeaseWork {
	t.Helper()
	work := fixture.base.AddRunLease(t, state, assignedAt)
	return runLeaseWork{leaseID: work.LeaseID, runID: work.RunID}
}

func startTaskCompletionWork(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
) taskCompletionWork {
	t.Helper()
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'running', started_at = claimed_at
		 WHERE id = $1 AND status = 'starting'
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'running', revision = revision + 1,
		       started_at = (SELECT started_at FROM run_leases WHERE id = $1),
		       active_started_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE id = $2 AND status = 'queued' AND current_run_lease_id = $1
	`, work.leaseID, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_attempts
		   SET entrypoint_entered_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE run_id = $2 AND number = 1 AND entrypoint_entered_at IS NULL
	`, work.leaseID, work.runID)
	var authority taskCompletionWork
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, runs.base_workspace_version_id,
		       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
		       workspace_leases.id, workspace_leases.base_workspace_version_id
		  FROM runs
		  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE runs.id = $1
	`, work.runID).Scan(
		&authority.workspaceID,
		&authority.baseWorkspaceVersionID,
		&authority.runtimeID,
		&authority.mountID,
		&authority.workspaceLeaseID,
		&authority.physicalVersionID,
	); err != nil {
		t.Fatal(err)
	}
	return authority
}
