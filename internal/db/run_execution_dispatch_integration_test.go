package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedReadyRestoreCheckpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, workerInstanceID pgtype.UUID) pgtype.UUID {
	t.Helper()
	sessionID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
	INSERT INTO run_execution_sessions (
	    id,
	    org_id,
	    run_id,
		    attempt_id,
	    worker_instance_id,
	    worker_group_id,
	    dispatch_message_id,
	    dispatch_lease_id,
	    dispatch_attempt,
	    status,
	    lease_expires_at,
	    runtime_id,
	    active_duration_ms,
	    trace_id,
	    span_id,
	    parent_span_id,
	    traceparent,
	    released_at
		)
	SELECT $1,
	       $2,
	       $3,
	       runs.current_attempt_id,
	       $4,
	       (SELECT worker_group_id FROM worker_instances WHERE id = $4),
	       'previous-message',
	       'previous-lease',
	       1,
	       'detached',
	       now() + interval '1 minute',
	       'sha256:runtime',
	       100,
	       runs.trace_id,
	       'fedcba9876543210',
	       runs.root_span_id,
	       '00-' || runs.trace_id || '-fedcba9876543210-01',
	       now()
	  FROM runs
	 WHERE runs.org_id = $2
	   AND runs.id = $3
	`, sessionID, orgID, runID, workerInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoints (
	    id,
	    org_id,
	    run_id,
	    project_id,
	    environment_id,
	    session_id,
	    status,
	    reason,
	    manifest,
	    ready_at
	)
	SELECT $1::uuid,
	       runs.org_id,
	       runs.id,
	       runs.project_id,
	       runs.environment_id,
	       $4,
	       'ready',
	       'waitpoint',
	       '{"runtime":{"backend":"firecracker"}}',
	       now()
	  FROM runs
	 WHERE runs.org_id = $2
	   AND runs.id = $3
	`, checkpointID, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}
	runtimeConfigArtifactID := ids.ToPG(ids.New())
	workspaceArtifactID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
	INSERT INTO cas_objects (digest, size_bytes, media_type)
	VALUES
	    ('sha256:runtime-config', 1, 'application/vnd.helmr.checkpoint.runtime-config.v0+json'),
	    ($1, 1, 'application/vnd.helmr.workspace.v0.tar')
	ON CONFLICT (digest) DO NOTHING
	`, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
	SELECT $1,
	       runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       'sha256:runtime-config',
	       'checkpoint_runtime_config'::artifact_kind,
	       1,
	       'application/vnd.helmr.checkpoint.runtime-config.v0+json'
	  FROM runs
	 WHERE runs.org_id = $3
	   AND runs.id = $4
	UNION ALL
	SELECT $2::uuid,
	       runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       $5,
	       'checkpoint_workspace'::artifact_kind,
	       1,
	       'application/vnd.helmr.workspace.v0.tar'
	  FROM runs
	 WHERE runs.org_id = $3
	   AND runs.id = $4
	`, runtimeConfigArtifactID, workspaceArtifactID, orgID, runID, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_runtime_snapshots (
	    org_id,
	    project_id,
	    environment_id,
	    run_id,
	    checkpoint_id,
	    runtime_backend,
	    runtime_id,
	    runtime_arch,
	    runtime_abi,
	    kernel_digest,
	    initramfs_digest,
	    rootfs_digest,
	    cni_profile,
	    runtime_config_artifact_id
	)
	SELECT runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       runs.id,
	       $3,
	       'firecracker',
	       'sha256:runtime',
	       'x86_64',
	       'helmr.firecracker.snapshot.v0',
	       'sha256:kernel',
	       'sha256:initramfs',
	       'sha256:rootfs',
	       'helmr/v0',
	       $4
	  FROM runs
	 WHERE runs.org_id = $1
	   AND runs.id = $2
	`, orgID, runID, checkpointID, runtimeConfigArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_workspace_snapshots (
	    org_id,
	    project_id,
	    environment_id,
	    run_id,
	    checkpoint_id,
	    workspace_artifact_id,
		    workspace_artifact_encoding,
		    workspace_mount_path,
		    workspace_volume_kind
		)
	SELECT runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       runs.id,
	       $3,
	       $4,
	       'tar',
	       '/workspace',
	       'copy-on-write'
	  FROM runs
	 WHERE runs.org_id = $1
	   AND runs.id = $2
		`, orgID, runID, checkpointID, workspaceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	WITH run_scope AS (
	    SELECT org_id, id AS run_id, project_id, environment_id
	      FROM runs
	     WHERE org_id = $2
	       AND id = $3
	),
	waitpoint AS (
	    INSERT INTO waitpoints (
	        id,
	        org_id,
	        project_id,
	        environment_id,
	        kind,
	        request,
	        display_text,
	        status,
	        resolution_kind,
	        output,
	        completed_at
	    )
	    SELECT $1,
	           run_scope.org_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           'human',
	           '{}',
	           'approve',
	           'completed',
	           'completed',
	           '{"value":{"approved":true}}',
	           now()
	      FROM run_scope
	    RETURNING *
	),
	run_wait AS (
	    INSERT INTO run_waits (
	        id,
	        org_id,
	        run_id,
	        project_id,
	        environment_id,
	        session_id,
	        checkpoint_id,
	        correlation_id,
	        status,
	        resolution_kind,
	        resolution,
	        waiting_at,
	        resolved_at
	    )
	    SELECT $6,
	           run_scope.org_id,
	           run_scope.run_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           $4,
	           $5,
	           'restore-waitpoint',
	           'resuming',
	           'completed',
	           '{"value":{"approved":true}}',
	           now(),
	           now()
	      FROM waitpoint
	      JOIN run_scope ON true
	    RETURNING *
	)
	INSERT INTO run_wait_dependencies (
	    org_id,
	    run_id,
	    project_id,
	    environment_id,
	    run_wait_id,
	    waitpoint_id
	)
	SELECT run_wait.org_id,
	       run_wait.run_id,
	       run_wait.project_id,
	       run_wait.environment_id,
	       run_wait.id,
	       waitpoint.id
	  FROM run_wait
	  JOIN waitpoint ON true
	`, waitpointID, orgID, runID, sessionID, checkpointID, runWaitID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET latest_checkpoint_id = $1
	 WHERE org_id = $2
	   AND id = $3
	`, checkpointID, orgID, runID); err != nil {
		t.Fatal(err)
	}
	return checkpointID
}

func requireCheckpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID, want db.CheckpointStatus) {
	t.Helper()
	var got db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
	SELECT status
	  FROM checkpoints
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, checkpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("checkpoint status = %s, want %s", got, want)
	}
}

func requireRuntimeConfigArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) {
	t.Helper()
	var artifactID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT runtime_config_artifact_id
  FROM checkpoint_runtime_snapshots
 WHERE org_id = $1
   AND run_id = $2
   AND checkpoint_id = $3
`, orgID, runID, checkpointID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	if !artifactID.Valid {
		t.Fatal("runtime_config_artifact_id is null")
	}
}

func requireNoCheckpointArtifacts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM checkpoint_artifacts
 WHERE org_id = $1
   AND run_id = $2
   AND checkpoint_id = $3
`, orgID, runID, checkpointID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("checkpoint artifact rows = %d, want 0", count)
	}
}

func requireWaitpointForCheckpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	var runWaitID pgtype.UUID
	var waitpointID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT run_waits.id, run_wait_dependencies.waitpoint_id
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_waits.checkpoint_id = $3
	`, orgID, runID, checkpointID).Scan(&runWaitID, &waitpointID); err != nil {
		t.Fatal(err)
	}
	return runWaitID, waitpointID
}

func requireWaitpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want db.RunWaitStatus) {
	t.Helper()
	var got db.RunWaitStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_wait_dependencies.waitpoint_id = $3
`, orgID, runID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint status = %s, want %s", got, want)
	}
}

func requireWaitpointConditionStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, waitpointID pgtype.UUID, want db.WaitpointStatus) {
	t.Helper()
	var got db.WaitpointStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM waitpoints
 WHERE org_id = $1
   AND id = $2
`, orgID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint condition status = %s, want %s", got, want)
	}
}

func requireRunStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want db.RunStatus) {
	t.Helper()
	var got db.RunStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run status = %s, want %s", got, want)
	}
}

func requireRunStateVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID) int64 {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
SELECT state_version
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func requireCurrentRunAttemptStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want db.RunAttemptStatus) {
	t.Helper()
	var got db.RunAttemptStatus
	if err := pool.QueryRow(ctx, `
SELECT run_attempts.status
  FROM runs
  JOIN run_attempts ON run_attempts.org_id = runs.org_id
                   AND run_attempts.run_id = runs.id
                   AND run_attempts.id = runs.current_attempt_id
 WHERE runs.org_id = $1
   AND runs.id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("current attempt status = %s, want %s", got, want)
	}
}

