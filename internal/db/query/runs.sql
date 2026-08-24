-- name: GetRunAdmissionTime :one
SELECT transaction_timestamp()::timestamptz;

-- name: LockTaskStartDeploymentAuthority :one
SELECT task_definition.id AS task_definition_id,
       task_definition.deployment_id,
       task_definition.manifest_version AS task_manifest_version,
       task_definition.manifest AS task_manifest,
       task_definition.manifest_digest AS task_manifest_digest,
       deployments.queue_config
  FROM environments
  JOIN deployment_definitions AS task_definition
    ON task_definition.environment_id = environments.id
   AND task_definition.deployment_id = environments.current_deployment_id
   AND task_definition.kind = 'task'
   AND task_definition.declared_id = sqlc.arg(task_declared_id)
  JOIN deployments
    ON deployments.org_id = environments.org_id
   AND deployments.project_id = environments.project_id
   AND deployments.environment_id = environments.id
   AND deployments.id = task_definition.deployment_id
   AND deployments.program_artifact_id IS NOT NULL
   AND deployments.program_index_digest IS NOT NULL
   AND deployments.runtime_artifact_digest IS NOT NULL
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND environments.id = sqlc.arg(environment_id)
 FOR NO KEY UPDATE OF environments;

-- name: CreateRootRunFromCurrentDeployment :one
WITH selected_target AS MATERIALIZED (
    SELECT definitions.environment_id,
           definitions.deployment_id,
           definitions.id AS deployment_definition_id,
           definitions.declared_id AS entrypoint_declared_id,
           environments.org_id,
           environments.project_id,
           workspaces.id AS workspace_id
      FROM environments
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = environments.id
       AND definitions.deployment_id = environments.current_deployment_id
       AND definitions.kind = 'task'
       AND definitions.declared_id = sqlc.arg(entrypoint_declared_id)
      JOIN deployments
        ON deployments.environment_id = definitions.environment_id
       AND deployments.id = definitions.deployment_id
      JOIN workspaces
        ON workspaces.environment_id = environments.id
       AND workspaces.id = sqlc.arg(workspace_id)
      JOIN workspace_versions
        ON workspace_versions.workspace_id = workspaces.id
       AND workspace_versions.id = sqlc.arg(base_workspace_version_id)
       AND workspace_versions.state = 'committed'
     WHERE environments.id = sqlc.arg(environment_id)
       AND environments.org_id = sqlc.arg(org_id)
       AND environments.project_id = sqlc.arg(project_id)
       AND environments.current_deployment_id IS NOT NULL
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM idempotency_claims
                WHERE idempotency_claims.environment_id = environments.id
                  AND idempotency_claims.id = sqlc.narg(claim_id)
                  AND idempotency_claims.operation = 'task.start'
                  AND idempotency_claims.state = 'pending'
                  AND idempotency_claims.retired_at IS NULL
           )
       )
     FOR NO KEY UPDATE OF environments
), created_run AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           selected_target.org_id,
           selected_target.project_id,
           selected_target.environment_id,
           selected_target.deployment_id,
           selected_target.deployment_definition_id,
           'task',
           selected_target.entrypoint_declared_id,
           sqlc.arg(cause_kind),
           selected_target.workspace_id,
           sqlc.arg(base_workspace_version_id),
           sqlc.narg(payload),
           coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
           sqlc.arg(queue_name),
           sqlc.narg(concurrency_key),
           sqlc.narg(queue_concurrency_limit),
           sqlc.arg(priority),
           sqlc.arg(queue_origin_at),
           sqlc.arg(queue_score_at),
           sqlc.narg(queued_expires_at),
           sqlc.arg(max_active_duration_ms),
           sqlc.arg(retry_policy),
           sqlc.narg(trace_id),
           sqlc.arg(root_span_id),
           sqlc.narg(claim_id)
      FROM selected_target
    RETURNING runs.*
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: CreateAdmittedRootTaskRun :one
WITH created_run AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        schedule_id,
        schedule_generation,
        scheduled_at,
        previous_scheduled_at,
        schedule_timezone,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    VALUES (
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(deployment_id),
        sqlc.arg(deployment_definition_id),
        'task',
        sqlc.arg(entrypoint_declared_id),
        sqlc.arg(cause_kind),
        sqlc.narg(schedule_id),
        sqlc.narg(schedule_generation),
        sqlc.narg(scheduled_at),
        sqlc.narg(previous_scheduled_at),
        sqlc.narg(schedule_timezone),
        sqlc.arg(workspace_id),
        sqlc.arg(base_workspace_version_id),
        sqlc.narg(payload),
        coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
        coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
        sqlc.arg(queue_name),
        sqlc.narg(concurrency_key),
        sqlc.narg(queue_concurrency_limit),
        sqlc.arg(priority)::integer,
        now(),
        now() - (sqlc.arg(priority)::double precision * interval '1 second'),
        CASE
            WHEN sqlc.narg(queued_ttl_ms)::bigint IS NULL THEN NULL
            ELSE now() + (sqlc.narg(queued_ttl_ms)::bigint::double precision * interval '1 millisecond')
        END,
        sqlc.arg(max_active_duration_ms),
        sqlc.arg(retry_policy),
        sqlc.narg(trace_id),
        sqlc.arg(root_span_id),
        sqlc.narg(claim_id)
    )
    RETURNING *
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: CreateActorStartRun :one
WITH selected_actor AS MATERIALIZED (
    SELECT sessions.*,
           environments.org_id,
           environments.project_id,
           deployment_definitions.deployment_id
      FROM sessions
      JOIN environments ON environments.id = sessions.environment_id
      JOIN deployment_definitions
        ON deployment_definitions.environment_id = sessions.environment_id
       AND deployment_definitions.id = sessions.deployment_definition_id
       AND deployment_definitions.kind = 'actor'
       AND deployment_definitions.declared_id = sessions.actor_declared_id
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
       AND sessions.workspace_id = sqlc.arg(workspace_id)
       AND sessions.state = 'open'
       AND sessions.current_run_id IS NULL
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM idempotency_claims
                WHERE idempotency_claims.environment_id = sessions.environment_id
                  AND idempotency_claims.id = sqlc.narg(claim_id)
                  AND idempotency_claims.operation = 'actor.start'
                  AND idempotency_claims.state = 'pending'
                  AND idempotency_claims.retired_at IS NULL
           )
       )
     FOR UPDATE OF sessions
), created_run AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        session_id,
        cause_kind,
        workspace_id,
        base_workspace_version_id,
        session_input_start_sequence,
        session_input_high_watermark,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           selected_actor.org_id,
           selected_actor.project_id,
           selected_actor.environment_id,
           selected_actor.deployment_id,
           selected_actor.deployment_definition_id,
           'actor',
           selected_actor.actor_declared_id,
           selected_actor.id,
           'actor_start',
           selected_actor.workspace_id,
           sqlc.arg(base_workspace_version_id),
           0,
           sqlc.arg(input_high_watermark),
           selected_actor.run_metadata,
           selected_actor.run_tags,
           selected_actor.run_queue_name,
           selected_actor.run_concurrency_key,
           selected_actor.run_queue_concurrency_limit,
           selected_actor.run_priority,
           now(),
           now() - (selected_actor.run_priority::double precision * interval '1 second'),
           CASE
               WHEN selected_actor.run_queue_ttl_ms IS NULL THEN NULL
               ELSE now() + (selected_actor.run_queue_ttl_ms::double precision * interval '1 millisecond')
           END,
           selected_actor.run_max_active_duration_ms,
           selected_actor.run_retry_policy,
           sqlc.narg(trace_id),
           sqlc.arg(root_span_id),
           sqlc.narg(claim_id)
      FROM selected_actor
    RETURNING runs.*
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        session_input_start_sequence,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           0,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: CreateChildRunFromParentDeployment :one
WITH selected_target AS MATERIALIZED (
    SELECT definitions.environment_id,
           definitions.deployment_id,
           definitions.id AS deployment_definition_id,
           definitions.declared_id AS entrypoint_declared_id,
           parent.org_id,
           parent.project_id,
           parent.id AS parent_run_id,
           workspaces.id AS workspace_id
      FROM runs AS parent
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = parent.environment_id
       AND definitions.deployment_id = parent.deployment_id
       AND definitions.kind = 'task'
       AND definitions.declared_id = sqlc.arg(entrypoint_declared_id)
      JOIN workspaces
        ON workspaces.environment_id = parent.environment_id
       AND workspaces.id = sqlc.arg(workspace_id)
      JOIN workspace_versions
        ON workspace_versions.workspace_id = workspaces.id
       AND workspace_versions.id = sqlc.arg(base_workspace_version_id)
       AND workspace_versions.state = 'committed'
      LEFT JOIN idempotency_claims
        ON idempotency_claims.environment_id = parent.environment_id
       AND idempotency_claims.id = sqlc.narg(claim_id)
       AND idempotency_claims.operation = 'task.child.invoke'
       AND idempotency_claims.state = 'pending'
       AND idempotency_claims.retired_at IS NULL
     WHERE parent.environment_id = sqlc.arg(environment_id)
       AND parent.id = sqlc.arg(parent_run_id)
       AND parent.status IN ('queued', 'running', 'waiting', 'retry_delayed')
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR idempotency_claims.id IS NOT NULL
       )
     FOR UPDATE OF parent
), created_run AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        parent_run_id,
        parent_owns_lifecycle,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           selected_target.org_id,
           selected_target.project_id,
           selected_target.environment_id,
           selected_target.deployment_id,
           selected_target.deployment_definition_id,
           'task',
           selected_target.entrypoint_declared_id,
           'child',
           selected_target.parent_run_id,
           sqlc.arg(parent_owns_lifecycle),
           selected_target.workspace_id,
           sqlc.arg(base_workspace_version_id),
           sqlc.narg(payload),
           coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
           sqlc.arg(queue_name),
           sqlc.narg(concurrency_key),
           sqlc.narg(queue_concurrency_limit),
           sqlc.arg(priority),
           sqlc.arg(queue_origin_at),
           sqlc.arg(queue_score_at),
           sqlc.narg(queued_expires_at),
           sqlc.arg(max_active_duration_ms),
           sqlc.arg(retry_policy),
           sqlc.narg(trace_id),
           sqlc.arg(root_span_id),
           sqlc.narg(claim_id)
      FROM selected_target
    RETURNING runs.*
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: CreateSameWorkspaceChildRunFromParentDeployment :one
WITH selected_target AS MATERIALIZED (
    SELECT definitions.environment_id,
           definitions.deployment_id,
           definitions.id AS deployment_definition_id,
           definitions.declared_id AS entrypoint_declared_id,
           parent.org_id,
           parent.project_id,
           parent.id AS parent_run_id,
           parent.workspace_id,
           checkpoint.private_workspace_version_id AS base_workspace_version_id
      FROM runs AS parent
      JOIN run_waits AS wait
        ON wait.environment_id = parent.environment_id
       AND wait.run_id = parent.id
       AND wait.workspace_id = parent.workspace_id
       AND wait.id = sqlc.arg(run_wait_id)
       AND wait.kind = 'child'
       AND wait.child_run_id IS NULL
       AND wait.child_parent_owned IS TRUE
       AND wait.child_target_declared_id = sqlc.arg(entrypoint_declared_id)
       AND wait.child_claim_id = sqlc.arg(claim_id)
       AND wait.condition_state = 'pending'
       AND wait.suspension_state = 'checkpointing'
       AND wait.current_run_lease_id = sqlc.arg(parent_run_lease_id)
       AND wait.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
      JOIN run_checkpoints AS checkpoint
        ON checkpoint.run_id = parent.id
       AND checkpoint.attempt_number = wait.attempt_number
       AND checkpoint.workspace_id = parent.workspace_id
       AND checkpoint.run_wait_id = wait.id
       AND checkpoint.id = wait.suspend_checkpoint_id
       AND checkpoint.state = 'ready'
       AND checkpoint.private_workspace_version_id =
           sqlc.arg(base_workspace_version_id)
      JOIN workspace_versions AS base
        ON base.workspace_id = parent.workspace_id
       AND base.id = checkpoint.private_workspace_version_id
       AND base.state = 'private'
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = parent.environment_id
       AND definitions.deployment_id = parent.deployment_id
       AND definitions.kind = 'task'
       AND definitions.declared_id = wait.child_target_declared_id
      JOIN idempotency_claims AS claim
        ON claim.environment_id = parent.environment_id
       AND claim.id = wait.child_claim_id
       AND claim.operation = 'task.child.invoke'
       AND claim.state = 'pending'
       AND claim.retired_at IS NULL
     WHERE parent.environment_id = sqlc.arg(environment_id)
       AND parent.id = sqlc.arg(parent_run_id)
       AND parent.status = 'waiting'
       AND parent.current_attempt_number = sqlc.arg(parent_attempt_number)
       AND parent.current_run_lease_id = sqlc.arg(parent_run_lease_id)
     FOR UPDATE OF parent, wait
), created_run AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        parent_run_id,
        parent_owns_lifecycle,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           selected_target.org_id,
           selected_target.project_id,
           selected_target.environment_id,
           selected_target.deployment_id,
           selected_target.deployment_definition_id,
           'task',
           selected_target.entrypoint_declared_id,
           'child',
           selected_target.parent_run_id,
           TRUE,
           selected_target.workspace_id,
           selected_target.base_workspace_version_id,
           sqlc.narg(payload),
           coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
           sqlc.arg(queue_name),
           sqlc.narg(concurrency_key),
           sqlc.narg(queue_concurrency_limit),
           sqlc.arg(priority),
           sqlc.arg(queue_origin_at),
           sqlc.arg(queue_score_at),
           sqlc.narg(queued_expires_at),
           sqlc.arg(max_active_duration_ms),
           sqlc.arg(retry_policy),
           sqlc.narg(trace_id),
           sqlc.arg(root_span_id),
           sqlc.arg(claim_id)
      FROM selected_target
    RETURNING runs.*
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: GetRun :one
SELECT *
  FROM runs
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetRunSnapshot :one
SELECT runs.*,
       deployments.version AS deployment_version
  FROM runs
  JOIN deployments
    ON deployments.environment_id = runs.environment_id
   AND deployments.id = runs.deployment_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(id);

