-- name: CreateWorkspaceFromCurrentDeployment :one
WITH selected_definition AS (
    SELECT deployment_definitions.environment_id,
           deployment_definitions.id AS deployment_definition_id,
           deployment_definitions.declared_id AS workspace_declared_id,
           projects.default_region_id
      FROM deployment_definitions
      JOIN environments
        ON environments.id = deployment_definitions.environment_id
       AND environments.current_deployment_id = deployment_definitions.deployment_id
      JOIN deployments
        ON deployments.environment_id = deployment_definitions.environment_id
       AND deployments.id = deployment_definitions.deployment_id
       AND deployments.status = 'deployed'
      JOIN projects
        ON projects.id = environments.project_id
       AND projects.id = sqlc.arg(project_id)
       AND environments.org_id = sqlc.arg(org_id)
     WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
       AND deployment_definitions.id = sqlc.arg(deployment_definition_id)
       AND deployment_definitions.kind = 'workspace'
       AND deployment_definitions.declared_id = sqlc.arg(workspace_declared_id)
), created_workspace AS (
    INSERT INTO workspaces (
        id,
        public_id,
        environment_id,
        region_id,
        workspace_declared_id,
        deployment_definition_id,
        head_version_id,
        key
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           selected_definition.environment_id,
           selected_definition.default_region_id,
           selected_definition.workspace_declared_id,
           selected_definition.deployment_definition_id,
           sqlc.arg(initial_version_id),
           sqlc.narg(key)
      FROM selected_definition
    RETURNING *
), created_version AS (
    INSERT INTO workspace_versions (
        id,
        public_id,
        environment_id,
        workspace_id,
        kind,
        state,
        content_digest,
        size_bytes,
        entry_count,
        ownership_generation,
        writer_generation,
        published_at
    )
    SELECT sqlc.arg(initial_version_id),
           sqlc.arg(initial_version_public_id),
           created_workspace.environment_id,
           created_workspace.id,
           'system'::workspace_version_kind,
           'committed',
           'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
           0,
           0,
           0,
           0,
           now()
      FROM created_workspace
    RETURNING workspace_id
)
SELECT created_workspace.*
  FROM created_workspace
  JOIN created_version ON created_version.workspace_id = created_workspace.id;

-- name: ResolveCurrentWorkspaceDefinitionForCreate :one
SELECT deployment_definitions.*
  FROM deployment_definitions
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
   AND deployments.status = 'deployed'
  JOIN environments
    ON environments.id = deployment_definitions.environment_id
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
   AND deployment_definitions.kind = 'workspace'
   AND deployment_definitions.declared_id = sqlc.arg(workspace_declared_id)
   AND environments.current_deployment_id = deployment_definitions.deployment_id
 LIMIT 1;

-- name: ResolveRunPinnedWorkspaceDefinitionForCreate :one
SELECT deployment_definitions.*
  FROM runs
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runs.environment_id
   AND deployment_definitions.deployment_id = runs.deployment_id
   AND deployment_definitions.kind = 'workspace'
   AND deployment_definitions.declared_id = sqlc.arg(workspace_declared_id)
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status IN ('queued', 'running', 'waiting', 'retry_delayed')
 LIMIT 1;

-- name: GetWorkspace :one
SELECT workspaces.*
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.deleted_at IS NULL;

-- name: GetWorkspaceByPublicID :one
SELECT workspaces.*
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.public_id = sqlc.arg(public_id)
   AND workspaces.deleted_at IS NULL;

-- name: GetWorkspaceByDeclaredIDAndKey :one
SELECT workspaces.*
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.workspace_declared_id = sqlc.arg(workspace_declared_id)
   AND workspaces.key = sqlc.arg(key)
   AND workspaces.deleted_at IS NULL;

-- name: CreateWorkspaceSecret :one
INSERT INTO workspace_secrets (
    workspace_id,
    environment_id,
    placement_kind,
    placement_target,
    secret_id
) VALUES (
    sqlc.arg(workspace_id),
    sqlc.arg(environment_id),
    sqlc.arg(placement_kind),
    sqlc.arg(placement_target),
    sqlc.arg(secret_id)
)
RETURNING *;

-- name: ResolveWorkspaceTarget :one
SELECT workspaces.id
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.deleted_at IS NULL
   AND (
       (sqlc.narg(public_id)::text IS NOT NULL
        AND sqlc.narg(key)::text IS NULL
        AND workspaces.public_id = sqlc.narg(public_id)::text)
       OR
       (sqlc.narg(public_id)::text IS NULL
        AND sqlc.narg(key)::text IS NOT NULL
        AND workspaces.key = sqlc.narg(key)::text)
   );

-- name: GetWorkspaceByOrgAndID :one
SELECT workspaces.*
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.deleted_at IS NULL;

-- name: LockWorkspaceAdmissionAuthority :one
SELECT workspaces.*,
       environments.org_id,
       environments.project_id,
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
  FROM workspaces
  JOIN environments
    ON environments.id = workspaces.environment_id
  JOIN deployment_definitions AS definitions
    ON definitions.environment_id = workspaces.environment_id
   AND definitions.id = workspaces.deployment_definition_id
   AND definitions.kind = 'workspace'
   AND definitions.declared_id = workspaces.workspace_declared_id
  JOIN workspace_versions AS head
    ON head.workspace_id = workspaces.id
   AND head.id = workspaces.head_version_id
   AND head.state = 'committed'
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
 FOR UPDATE OF workspaces;

-- name: LockWorkspaceForDelete :one
SELECT workspaces.*,
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
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.public_id = sqlc.arg(public_id)
   AND workspaces.state <> 'deleted'
 FOR UPDATE OF workspaces;

-- name: MarkWorkspaceDeleting :one
UPDATE workspaces
   SET state = 'deleting',
       desired_state = 'deleted',
       state_version = state_version + 1,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND state_version = sqlc.arg(expected_state_version)
   AND state IN ('active', 'recovery_required')
   AND owner_actor_id IS NULL
   AND owner_run_id IS NULL
RETURNING *;

-- name: LockActorInputWorkspace :one
SELECT *
  FROM workspaces
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND owner_actor_id = sqlc.arg(actor_id)
   AND owner_run_id IS NULL
 FOR UPDATE;

-- name: ReserveWorkspaceForRun :one
UPDATE workspaces
   SET owner_run_id = sqlc.arg(run_id),
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       desired_state = 'active',
       last_activity_at = now(),
       updated_at = now()
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.state_version = sqlc.arg(expected_state_version)
   AND workspaces.head_version_id = sqlc.arg(expected_head_version_id)
   AND workspaces.state = 'active'
   AND workspaces.desired_state IN ('active', 'stopped')
   AND workspaces.dirty_state = 'clean'
   AND workspaces.owner_actor_id IS NULL
   AND workspaces.owner_run_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_processes
        WHERE workspace_processes.workspace_id = workspaces.id
          AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING *;

-- name: ReserveWorkspaceForActor :one
UPDATE workspaces
   SET owner_actor_id = sqlc.arg(actor_id),
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       desired_state = 'active',
       last_activity_at = now(),
       updated_at = now()
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.state_version = sqlc.arg(expected_state_version)
   AND workspaces.head_version_id = sqlc.arg(expected_head_version_id)
   AND workspaces.state = 'active'
   AND workspaces.desired_state IN ('active', 'stopped')
   AND workspaces.dirty_state = 'clean'
   AND workspaces.owner_actor_id IS NULL
   AND workspaces.owner_run_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_processes
        WHERE workspace_processes.workspace_id = workspaces.id
          AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING *;
-- name: LockChildWorkspacePair :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);