func requireRunRetryDecision(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, wantDecision db.RunRetryDecisionKind, wantReason string, wantNextAttempt int32) {
	t.Helper()
	var decision db.RunRetryDecisionKind
	var reason string
	var retryAfter pgtype.Timestamptz
	var nextAttempt pgtype.Int4
	if err := pool.QueryRow(ctx, `
SELECT decision,
       reason,
       retry_after,
       next_attempt_number
  FROM run_retry_decisions
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
`, orgID, runID, sessionID).Scan(&decision, &reason, &retryAfter, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if decision != wantDecision || reason != wantReason {
		t.Fatalf("retry decision = %s/%q, want %s/%q", decision, reason, wantDecision, wantReason)
	}
	if !retryAfter.Valid || !retryAfter.Time.After(time.Now()) {
		t.Fatalf("retry_after = %+v, want future timestamp", retryAfter)
	}
	if !nextAttempt.Valid || nextAttempt.Int32 != wantNextAttempt {
		t.Fatalf("next_attempt_number = %+v, want %d", nextAttempt, wantNextAttempt)
	}
}

func requireRunUsageEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, wantCount int, wantQuantity int64) {
	t.Helper()
	var gotCount int
	var gotQuantity int64
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int,
       COALESCE(sum(quantity), 0)::bigint
  FROM run_usage_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&gotCount, &gotQuantity); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount || gotQuantity != wantQuantity {
		t.Fatalf("usage %s count/quantity = %d/%d, want %d/%d", kind, gotCount, gotQuantity, wantCount, wantQuantity)
	}
}

func requireRunUsageEventPositive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, wantCount int) {
	t.Helper()
	var gotCount int
	var gotQuantity int64
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int,
       COALESCE(sum(quantity), 0)::bigint
  FROM run_usage_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&gotCount, &gotQuantity); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount || gotQuantity <= 0 {
		t.Fatalf("usage %s count/quantity = %d/%d, want %d/positive", kind, gotCount, gotQuantity, wantCount)
	}
}

func requireRunUsageEventAtLeast(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, wantCount int, minQuantity int64) {
	t.Helper()
	var gotCount int
	var gotQuantity int64
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int,
       COALESCE(sum(quantity), 0)::bigint
  FROM run_usage_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&gotCount, &gotQuantity); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount || gotQuantity < minQuantity {
		t.Fatalf("usage %s count/quantity = %d/%d, want %d/>=%d", kind, gotCount, gotQuantity, wantCount, minQuantity)
	}
}

func requireRunUsageDuration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
SELECT usage_duration_ms
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run usage_duration_ms = %d, want %d", got, want)
	}
}

func requireRunUsageEventSnapshotTransition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind, wantTransition string) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_usage_events
  JOIN run_snapshots
    ON run_snapshots.org_id = run_usage_events.org_id
   AND run_snapshots.run_id = run_usage_events.run_id
   AND run_snapshots.version = run_usage_events.snapshot_version
 WHERE run_usage_events.org_id = $1
   AND run_usage_events.run_id = $2
   AND run_usage_events.kind = $3
   AND run_snapshots.transition = $4
`, orgID, runID, kind, wantTransition).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Fatalf("usage %s has no snapshot transition %q", kind, wantTransition)
	}
}

func requireRunExecutionSessionActiveDuration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
SELECT active_duration_ms
  FROM run_execution_sessions
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("session active_duration_ms = %d, want %d", got, want)
	}
}

func requireRunExecutionSessionStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, want db.RunExecutionSessionStatus) {
	t.Helper()
	var got db.RunExecutionSessionStatus
	var lostAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
SELECT status, lost_at
  FROM run_execution_sessions
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID).Scan(&got, &lostAt); err != nil {
		t.Fatal(err)
	}
	if got != want || (want == db.RunExecutionSessionStatusLost && !lostAt.Valid) {
		t.Fatalf("run execution status = %s lost_at = %+v, want %s", got, lostAt, want)
	}
}

func requireNoActiveConcurrencySlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_queue_concurrency_leases
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
   AND released_at IS NULL
`, orgID, runID, sessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active concurrency slots = %d, want 0", count)
	}
}

func requireActiveConcurrencySlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_queue_concurrency_leases
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
   AND released_at IS NULL
`, orgID, runID, sessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active concurrency slots = %d, want 1", count)
	}
}

func requireWaitpointResponseCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM waitpoint_responses
 WHERE org_id = $1
   AND waitpoint_id = $2
`, orgID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint response count = %d, want %d", got, want)
	}
}

func requireWaitpointCompletionPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, wantOutput, wantResolution []byte) {
	t.Helper()
	var output, waitpointResolution, runWaitResolution, responseResolution []byte
	if err := pool.QueryRow(ctx, `
SELECT waitpoints.output,
       waitpoints.resolution,
       run_waits.resolution,
       waitpoint_responses.resolution
  FROM waitpoints
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
  JOIN run_waits ON run_waits.org_id = run_wait_dependencies.org_id
                AND run_waits.run_id = $2
                AND run_waits.id = run_wait_dependencies.run_wait_id
  JOIN waitpoint_responses ON waitpoint_responses.org_id = waitpoints.org_id
                          AND waitpoint_responses.waitpoint_id = waitpoints.id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &runWaitResolution, &responseResolution); err != nil {
		t.Fatal(err)
	}
	requireCanonicalJSON(t, "waitpoint output", output, wantOutput)
	requireCanonicalJSON(t, "waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "run wait resolution", runWaitResolution, wantResolution)
	requireCanonicalJSON(t, "waitpoint response resolution", responseResolution, wantResolution)
}

func requireCancelledWaitpointPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, wantResolution []byte) {
	t.Helper()
	var output, waitpointResolution, runWaitResolution, runWaitFailure []byte
	var outputIsError bool
	if err := pool.QueryRow(ctx, `
SELECT waitpoints.output,
       waitpoints.resolution,
       waitpoints.output_is_error,
       run_waits.resolution,
       run_waits.failure
  FROM waitpoints
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
  JOIN run_waits ON run_waits.org_id = run_wait_dependencies.org_id
                AND run_waits.run_id = $2
                AND run_waits.id = run_wait_dependencies.run_wait_id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &outputIsError, &runWaitResolution, &runWaitFailure); err != nil {
		t.Fatal(err)
	}
	if !outputIsError {
		t.Fatal("waitpoint output_is_error = false, want true")
	}
	requireCanonicalJSON(t, "cancelled waitpoint output", output, []byte(`null`))
	requireCanonicalJSON(t, "cancelled waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "cancelled run wait resolution", runWaitResolution, wantResolution)
	requireCanonicalJSON(t, "cancelled run wait failure", runWaitFailure, wantResolution)
}

func requireCanonicalJSON(t *testing.T, name string, got []byte, want []byte) {
	t.Helper()
	gotCanonical := canonicalJSON(t, got)
	wantCanonical := canonicalJSON(t, want)
	if gotCanonical != wantCanonical {
		t.Fatalf("%s = %s, want %s", name, gotCanonical, wantCanonical)
	}
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("invalid JSON %q: %v", string(raw), err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(canonical)
}

func requireRunEventKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("run event %q not found", kind)
	}
}

func requireRunEventKindCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run event %q count = %d, want %d", kind, got, want)
	}
}