-- name: ListRunListItems :many
SELECT runs.id,
       runs.status,
       runs.entrypoint_kind,
       runs.entrypoint_declared_id,
       runs.workspace_id,
       runs.session_id,
       runs.current_attempt_number,
       runs.created_at,
       runs.started_at,
       runs.terminal_at
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND (
       coalesce(cardinality(sqlc.arg(statuses)::text[]), 0) = 0
       OR runs.status = ANY(sqlc.arg(statuses)::text[])
   )
   AND (
       sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (runs.created_at, runs.id) < (
           sqlc.narg(after_created_at)::timestamptz,
           sqlc.arg(after_id)::uuid
       )
   )
 ORDER BY runs.created_at DESC, runs.id DESC
 LIMIT sqlc.arg(limit_count);

-- name: ListExpiredParentOwnedChildRuns :many
SELECT child.id,
       child.parent_run_id,
       child.org_id,
       child.project_id,
       child.environment_id
  FROM runs AS child
 WHERE child.entrypoint_kind = 'task'
   AND child.session_id IS NULL
   AND child.parent_run_id IS NOT NULL
   AND child.parent_owns_lifecycle IS TRUE
   AND child.status = 'queued'
   AND child.first_lease_at IS NULL
   AND child.queued_expires_at IS NOT NULL
   AND child.queued_expires_at <= transaction_timestamp()
 ORDER BY child.queued_expires_at, child.id
 LIMIT sqlc.arg(limit_count);

