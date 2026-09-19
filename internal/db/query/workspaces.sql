-- name: CreateWorkspaceFromCurrentDeployment :one
WITH selected_definition AS (
    SELECT deployment_definitions.environment_id,
           deployment_definitions.id AS deployment_definition_id,
           deployment_definitions.declared_id AS sandbox_declared_id,
           projects.default_region_id
      FROM deployment_definitions
      JOIN environments
        ON environments.id = deployment_definitions.environment_id
       AND environments.current_deployment_id = deployment_definitions.deployment_id
      JOIN deployments
        ON deployments.environment_id = deployment_definitions.environment_id
       AND deployments.id = deployment_definitions.deployment_id
      JOIN projects
        ON projects.id = environments.project_id
       AND projects.id = sqlc.arg(project_id)
       AND environments.org_id = sqlc.arg(org_id)
     WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
       AND deployment_definitions.id = sqlc.arg(deployment_definition_id)
       AND deployment_definitions.kind = 'sandbox'
       AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
), created_workspace AS (
    INSERT INTO workspaces (
        id,
        environment_id,
        region_id,
        sandbox_declared_id,
        deployment_definition_id,
        head_version_id,
        key
    )
    SELECT sqlc.arg(id),
           selected_definition.environment_id,
           selected_definition.default_region_id,
           selected_definition.sandbox_declared_id,
           selected_definition.deployment_definition_id,
           sqlc.arg(initial_version_id),
           sqlc.narg(key)
      FROM selected_definition
    RETURNING workspaces.id, workspaces.environment_id, workspaces.region_id, workspaces.sandbox_declared_id, workspaces.deployment_definition_id, workspaces.key, workspaces.revision, workspaces.owner_session_id, workspaces.owner_run_id, workspaces.ownership_generation, workspaces.writer_generation, workspaces.head_version_id, workspaces.status, workspaces.desired_state, workspaces.dirty_state, workspaces.last_activity_at, workspaces.created_at, workspaces.updated_at, workspaces.deleted_at
), created_version AS (
    INSERT INTO workspace_versions (
        id,
        environment_id,
        workspace_id,
        status,
        content_digest,
        size_bytes,
        entry_count,
        ownership_generation,
        writer_generation,
        published_at
    )
    SELECT sqlc.arg(initial_version_id),
           created_workspace.environment_id,
           created_workspace.id,
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
  JOIN environments
    ON environments.id = deployment_definitions.environment_id
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
   AND deployment_definitions.kind = 'sandbox'
   AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
   AND environments.current_deployment_id = deployment_definitions.deployment_id
 LIMIT 1;

-- name: ResolveRunPinnedWorkspaceDefinitionForCreate :one
SELECT deployment_definitions.*
  FROM runs
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runs.environment_id
   AND deployment_definitions.deployment_id = runs.deployment_id
   AND deployment_definitions.kind = 'sandbox'
   AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status IN ('queued', 'running', 'waiting', 'retry_delayed')
 LIMIT 1;

-- name: GetWorkspace :one
SELECT workspaces.id,
       workspaces.environment_id,
       workspaces.region_id,
       workspaces.sandbox_declared_id,
       workspaces.deployment_definition_id,
       workspaces.key,
       workspaces.revision,
       workspaces.owner_session_id,
       workspaces.owner_run_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspaces.head_version_id,
       workspaces.status,
       workspaces.desired_state,
       workspaces.dirty_state,
       workspaces.last_activity_at,
       workspaces.created_at,
       workspaces.updated_at,
       workspaces.deleted_at
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.deleted_at IS NULL;

-- name: GetWorkspaceListItemByKey :one
SELECT workspaces.id,
       workspaces.key,
       deployment_definitions.declared_id AS sandbox_id,
       deployment_definitions.deployment_id,
       workspaces.status,
       workspaces.owner_session_id,
       workspaces.owner_run_id,
       workspaces.last_activity_at,
       workspaces.created_at,
       workspaces.updated_at
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = workspaces.environment_id
   AND deployment_definitions.id = workspaces.deployment_definition_id
   AND deployment_definitions.kind = 'sandbox'
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.key = sqlc.arg(key)
   AND workspaces.deleted_at IS NULL;

-- name: ListWorkspaceListItems :many
SELECT workspaces.id,
       workspaces.key,
       deployment_definitions.declared_id AS sandbox_id,
       deployment_definitions.deployment_id,
       workspaces.status,
       workspaces.owner_session_id,
       workspaces.owner_run_id,
       workspaces.last_activity_at,
       workspaces.created_at,
       workspaces.updated_at
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = workspaces.environment_id
   AND deployment_definitions.id = workspaces.deployment_definition_id
   AND deployment_definitions.kind = 'sandbox'
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.deleted_at IS NULL
   AND (
       NOT sqlc.arg(has_after)::boolean
       OR (workspaces.created_at, workspaces.id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
   )
 ORDER BY workspaces.created_at DESC, workspaces.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkspaceDefinitionIdentity :one
SELECT deployment_definitions.declared_id,
       deployment_definitions.deployment_id
  FROM deployment_definitions
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
   AND deployment_definitions.id = sqlc.arg(deployment_definition_id)
   AND deployment_definitions.kind = 'sandbox'
 LIMIT 1;

-- name: CreateWorkspaceSecret :one
INSERT INTO workspace_secrets (
    workspace_id,
    environment_id,
    placement_kind,
    placement_target,
    secret_id, mode, allowed_origins, placeholder
) VALUES (
    sqlc.arg(workspace_id),
    sqlc.arg(environment_id),
    sqlc.arg(placement_kind),
    sqlc.arg(placement_target),
    sqlc.arg(secret_id), sqlc.arg(mode), COALESCE(sqlc.arg(allowed_origins)::text[], '{}'::text[]), sqlc.arg(placeholder)
)
RETURNING *;

-- name: LockWorkspaceAdmissionAuthority :one
SELECT workspaces.id,
       workspaces.environment_id,
       workspaces.region_id,
       workspaces.sandbox_declared_id,
       workspaces.deployment_definition_id,
       workspaces.key,
       workspaces.revision,
       workspaces.owner_session_id,
       workspaces.owner_run_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspaces.head_version_id,
       workspaces.status,
       workspaces.desired_state,
       workspaces.dirty_state,
       workspaces.last_activity_at,
       workspaces.created_at,
       workspaces.updated_at,
       workspaces.deleted_at,
       environments.org_id,
       environments.project_id,
       EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.workspace_id = workspaces.id
              AND workspace_leases.status IN ('active', 'releasing')
       ) AS has_active_lease,
       EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = workspaces.id
              AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS has_active_process
  FROM workspaces
  JOIN environments
    ON environments.id = workspaces.environment_id
  JOIN deployment_definitions AS definitions
    ON definitions.environment_id = workspaces.environment_id
   AND definitions.id = workspaces.deployment_definition_id
   AND definitions.kind = 'sandbox'
   AND definitions.declared_id = workspaces.sandbox_declared_id
  JOIN workspace_versions AS head
    ON head.workspace_id = workspaces.id
   AND head.id = workspaces.head_version_id
   AND head.status = 'committed'
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
 FOR UPDATE OF workspaces;

-- name: LockWorkspaceForDelete :one
SELECT workspaces.id,
       workspaces.environment_id,
       workspaces.region_id,
       workspaces.sandbox_declared_id,
       workspaces.deployment_definition_id,
       workspaces.key,
       workspaces.revision,
       workspaces.owner_session_id,
       workspaces.owner_run_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspaces.head_version_id,
       workspaces.status,
       workspaces.desired_state,
       workspaces.dirty_state,
       workspaces.last_activity_at,
       workspaces.created_at,
       workspaces.updated_at,
       workspaces.deleted_at,
       EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.workspace_id = workspaces.id
              AND workspace_leases.status IN ('active', 'releasing')
       ) AS has_active_lease,
       EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = workspaces.id
              AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS has_active_process
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.status <> 'deleted'
 FOR UPDATE OF workspaces;