func requireRunEventObservability(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID, runID pgtype.UUID, kind, wantCategory, wantSeverity, wantSource, wantRedactionClass string, wantSession bool) {
	t.Helper()
	var gotProjectID pgtype.UUID
	var gotEnvironmentID pgtype.UUID
	var gotAttemptID pgtype.UUID
	var gotSessionID pgtype.UUID
	var traceID string
	var spanID pgtype.Text
	var parentSpanID pgtype.Text
	var traceparent pgtype.Text
	var category string
	var severity string
	var source string
	var message string
	var redactionClass string
	var snapshotVersion pgtype.Int8
	if err := pool.QueryRow(ctx, `
SELECT project_id,
       environment_id,
       attempt_id,
       session_id,
       trace_id,
       span_id,
       parent_span_id,
       traceparent,
       category,
       severity,
       source,
       message,
       redaction_class,
       snapshot_version
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
 ORDER BY id DESC
 LIMIT 1
`, orgID, runID, kind).Scan(
		&gotProjectID,
		&gotEnvironmentID,
		&gotAttemptID,
		&gotSessionID,
		&traceID,
		&spanID,
		&parentSpanID,
		&traceparent,
		&category,
		&severity,
		&source,
		&message,
		&redactionClass,
		&snapshotVersion,
	); err != nil {
		t.Fatal(err)
	}
	if gotProjectID != projectID || gotEnvironmentID != environmentID {
		t.Fatalf("event %q scope = %v/%v, want %v/%v", kind, gotProjectID, gotEnvironmentID, projectID, environmentID)
	}
	if !gotAttemptID.Valid {
		t.Fatalf("event %q attempt_id is null", kind)
	}
	if len(traceID) != 32 {
		t.Fatalf("event %q trace_id = %q", kind, traceID)
	}
	if category != wantCategory || severity != wantSeverity || source != wantSource || redactionClass != wantRedactionClass || message != kind {
		t.Fatalf("event %q envelope category/severity/source/redaction/message = %q/%q/%q/%q/%q, want %q/%q/%q/%q/%q", kind, category, severity, source, redactionClass, message, wantCategory, wantSeverity, wantSource, wantRedactionClass, kind)
	}
	if !snapshotVersion.Valid || snapshotVersion.Int64 <= 0 {
		t.Fatalf("event %q snapshot_version = %+v", kind, snapshotVersion)
	}
	if wantSession {
		if !gotSessionID.Valid || !spanID.Valid || len(spanID.String) != 16 || !parentSpanID.Valid || len(parentSpanID.String) != 16 || !traceparent.Valid || !strings.Contains(traceparent.String, traceID+"-"+spanID.String) {
			t.Fatalf("event %q session trace fields session=%+v span=%+v parent=%+v traceparent=%+v", kind, gotSessionID, spanID, parentSpanID, traceparent)
		}
	} else if gotSessionID.Valid {
		t.Fatalf("event %q session_id = %+v, want null", kind, gotSessionID)
	}
}

func requireRunSnapshotTransitionCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, transition string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_snapshots
 WHERE org_id = $1
   AND run_id = $2
   AND transition = $3
`, orgID, runID, transition).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run snapshot transition %q count = %d, want %d", transition, got, want)
	}
}

func requireRunSessionEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, attemptNumber int32, kind string, wantPayload []byte) {
	t.Helper()
	var gotSessionID pgtype.UUID
	var gotAttemptNumber pgtype.Int4
	var gotPayload []byte
	if err := pool.QueryRow(ctx, `
SELECT session_id, attempt_number, payload
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
   AND kind = $4
 ORDER BY id DESC
 LIMIT 1