-- name: LockQueuedRunExpiry :one
SELECT first_lease_at,
       queued_expires_at,
       coalesce(
           queued_expires_at <= transaction_timestamp(),
           false
       )::bool AS expired
  FROM runs
 WHERE id = sqlc.arg(id)
 FOR UPDATE;

-- name: RequestQueuedRunRuntimeCleanup :exec
WITH closing_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = CASE
               WHEN desired_state = 'closed' THEN desired_version
               ELSE desired_version + 1
           END,
           desired_at = transaction_timestamp(),
           desired_reason = 'queued_ttl_expired',
           updated_at = transaction_timestamp()
     WHERE reserved_run_id = sqlc.arg(run_id)
       AND observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING id
)
UPDATE workspace_mounts
   SET state = 'unmounting',
       stopped_at = coalesce(stopped_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE runtime_instance_id IN (SELECT id FROM closing_runtimes)
   AND state IN ('mounting', 'mounted');

-- name: ExpireQueuedRunAttempt :execrows
UPDATE run_attempts
   SET terminal_outcome = 'cancelled',
       terminal_reason_code = 'queued_ttl_expired',
       terminal_error = sqlc.arg(error_payload)::jsonb,
       terminal_at = transaction_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(attempt_number)
   AND terminal_at IS NULL;

-- name: ExpireQueuedRun :execrows
UPDATE runs
   SET status = 'expired',
       failure = sqlc.arg(failure)::jsonb,
       state_version = state_version + 1,
       retry_at = NULL,
       terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND state_version = sqlc.arg(expected_state_version)
   AND status = 'queued'
   AND current_run_lease_id IS NULL
   AND first_lease_at IS NULL
   AND queued_expires_at IS NOT NULL
   AND queued_expires_at <= transaction_timestamp();

-- name: ReleaseQueuedRunWorkspace :execrows
UPDATE workspaces
   SET owner_run_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.owner_run_id = sqlc.arg(run_id)
   AND workspaces.owner_session_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   );