-- name: MarkWorkspaceDeleting :one
UPDATE workspaces
   SET status = 'deleting',
       desired_state = 'deleted',
       -- Explicit deletion discards lost dirty state; it does not recover a version.
       dirty_state = CASE WHEN dirty_state = 'dirty_state_lost' THEN 'clean' ELSE dirty_state END,
       revision = revision + 1,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND revision = sqlc.arg(expected_revision)
   AND status IN ('active', 'recovery_required')
   AND owner_session_id IS NULL
   AND owner_run_id IS NULL
RETURNING workspaces.id, workspaces.environment_id, workspaces.region_id, workspaces.sandbox_declared_id, workspaces.deployment_definition_id, workspaces.key, workspaces.revision, workspaces.owner_session_id, workspaces.owner_run_id, workspaces.ownership_generation, workspaces.writer_generation, workspaces.head_version_id, workspaces.status, workspaces.desired_state, workspaces.dirty_state, workspaces.last_activity_at, workspaces.created_at, workspaces.updated_at, workspaces.deleted_at;

-- name: FinalizeDeletingWorkspaces :many
WITH eligible AS (
    SELECT workspaces.id
      FROM workspaces
     WHERE workspaces.status = 'deleting'
       AND workspaces.desired_state = 'deleted'
       AND workspaces.owner_session_id IS NULL
       AND workspaces.owner_run_id IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = workspaces.id
              AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.workspace_id = workspaces.id
              AND workspace_leases.status IN ('active', 'releasing')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_mounts
            WHERE workspace_mounts.workspace_id = workspaces.id
              AND workspace_mounts.status IN ('mounting', 'mounted', 'unmounting')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM runtime_instances
            WHERE runtime_instances.workspace_id = workspaces.id
              AND runtime_instances.reclaimed_at IS NULL
              AND runtime_instances.observed_state <> 'lost'
       )
     ORDER BY workspaces.updated_at, workspaces.id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF workspaces SKIP LOCKED
), finalized AS (
    UPDATE workspaces
       SET key = NULL,
           sandbox_declared_id = NULL,
           head_version_id = NULL,
           dirty_state = 'clean',
           status = 'deleted',
           revision = revision + 1,
           deleted_at = now(),
           updated_at = now()
      FROM eligible
     WHERE workspaces.id = eligible.id
    RETURNING workspaces.id
)
SELECT id FROM finalized;

