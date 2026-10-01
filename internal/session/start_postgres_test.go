package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

func startRequest(f sessiontest.Fixture, index int, key *string) StartRequest {
	return StartRequest{
		OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID,
		ActorDeclaredID: sessiontest.ActorDeclaredID, ComputerID: f.ComputerIDs[index], Key: key,
	}
}

// startClaim is the idempotency claim of a start under the key, or none for
// an empty key.
func startClaim(t *testing.T, request StartRequest, key string) idempotency.Request {
	t.Helper()
	if key == "" {
		return nil
	}
	address, err := json.Marshal(map[string]string{"id": request.ComputerID.String()})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := idempotency.NewActorStartRequest(request.EnvironmentID, request.ActorDeclaredID, key, idempotency.ActorStartFingerprint{
		Key: request.Key, ComputerAddress: address, ManagedQueueName: request.QueueName,
		ManagedConcurrencyKey: request.ConcurrencyKey, ManagedPriority: request.Priority,
		ManagedQueuedTTLMS: request.QueuedTTLMS, ManagedRetryPolicy: request.RetryPolicy,
		ManagedRunMetadata: request.Metadata, ManagedRunTags: request.Tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func TestStartCommitsReplaysAndRejectsConflicts(t *testing.T) {
	f := sessiontest.New(t, 2)
	key := "thread:42"
	request := startRequest(f, 0, &key)
	request.Metadata = json.RawMessage(`{"kind":"boot"}`)
	request.Tags = []string{"managed"}
	claim := startClaim(t, request, "start-1")

	created, err := Start(t.Context(), f.Pool, claim, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed {
		t.Fatalf("created = %+v", created)
	}
	limit := int64(2)
	assertStartTuple(t, f, created, "default", &limit)

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status = 'closing' WHERE id = $1`, created.SessionID)
	replayed, err := Start(t.Context(), f.Pool, claim, request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.SessionID != created.SessionID || replayed.BootRunID != created.BootRunID {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id = NULL WHERE id = $1`, f.EnvironmentID)
	undeployed, err := Start(t.Context(), f.Pool, claim, request)
	if err != nil {
		t.Fatalf("replay after undeploy: %v", err)
	}
	if !undeployed.Replayed || undeployed.SessionID != created.SessionID {
		t.Fatalf("undeployed replay = %+v, created = %+v", undeployed, created)
	}
	if _, err := Start(t.Context(), f.Pool, nil, startRequest(f, 1, nil)); !errors.Is(err, ErrActorNotDeployed) {
		t.Fatalf("undeployed start = %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id = $1 WHERE id = $2`, f.DeploymentID, f.EnvironmentID)

	changed := request
	changed.Metadata = json.RawMessage(`{"kind":"different"}`)
	var idempotencyConflict idempotency.ConflictError
	if _, err := Start(t.Context(), f.Pool, startClaim(t, changed, "start-1"), changed); !errors.As(err, &idempotencyConflict) {
		t.Fatalf("fingerprint conflict = %v", err)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET dirty_state = 'dirty' WHERE id = $1`, f.ComputerIDs[1])
	collision := startRequest(f, 1, &key)
	_, err = Start(t.Context(), f.Pool, startClaim(t, collision, "start-2"), collision)
	var keyConflict KeyConflictError
	if !errors.As(err, &keyConflict) || keyConflict.Key != key {
		t.Fatalf("Actor key conflict = %v", err)
	}
	var claims, sessions, runs int
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM sessions),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start')
	`).Scan(&claims, &sessions, &runs); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || sessions != 1 || runs != 1 {
		t.Fatalf("counts after conflicts = claims %d sessions %d runs %d", claims, sessions, runs)
	}
}

func TestStartUsesSelectedQueueAndEmptyBoot(t *testing.T) {
	f := sessiontest.New(t, 1)
	key := "no-input"
	request := startRequest(f, 0, &key)
	request.QueueName = "priority"
	created, err := Start(t.Context(), f.Pool, startClaim(t, request, "no-input-1"), request)
	if err != nil {
		t.Fatal(err)
	}
	assertStartTuple(t, f, created, "priority", nil)
}

func TestStartKeylessRequestsRemainAtLeastOnce(t *testing.T) {
	f := sessiontest.New(t, 2)
	for index := range 2 {
		if _, err := Start(t.Context(), f.Pool, nil, startRequest(f, index, nil)); err != nil {
			t.Fatal(err)
		}
	}
	var claims, sessions, runs, owned int
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM sessions),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM computers c WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.computer_id=c.id))
	`).Scan(&claims, &sessions, &runs, &owned); err != nil {
		t.Fatal(err)
	}
	if claims != 0 || sessions != 2 || runs != 2 || owned != 2 {
		t.Fatalf("keyless counts claims=%d sessions=%d runs=%d owned=%d", claims, sessions, runs, owned)
	}
}

func TestStartConcurrentKeyCollisionCreatesOneIdentity(t *testing.T) {
	f := sessiontest.New(t, 2)
	key := "shared-key"
	type outcome struct {
		started Started
		err     error
	}
	begin := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for index := range 2 {
		go func() {
			<-begin
			request := startRequest(f, index, &key)
			started, err := Start(context.Background(), f.Pool, startClaim(t, request, fmt.Sprintf("race-%d", index)), request)
			outcomes <- outcome{started: started, err: err}
		}()
	}
	close(begin)
	var successes, conflicts int
	for range 2 {
		value := <-outcomes
		var conflict KeyConflictError
		switch {
		case value.err == nil:
			successes++
		case errors.As(value.err, &conflict):
			conflicts++
		default:
			t.Fatalf("race error = %v", value.err)
		}
	}
	var claims, sessions, runs, owned int
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM sessions),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM computers c WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.computer_id=c.id))
	`).Scan(&claims, &sessions, &runs, &owned); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || claims != 1 || sessions != 1 || runs != 1 || owned != 1 {
		t.Fatalf("race successes=%d conflicts=%d claims=%d sessions=%d runs=%d owned=%d",
			successes, conflicts, claims, sessions, runs, owned)
	}
}

// An admission holding the environment and then the Computer makes a start
// wait for the environment, rather than hold the environment while waiting
// for that Computer.
func TestStartWaitsForEnvironmentBeforeComputer(t *testing.T) {
	f := sessiontest.New(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT id FROM computers WHERE id=$1 FOR UPDATE`, f.ComputerIDs[0]); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		request := startRequest(f, 0, nil)
		_, err := Start(ctx, f.Pool, startClaim(t, request, "public-admission"), request)
		done <- err
	}()
	for {
		var waiting bool
		err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%LockActorStartDeploymentAuthority%')`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("start did not serialize: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func assertStartTuple(t *testing.T, f sessiontest.Fixture, started Started, wantQueue string, wantQueueLimit *int64) {
	t.Helper()
	var currentRun, sessionComputer, runSession uuid.UUID
	var nextInput, committed, maxDuration, runStart, runHigh, attemptStart int64
	var queue, cause, claimStatus string
	var queueLimit *int64
	var retry []byte
	var turns, resolutions int
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT current_run_id, computer_id, next_input_sequence, committed_input_sequence,
		       run_queue_name, run_queue_concurrency_limit, run_max_active_duration_ms, run_retry_policy
		  FROM sessions
		 WHERE id = $1
	`, started.SessionID).Scan(&currentRun, &sessionComputer, &nextInput, &committed, &queue, &queueLimit, &maxDuration, &retry); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT session_id, cause_kind, session_input_start_sequence, session_input_high_watermark
		  FROM runs
		 WHERE id = $1
	`, started.BootRunID).Scan(&runSession, &cause, &runStart, &runHigh); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT session_input_start_sequence FROM run_attempts WHERE run_id = $1 AND number = 1
	`, started.BootRunID).Scan(&attemptStart); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM session_turns WHERE session_id = $1),
		       (SELECT status FROM idempotency_claims WHERE operation = 'actor.start'),
		       (SELECT count(*) FROM secret_resolutions WHERE run_id = $2 AND attempt_number = 1)
	`, started.SessionID, started.BootRunID).Scan(&turns, &claimStatus, &resolutions); err != nil {
		t.Fatal(err)
	}
	queueLimitValid := (wantQueueLimit == nil && queueLimit == nil) ||
		(wantQueueLimit != nil && queueLimit != nil && *wantQueueLimit == *queueLimit)
	if currentRun != started.BootRunID || sessionComputer != f.ComputerIDs[0] ||
		runSession != started.SessionID || cause != "actor_start" ||
		runStart != 0 || runHigh != 0 || attemptStart != 0 || nextInput != 1 || committed != 0 ||
		queue != wantQueue || !queueLimitValid || maxDuration != 300_000 ||
		string(retry) != `{"enabled": false}` || turns != 0 || claimStatus != "completed" || resolutions != 1 {
		t.Fatalf(
			"start tuple run=%s computer=%s next=%d committed=%d queue=%s/%v max=%d retry=%s runSession=%s cause=%s cursor=%d high=%d attempt=%d turns=%d claim=%s resolutions=%d",
			currentRun, sessionComputer, nextInput, committed, queue, queueLimit, maxDuration, retry,
			runSession, cause, runStart, runHigh, attemptStart, turns, claimStatus, resolutions,
		)
	}
}

// runSourcedStart is a live, entered source Task Run whose environment's
// current deployment also declares the Actor starter.
type runSourcedStart struct {
	runtest.Fixture
	work     runtest.RunLease
	fence    run.ExecutionFence
	computer uuid.UUID
}

func newRunSourcedStart(t *testing.T) runSourcedStart {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	transact := func(fn func(pgx.Tx) error) {
		t.Helper()
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if err = fn(tx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	transact(func(tx pgx.Tx) error { _, err := run.ClaimExecution(t.Context(), tx, fence); return err })
	transact(func(tx pgx.Tx) error { _, err := run.StartExecution(t.Context(), tx, fence); return err })
	transact(func(tx pgx.Tx) error { return run.EnterExecution(t.Context(), tx, fence, "task", "test-task") })
	manifest := []byte(`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := definition.CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET queue_config = '{"formatVersion":0,"queues":[{"name":"default"}]}'::jsonb WHERE id = $1`, f.DeploymentID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id = $1 WHERE id = $2`, f.DeploymentID, f.EnvironmentID)
	dbtest.MustExec(t, t.Context(), f.Pool, `
		INSERT INTO deployment_definitions (id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest)
		VALUES ($1, $2, $3, 'actor', 'starter', 0, $4::jsonb, $5)
	`, uuid.NewV7(), f.EnvironmentID, f.DeploymentID, manifest, digest[:])
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id = $1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	return runSourcedStart{Fixture: f, work: work, fence: fence, computer: computerID}
}

func (s runSourcedStart) request(computerID uuid.UUID) StartRequest {
	return StartRequest{OrgID: s.OrgID, ProjectID: s.ProjectID, EnvironmentID: s.EnvironmentID, ActorDeclaredID: "starter", ComputerID: computerID}
}

// A run-sourced start authorizes its live source with the start Computer
// addressed, both when it admits a new Session and when it replays one.
func TestStartFromRunAuthorizesTheSource(t *testing.T) {
	s := newRunSourcedStart(t)
	request := s.request(s.computer)
	claim := startClaim(t, request, "run-sourced")
	created, err := StartFromRun(t.Context(), s.Pool, s.fence, claim, request)
	if err != nil {
		t.Fatal(err)
	}
	var sessionComputer uuid.UUID
	if err := s.Pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id = $1 AND current_run_id = $2`, created.SessionID, created.BootRunID).Scan(&sessionComputer); err != nil || sessionComputer != s.computer {
		t.Fatalf("run-sourced Session Computer = %s, %v", sessionComputer, err)
	}
	replayed, err := StartFromRun(t.Context(), s.Pool, s.fence, claim, request)
	if err != nil || !replayed.Replayed || replayed.SessionID != created.SessionID {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	stale := s.fence
	stale.LeaseSequence++
	if _, err := StartFromRun(t.Context(), s.Pool, stale, claim, request); !errors.Is(err, run.ErrStaleSource) {
		t.Fatalf("stale source replay = %v", err)
	}
	if _, err := StartFromRun(t.Context(), s.Pool, stale, nil, request); !errors.Is(err, run.ErrStaleSource) {
		t.Fatalf("stale source start = %v", err)
	}
	if _, err := StartFromRun(t.Context(), s.Pool, s.fence, nil, s.request(uuid.NewV7())); !errors.Is(err, ErrStartComputerNotFound) {
		t.Fatalf("unaddressable start Computer = %v", err)
	}
	var sessions int
	if err := s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions = %d, %v", sessions, err)
	}
}
