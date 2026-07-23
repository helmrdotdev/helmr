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

-- name: LockActorStartAuthority :one
SELECT actor_definition.id AS actor_definition_id,
       actor_definition.deployment_id,
       actor_definition.manifest_version AS actor_manifest_version,
       actor_definition.manifest AS actor_manifest,
       actor_definition.manifest_digest AS actor_manifest_digest,
       deployments.queue_config,
       workspaces.id AS workspace_id,
       workspaces.head_version_id,
       workspaces.state AS workspace_state,
       workspaces.desired_state AS workspace_desired_state,
       workspaces.dirty_state AS workspace_dirty_state,
       workspaces.state_version AS workspace_state_version,
       workspaces.owner_actor_id,
       workspaces.owner_run_id,
       deployments.program_architecture,
       workspace_definition.workspace_architecture,
       EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.workspace_id = workspaces.id
              AND workspace_leases.state IN ('active', 'releasing')
       ) AS has_active_lease,
       EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = workspaces.id
              AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS has_active_process
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
   AND deployments.program_code_artifact_id IS NOT NULL
   AND deployments.program_dependency_artifact_id IS NOT NULL
   AND deployments.program_runtime_digest IS NOT NULL
   AND deployments.program_architecture IS NOT NULL
   AND deployments.program_architecture = deployments.build_architecture
   AND deployments.program_runtime_digest = deployments.build_runtime_digest
  JOIN workspaces
    ON workspaces.org_id = environments.org_id
   AND workspaces.project_id = environments.project_id
   AND workspaces.environment_id = environments.id
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.deleted_at IS NULL
  JOIN deployment_definitions AS workspace_definition
    ON workspace_definition.environment_id = workspaces.environment_id
   AND workspace_definition.id = workspaces.deployment_definition_id
   AND workspace_definition.kind = 'workspace'
   AND workspace_definition.declared_id = workspaces.workspace_declared_id
  JOIN workspace_versions AS head
    ON head.workspace_id = workspaces.id
   AND head.id = workspaces.head_version_id
   AND head.state = 'committed'
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND environments.id = sqlc.arg(environment_id)
 FOR UPDATE OF environments, workspaces;

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