-- name: LockActorInputWorkspace :one
SELECT id, environment_id, region_id, sandbox_declared_id, deployment_definition_id, key, revision, owner_session_id, owner_run_id, ownership_generation, writer_generation, head_version_id, status, desired_state, dirty_state, last_activity_at, created_at, updated_at, deleted_at
  FROM workspaces
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND owner_session_id = sqlc.arg(session_id)
   AND owner_run_id IS NULL
 FOR UPDATE;

-- name: ReserveWorkspaceForRun :one
UPDATE workspaces
   SET owner_run_id = sqlc.arg(run_id),
       ownership_generation = ownership_generation + 1,
       revision = revision + 1,
       desired_state = 'active',
       last_activity_at = now(),
       updated_at = now()
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.revision = sqlc.arg(expected_revision)
   AND workspaces.head_version_id = sqlc.arg(expected_head_version_id)
   AND workspaces.status = 'active'
   AND workspaces.desired_state IN ('active', 'stopped')
   AND workspaces.dirty_state = 'clean'
   AND workspaces.owner_session_id IS NULL
   AND workspaces.owner_run_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.status IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_processes
        WHERE workspace_processes.workspace_id = workspaces.id
          AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING workspaces.id, workspaces.environment_id, workspaces.region_id, workspaces.sandbox_declared_id, workspaces.deployment_definition_id, workspaces.key, workspaces.revision, workspaces.owner_session_id, workspaces.owner_run_id, workspaces.ownership_generation, workspaces.writer_generation, workspaces.head_version_id, workspaces.status, workspaces.desired_state, workspaces.dirty_state, workspaces.last_activity_at, workspaces.created_at, workspaces.updated_at, workspaces.deleted_at;

-- name: ReserveWorkspaceForActor :one
UPDATE workspaces
   SET owner_session_id = sqlc.arg(session_id),
       ownership_generation = ownership_generation + 1,
       revision = revision + 1,
       desired_state = 'active',
       last_activity_at = now(),
       updated_at = now()
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.revision = sqlc.arg(expected_revision)
   AND workspaces.head_version_id = sqlc.arg(expected_head_version_id)
   AND workspaces.status = 'active'
   AND workspaces.desired_state IN ('active', 'stopped')
   AND workspaces.dirty_state = 'clean'
   AND workspaces.owner_session_id IS NULL
   AND workspaces.owner_run_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.status IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_processes
        WHERE workspace_processes.workspace_id = workspaces.id
          AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING workspaces.id, workspaces.environment_id, workspaces.region_id, workspaces.sandbox_declared_id, workspaces.deployment_definition_id, workspaces.key, workspaces.revision, workspaces.owner_session_id, workspaces.owner_run_id, workspaces.ownership_generation, workspaces.writer_generation, workspaces.head_version_id, workspaces.status, workspaces.desired_state, workspaces.dirty_state, workspaces.last_activity_at, workspaces.created_at, workspaces.updated_at, workspaces.deleted_at;
-- name: LockChildWorkspacePair :many
SELECT id, environment_id, region_id, sandbox_declared_id, deployment_definition_id, key, revision, owner_session_id, owner_run_id, ownership_generation, writer_generation, head_version_id, status, desired_state, dirty_state, last_activity_at, created_at, updated_at, deleted_at
  FROM workspaces
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = ANY(sqlc.arg(workspace_ids)::uuid[])
 ORDER BY id
 FOR UPDATE;
