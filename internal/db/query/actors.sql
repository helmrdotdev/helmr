-- name: CreateActor :one
INSERT INTO actors (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    declaration_kind,
    actor_declared_id,
    deployment_definition_id,
    workspace_id,
    key,
    managed_queue_name,
    managed_concurrency_key,
    managed_queue_concurrency_limit,
    managed_priority,
    managed_queued_ttl_ms,
    managed_max_active_duration_ms,
    managed_retry_policy_version,
    managed_retry_policy,
    managed_run_metadata,
    managed_run_tags,
    expires_at,
    metadata,
    tags
)
SELECT sqlc.arg(id),
       sqlc.arg(public_id),
       sqlc.arg(org_id),
       sqlc.arg(project_id),
       actor_definition.environment_id,
       actor_definition.kind,
       actor_definition.declared_id,
       actor_definition.id,
       workspaces.id,
       sqlc.narg(key),
       sqlc.arg(managed_queue_name),
       sqlc.narg(managed_concurrency_key),
       sqlc.narg(managed_queue_concurrency_limit),
       sqlc.arg(managed_priority),
       sqlc.narg(managed_queued_ttl_ms),
       sqlc.arg(managed_max_active_duration_ms),
       0,
       sqlc.arg(managed_retry_policy),
       coalesce(sqlc.narg(managed_run_metadata)::jsonb, '{}'::jsonb),
       coalesce(sqlc.narg(managed_run_tags)::text[], '{}'::text[]),
       sqlc.narg(expires_at),
       coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
       coalesce(sqlc.narg(tags)::text[], '{}'::text[])
  FROM deployment_definitions AS actor_definition
  JOIN environments
    ON environments.id = actor_definition.environment_id
   AND environments.current_deployment_id = actor_definition.deployment_id
  JOIN deployments
    ON deployments.environment_id = actor_definition.environment_id
   AND deployments.id = actor_definition.deployment_id
   AND deployments.status = 'deployed'
   AND deployments.program_code_artifact_id IS NOT NULL
   AND deployments.program_dependency_artifact_id IS NOT NULL
   AND deployments.program_runtime_digest IS NOT NULL
   AND deployments.program_architecture IS NOT NULL
   AND deployments.program_architecture = deployments.build_architecture
   AND deployments.program_runtime_digest = deployments.build_runtime_digest
  JOIN workspaces
    ON workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = actor_definition.environment_id
   AND workspaces.id = sqlc.arg(workspace_id)
  JOIN deployment_definitions AS workspace_definition
    ON workspace_definition.environment_id = workspaces.environment_id
   AND workspace_definition.id = workspaces.deployment_definition_id
   AND workspace_definition.kind = workspaces.declaration_kind
   AND workspace_definition.declared_id = workspaces.workspace_declared_id
   AND workspace_definition.workspace_architecture = deployments.program_architecture
 WHERE actor_definition.environment_id = sqlc.arg(environment_id)
   AND actor_definition.id = sqlc.arg(deployment_definition_id)
   AND actor_definition.kind = 'actor'
   AND actor_definition.declared_id = sqlc.arg(actor_declared_id)
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

-- name: ExpireDueActors :many
UPDATE actors
   SET state = 'expired',
       state_version = state_version + 1,
       run_generation = run_generation + 1,
       expired_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND state = 'open'
   AND current_run_id IS NULL
   AND expires_at IS NOT NULL
   AND expires_at <= now()
RETURNING *;