-- name: CreateQueuedRunExpiryEvent :exec
INSERT INTO telemetry_outbox (
    org_id, stream_kind, source_kind, source_id, project_id, environment_id,
    run_id, attempt_number, trace_id, span_id, category, severity, source,
    kind, message, payload, redaction_class, snapshot_version, observed_at
)
SELECT runs.org_id, 'event', 'run', runs.id, runs.project_id, runs.environment_id,
       runs.id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, 'lifecycle', 'info',
       'control', 'run.expired', 'Run expired',
       jsonb_build_object('reasonCode', 'queued_ttl_expired'),
       'internal', runs.state_version, transaction_timestamp()
  FROM runs
 WHERE runs.id = sqlc.arg(run_id);

-- name: CloseRunActiveIntervalForCheckpoint :one
UPDATE runs
   SET active_elapsed_ms = active_elapsed_ms
         + floor(extract(epoch FROM (transaction_timestamp() - active_started_at)) * 1000)::bigint,
       active_started_at = NULL,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND status = 'waiting'
   AND active_started_at IS NOT NULL
   AND transaction_timestamp() < active_started_at
         + ((max_active_duration_ms - active_elapsed_ms) * interval '1 millisecond')
RETURNING active_elapsed_ms;

-- name: CloseRunActiveIntervalForCheckpointFailure :one
UPDATE runs
   SET active_elapsed_ms = LEAST(
           max_active_duration_ms,
           active_elapsed_ms
             + GREATEST(
                 floor(extract(epoch FROM (sqlc.arg(failed_at) - active_started_at)) * 1000)::bigint,
                 0
               )
       ),
       active_started_at = NULL,
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND status = 'waiting'
   AND active_started_at IS NOT NULL
RETURNING active_elapsed_ms;