`, orgID, runID, sessionID, kind).Scan(&gotSessionID, &gotAttemptNumber, &gotPayload); err != nil {
		t.Fatal(err)
	}
	if gotSessionID != sessionID || !gotAttemptNumber.Valid || gotAttemptNumber.Int32 != attemptNumber {
		t.Fatalf("run event %q session = %+v attempt = %+v, want session %s attempt %d", kind, gotSessionID, gotAttemptNumber, ids.MustFromPG(sessionID), attemptNumber)
	}
	requireCanonicalJSON(t, "run event payload", gotPayload, wantPayload)
}

func requireNoRunEventKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("run event %q count = %d, want 0", kind, count)
	}
}

func seedWaitingWaitpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, orgID pgtype.UUID, suffix string) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-"+suffix)
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-" + suffix
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text(messageID),
		DispatchLeaseID:   "lease-" + suffix,
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    suffix,
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		SessionID:                  sessionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgvalue.Text(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:        pgvalue.Text("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusSuspended)
	return runID, waitpointID
}

func seedWaitpointResponseToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, tokenHash []byte, externalSubject string) pgtype.UUID {
	t.Helper()
	tokenID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO waitpoint_response_tokens (id, org_id, project_id, environment_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, waitpoints.project_id, waitpoints.environment_id, $4, $5, now() + interval '5 minutes', $6, '{}'
  FROM run_wait_dependencies
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID, tokenHash, externalSubject); err != nil {
		t.Fatal(err)
	}
	return tokenID
}

func respondWaitpointToken(ctx context.Context, queries *db.Queries, orgID, waitpointID, tokenID pgtype.UUID, tokenHash []byte, responseKey string) error {
	if _, err := queries.MarkWaitpointResponseTokenCompleted(ctx, db.MarkWaitpointResponseTokenCompletedParams{
		OrgID:                orgID,
		ID:                   tokenID,
		TokenHash:            tokenHash,
		CompletedByPrincipal: pgvalue.Text(responseKey),
		CompletedVia:         pgvalue.Text("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		ResponseKey:          responseKey,
		RequestHash:          responseKey,
		Action:               "respond",
		Kind:                 db.WaitpointKindHuman,
		ResolutionKind:       pgvalue.Text("completed"),
		Resolution:           approvedWaitpointResolution(responseKey),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgvalue.Text(responseKey),
		CompletedVia:         pgvalue.Text("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		return err
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, pgtype.UUID{}, waitpointID)); err != nil {
		return err
	}
	_, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	return err
}

func resolveApprovedWaitpointParams(orgID, runID, waitpointID pgtype.UUID) db.ResolveWaitpointParams {
	return db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgvalue.Text("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("reviewer@example.com"),
	}
}

func approvedWaitpointResolution(principal string) []byte {
	payload, err := json.Marshal(map[string]any{
		"value":     map[string]any{"approved": true},
		"principal": principal,
		"at":        "2026-04-23T00:00:00Z",
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func testCheckpointArtifactsJSON(t *testing.T) []byte {
	t.Helper()
	rows := []map[string]any{
		{"role": "runtime_config", "ordinal": 0, "digest": testDigest("1"), "size_bytes": 1, "media_type": cas.CheckpointRuntimeConfigMediaType},
		{"role": "runtime_vmstate", "ordinal": 0, "digest": testDigest("2"), "size_bytes": 2, "media_type": cas.CheckpointVMStateMediaType},
		{"role": "runtime_memory", "ordinal": 0, "digest": testDigest("3"), "size_bytes": 3, "media_type": cas.CheckpointMemoryMediaType},
		{"role": "runtime_scratch_disk", "ordinal": 0, "digest": testDigest("4"), "size_bytes": 4, "media_type": cas.CheckpointScratchDiskMediaType},
	}
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testDigest(char string) string {
	return "sha256:" + strings.Repeat(char, 64)
}

func seedLeasableRunQueueItem(t *testing.T, ctx context.Context, queries *db.Queries, orgID, runID pgtype.UUID, queueName string, instance db.UpsertWorkerInstanceHeartbeatRow, messageID string) {
	t.Helper()
	if _, err := queries.UpsertRunRuntimeRequirements(ctx, db.UpsertRunRuntimeRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeID:               instance.RuntimeID,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v0",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
		WorkerGroupID:           instance.WorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := queries.UpsertRunQueueItemQueued(ctx, db.UpsertRunQueueItemQueuedParams{
		RunID:             runID,
		OrgID:             orgID,
		Priority:          10,
		QueueName:         queueName,
		QueueTimestamp:    pgvalue.Timestamptz(time.Now()),
		DispatchMessageID: pgvalue.Text(messageID),
	})
	if err != nil {
		t.Fatal(err)
	}
	publishTestRunQueueItem(t, ctx, queries, orgID, runID, entry, messageID)
	if _, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgvalue.Text(messageID),
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
}

type runQueueItemDispatchState struct {
	Status                     db.RunQueueStatus
	QueuedExpiresAt            pgtype.Timestamptz
	DispatchMessageID          pgtype.Text
	ReservedByWorkerInstanceID pgtype.UUID
	ReservationExpiresAt       pgtype.Timestamptz
	DispatchGeneration         int64
	LastError                  string
}

func requireRunQueueItemDispatchState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID) runQueueItemDispatchState {
	t.Helper()
	var got runQueueItemDispatchState
	if err := pool.QueryRow(ctx, `
SELECT status,
       queued_expires_at,
       dispatch_message_id,
       reserved_by_worker_instance_id,
       reservation_expires_at,
       dispatch_generation,
       last_error
  FROM run_queue_items
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(
		&got.Status,
		&got.QueuedExpiresAt,
		&got.DispatchMessageID,
		&got.ReservedByWorkerInstanceID,
		&got.ReservationExpiresAt,
		&got.DispatchGeneration,
		&got.LastError,
	); err != nil {
		t.Fatal(err)
	}
	return got
}

func requireRunQueueItemStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID, want db.RunQueueStatus) {
	t.Helper()
	var got db.RunQueueStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_queue_items
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run queue status = %s, want %s", got, want)
	}
}
