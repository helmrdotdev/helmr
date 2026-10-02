package token

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgconn"
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
	registrar := newTestRegistrar(t, fixture.pool)
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
	registered, err := registrar.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ConditionStatus != db.WaitStatusCompleted || registered.SuspensionStatus != db.RunWaitStatusReleased ||
		registered.RunRevision != expectedRunVersion+2 || string(registered.Result) != `{"approved": true}` {
		t.Fatalf("registration = %+v", registered)
	}
	replayed, err := registrar.RegisterWait(ctx, registration)
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
		t.Fatalf("durable registration = run %s/%d condition %s suspension %s computer %s", runStatus, runVersion, condition, suspension, authority.computerID)
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
	registrar := newTestRegistrar(t, fixture.pool)
	waitID := uuid.NewV7()
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	registered, err := registrar.RegisterWait(ctx, registration)
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
	registrar := newTestRegistrar(t, fixture.pool)
	waitID := uuid.NewV7()
	if _, err := registrar.RegisterWait(ctx, tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)); err != nil {
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
	registrar := newTestRegistrar(t, fixture.pool)
	for _, work := range []runLeaseWork{first, second} {
		request := tokenWaitRegistrationRequest(
			t,
			ctx,
			fixture,
			work,
			tokenID,
			uuid.NewV7(),
		)
		registered, err := registrar.RegisterWait(ctx, request)
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
	var computerID uuid.UUID
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT computer_id FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO run_waits (
		    id, environment_id, run_id, computer_id, kind, token_id,
		    token_registration_run_revision, expected_run_revision,
		    attempt_number, current_run_lease_id
		) VALUES ($1, $2, $3, $4, 'token', $5, 0, 1, 1, $6)
	`, uuid.NewV7(), fixture.environmentID, work.runID, computerID,
		otherTokenID, work.leaseID); err != nil {
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23503" || pgerr.ConstraintName != "run_waits_environment_id_token_id_fkey" {
			t.Fatalf("cross-Environment Token FK error=%v", err)
		}
	} else {
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
	registrar, err := NewRegistrar(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	if actor {
		registration.SessionSpeculativeInputSequence = pgtype.Int8{Int64: 1, Valid: true}
	}
	registered, err := registrar.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	cp := captureTokenWait(t, fixture, work, true)
	var exact bool
	if err := fixture.pool.QueryRow(ctx, `SELECT r.status='waiting' AND r.current_run_lease_id IS NULL AND l.status='checkpointed' AND l.process_reconciled_at IS NULL AND w.suspension_status='parked' AND w.prior_run_lease_id=l.id AND w.current_run_lease_id IS NULL AND cp.status='ready' AND i.desired_state='closed' AND i.reclaimed_at IS NULL AND c.head_disk_version_id=cp.base_computer_disk_version_id AND cp.private_computer_disk_version_id<>c.head_disk_version_id FROM runs r JOIN run_leases l ON l.id=$2 JOIN run_waits w ON w.id=$3 JOIN computer_checkpoints cp ON cp.id=$4 JOIN computer_instances i ON i.id=cp.source_computer_instance_id JOIN computers c ON c.id=r.computer_id WHERE r.id=$1`, work.runID, work.leaseID, registered.WaitID, cp.ID).Scan(&exact); err != nil || !exact {
		t.Fatalf("atomic token parking=%v err=%v computer=%s", exact, err, authority.computerID)
	}
}

func TestTokenWaitRegistrationConcurrentReplayConverges(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	registrar, err := NewRegistrar(fixture.pool)
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
			result, err := registrar.RegisterWait(ctx, request)
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
	registrar := newTestRegistrar(t, fixture.pool)
	registered, err := registrar.RegisterWait(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	captureTokenWait(t, fixture, work, true)
	if _, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, tokenID)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Resolved != 1 {
		t.Fatalf("parked completion = %+v, %v", batch, err)
	}
	replayed, err := registrar.RegisterWait(ctx, request)
	if err != nil || replayed.WaitID != request.WaitID || replayed.ConditionStatus != db.WaitStatusCancelled ||
		replayed.SuspensionStatus != db.RunWaitStatusResumePending ||
		replayed.ReasonCode != "token_cancelled" ||
		replayed.RunRevision != registered.RunRevision+1 {
		t.Fatalf("parked registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	recomputed := request
	recomputed.TimeoutAt = pgvalue.Timestamptz(time.Now().Add(10 * time.Minute))
	if replayed, err := registrar.RegisterWait(ctx, recomputed); err != nil || replayed.WaitID != request.WaitID {
		t.Fatalf("recomputed-deadline registration replay = %+v, %v", replayed, err)
	}
	changed := request
	changed.RequestFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := registrar.RegisterWait(ctx, changed); !errors.Is(err, ErrWaitAuthority) ||
		err.Error() != ErrWaitAuthority.Error()+": token wait registration replay does not match" {
		t.Fatalf("changed registration replay error = %v", err)
	}
}

// Draining and paused Groups stop admission only; started Runs still wait.
func TestTokenWaitRegistrationAllowsInFlightWorkerOnNonAdmittingGroup(t *testing.T) {
	for _, status := range []string{"draining", "paused"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunLeaseClaimFixture(t, ctx)
			work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			startTaskCompletionWork(t, ctx, fixture, work)
			tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
			request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
			dbtest.MustExec(t, ctx, fixture.pool, `UPDATE worker_groups SET status = $2 WHERE id = $1`, request.WorkerGroupID, status)
			dbtest.MustExec(t, ctx, fixture.pool, `
				UPDATE worker_hosts SET status = 'draining', draining_at = transaction_timestamp(), drain_reason = 'shutdown' WHERE id = $1
			`, request.WorkerHostID)
			registrar, err := NewRegistrar(fixture.pool)
			if err != nil {
				t.Fatal(err)
			}
			registered, err := registrar.RegisterWait(ctx, request)
			if err != nil || registered.WaitID != request.WaitID || registered.ConditionStatus != db.WaitStatusPending {
				t.Fatalf("%s Group registration = %+v, %v", status, registered, err)
			}
		})
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
	registrar, err := NewRegistrar(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registrar.RegisterWait(ctx, request); !errors.Is(err, ErrWaitAuthority) {
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
	registrar, err := NewRegistrar(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := registrar.RegisterWait(ctx, request)
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
		TokenID: tokenID, WaitID: waitID,
		RunLeaseID:         work.leaseID,
		RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT run_leases.lease_sequence, run_leases.worker_group_id,
		       run_leases.worker_host_id, run_leases.worker_epoch
		  FROM run_leases
		 WHERE run_leases.id = $1
	`, work.leaseID).Scan(
		&request.LeaseSequence, &request.WorkerGroupID,
		&request.WorkerHostID, &request.WorkerEpoch,
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
			result: `{"approved": true}`, reasonCode: "",
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
			reasonCode: "token_expired",
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
			reasonCode: "token_cancelled",
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
				reasonCode:     "wait_timeout",
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
	computerID   uuid.UUID
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
	if err := fixture.pool.QueryRow(ctx, `SELECT computer_id FROM runs WHERE id = $1`, work.runID).Scan(&setup.computerID); err != nil {
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
	insertTokenWaitFixture(t, ctx, fixture, setup.waitID, setup.runID, setup.computerID, setup.tokenID, setup.leaseID, 2)

	dbtest.MustExec(t, ctx, fixture.pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.runID)
	switch suspension {
	case db.RunWaitStatusHot:
	case db.RunWaitStatusCheckpointing, db.RunWaitStatusParked:
		cp := captureTokenWait(t, fixture, work, suspension == db.RunWaitStatusParked)
		setup.checkpointID = cp.ID
	default:
		t.Fatalf("unsupported suspension %s", suspension)
	}
	return setup
}

func insertTokenWaitFixture(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	waitID uuid.UUID,
	runID uuid.UUID,
	computerID uuid.UUID,
	tokenID uuid.UUID,
	leaseID uuid.UUID,
	expectedRunRevision int64,
) {
	t.Helper()
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO run_waits (
			id, environment_id, run_id, computer_id, kind, token_id,
			token_registration_run_revision, expected_run_revision,
			attempt_number, current_run_lease_id
		) VALUES ($1, $2, $3, $4, 'token', $5, $6 - 1, $6, 1, $7)
	`, waitID, fixture.environmentID, runID, computerID, tokenID,
		expectedRunRevision, leaseID)
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
	if err := fixture.pool.QueryRow(ctx, `
		SELECT condition_status, suspension_status, current_run_lease_id,
		       prior_run_lease_id, condition_result, condition_reason_code
		  FROM run_waits
		 WHERE id = $1
	`, setup.waitID).Scan(
		&conditionStatus,
		&suspensionStatus,
		&currentLeaseID,
		&priorLeaseID,
		&result,
		&reasonCode,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != want.runStatus || runVersion != want.runVersion || runLeaseID != want.currentLeaseID ||
		conditionStatus != want.conditionStatus || suspensionStatus != want.suspensionStatus ||
		currentLeaseID != want.currentLeaseID || priorLeaseID != want.priorLeaseID ||
		string(result) != want.result || reasonCode.String != want.reasonCode {
		t.Fatalf("state = run %s/v%d/lease %v wait %s/%s/current %v/prior %v/result %s/reason %q; want %+v",
			runStatus, runVersion, runLeaseID, conditionStatus, suspensionStatus,
			currentLeaseID, priorLeaseID, result, reasonCode.String, want)
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

type taskCompletionWork struct{ computerID uuid.UUID }

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
	if err := fixture.pool.QueryRow(ctx, `SELECT computer_id FROM runs WHERE id=$1`, work.runID).Scan(&authority.computerID); err != nil {
		t.Fatal(err)
	}
	return authority
}

func captureTokenWait(t *testing.T, f runLeaseClaimFixture, work runLeaseWork, park bool) db.ComputerCheckpoint {
	t.Helper()
	capture := computer.Capture{CheckpointID: uuid.NewV7(), EnvironmentID: f.environmentID}
	if err := f.pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,i.membership_revision,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.leaseID).Scan(&capture.InstanceID, &capture.WriterGeneration, &capture.MembershipRevision, &capture.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	cp, err := computer.BeginCapture(t.Context(), tx, capture)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !park {
		return cp
	}
	ref, manifest := computertest.CaptureRequest(t, f.base, cp)
	return computertest.Complete(t, f.base, ref, manifest, computertest.PrepareCapture(t, f.base, ref, manifest))
}

func newTestRegistrar(t *testing.T, txb db.TxBeginner) *Registrar {
	t.Helper()
	registrar, err := NewRegistrar(txb)
	if err != nil {
		t.Fatal(err)
	}
	return registrar
}
