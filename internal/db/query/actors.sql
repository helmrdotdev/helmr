-- name: CreateActor :one
INSERT INTO actors (
    id,
    public_id,
    environment_id,
    actor_declared_id,
    deployment_definition_id,
    workspace_id,
    key,
    run_queue_name,
    run_concurrency_key,
    run_queue_concurrency_limit,
    run_priority,
    run_queue_ttl_ms,
    run_max_active_duration_ms,
    run_retry_policy,
    run_metadata,
    run_tags
)
SELECT sqlc.arg(id),
       sqlc.arg(public_id),
       actor_definition.environment_id,
       actor_definition.declared_id,
       actor_definition.id,
       workspaces.id,
       sqlc.narg(key),
       sqlc.arg(run_queue_name),
       sqlc.narg(run_concurrency_key),
       sqlc.narg(run_queue_concurrency_limit),
       sqlc.arg(run_priority),
       sqlc.narg(run_queue_ttl_ms),
       sqlc.arg(run_max_active_duration_ms),
       sqlc.arg(run_retry_policy),
       coalesce(sqlc.narg(run_metadata)::jsonb, '{}'::jsonb),
       coalesce(sqlc.narg(run_tags)::text[], '{}'::text[])
  FROM deployment_definitions AS actor_definition
  JOIN environments
    ON environments.id = actor_definition.environment_id
   AND environments.current_deployment_id = actor_definition.deployment_id
  JOIN deployments
    ON deployments.environment_id = actor_definition.environment_id
   AND deployments.id = actor_definition.deployment_id
   AND deployments.status = 'deployed'
   AND deployments.program_artifact_id IS NOT NULL
   AND deployments.program_runtime_digest IS NOT NULL
   AND deployments.program_architecture IS NOT NULL
   AND deployments.program_architecture = deployments.build_architecture
   AND deployments.program_runtime_digest = deployments.build_runtime_digest
  JOIN workspaces
    ON workspaces.environment_id = actor_definition.environment_id
   AND workspaces.id = sqlc.arg(workspace_id)
  JOIN environments AS actor_environment
    ON actor_environment.id = workspaces.environment_id
   AND actor_environment.org_id = sqlc.arg(org_id)
   AND actor_environment.project_id = sqlc.arg(project_id)
  JOIN deployment_definitions AS workspace_definition
    ON workspace_definition.environment_id = workspaces.environment_id
   AND workspace_definition.id = workspaces.deployment_definition_id
   AND workspace_definition.kind = 'workspace'
   AND workspace_definition.declared_id = workspaces.workspace_declared_id
   AND workspace_definition.workspace_architecture = deployments.program_architecture
 WHERE actor_definition.environment_id = sqlc.arg(environment_id)
   AND actor_definition.id = sqlc.arg(deployment_definition_id)
   AND actor_definition.kind = 'actor'
   AND actor_definition.declared_id = sqlc.arg(actor_declared_id)
RETURNING *;

-- name: LockActorStartDeploymentAuthority :one
SELECT actor_definition.id AS actor_definition_id,
       actor_definition.deployment_id,
       actor_definition.manifest_version AS actor_manifest_version,
       actor_definition.manifest AS actor_manifest,
       actor_definition.manifest_digest AS actor_manifest_digest,
       deployments.queue_config,
       deployments.program_architecture
  FROM environments
  JOIN deployment_definitions AS actor_definition
    ON actor_definition.environment_id = environments.id
   AND actor_definition.deployment_id = environments.current_deployment_id
   AND actor_definition.kind = 'actor'
   AND actor_definition.declared_id = sqlc.arg(actor_declared_id)
  JOIN deployments
    ON deployments.org_id = environments.org_id
   AND deployments.project_id = environments.project_id
   AND deployments.environment_id = environments.id
   AND deployments.id = actor_definition.deployment_id
   AND deployments.status = 'deployed'
   AND deployments.program_artifact_id IS NOT NULL
   AND deployments.program_runtime_digest IS NOT NULL
   AND deployments.program_architecture IS NOT NULL
   AND deployments.program_architecture = deployments.build_architecture
   AND deployments.program_runtime_digest = deployments.build_runtime_digest
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND environments.id = sqlc.arg(environment_id)
 FOR UPDATE OF environments;

-- name: LockActorStartKey :exec
SELECT pg_advisory_xact_lock(
    hashtextextended(
        jsonb_build_array(
            'helmr.actor-start-key.v0',
            sqlc.arg(environment_id)::uuid::text,
            sqlc.arg(actor_declared_id)::text,
            sqlc.arg(key)::text
        )::text,
        0
    )
);

-- name: SetActorCurrentRun :one
UPDATE actors
   SET current_run_id = sqlc.arg(run_id),
       state_version = state_version + 1,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'open'
   AND current_run_id IS NULL
   AND run_generation = 1
   AND state_version = 1
RETURNING *;

-- name: GetActor :one
SELECT *
  FROM actors
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetActorByPublicID :one
SELECT *
  FROM actors
 WHERE environment_id = sqlc.arg(environment_id)
   AND actor_declared_id = sqlc.arg(actor_declared_id)
   AND public_id = sqlc.arg(public_id);

-- name: GetActorByKey :one
SELECT *
  FROM actors
 WHERE environment_id = sqlc.arg(environment_id)
   AND actor_declared_id = sqlc.arg(actor_declared_id)
   AND key = sqlc.arg(key);

-- name: GetActorRead :one
SELECT actors.*,
       current_runs.public_id AS current_run_public_id,
       failure_runs.public_id AS failure_run_public_id
  FROM actors
  LEFT JOIN runs AS current_runs
    ON current_runs.environment_id = actors.environment_id
   AND current_runs.actor_id = actors.id
   AND current_runs.id = actors.current_run_id
  LEFT JOIN runs AS failure_runs
    ON failure_runs.environment_id = actors.environment_id
   AND failure_runs.actor_id = actors.id
   AND failure_runs.id = actors.failure_run_id
 WHERE actors.environment_id = sqlc.arg(environment_id)
   AND actors.actor_declared_id = sqlc.arg(actor_declared_id)
   AND (
       (
           sqlc.narg(address_public_id)::text IS NOT NULL
           AND actors.public_id = sqlc.narg(address_public_id)::text
       )
       OR
       (
           sqlc.narg(address_key)::text IS NOT NULL
           AND actors.key = sqlc.narg(address_key)::text
       )
   );
