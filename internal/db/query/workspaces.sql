-- name: CreateWorkspaceFromSandbox :one
WITH created_workspace AS (
    INSERT INTO workspaces (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        region_id,
        deployment_sandbox_id,
        sandbox_id,
        sandbox_fingerprint,
        current_version_id,
        external_id,
        create_idempotency_key,
        create_idempotency_expires_at,
        create_request_fingerprint,
        metadata,
        tags,
        retention_policy
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           deployment_sandboxes.org_id,
           deployment_sandboxes.project_id,
           deployment_sandboxes.environment_id,
           projects.default_region_id,
           deployment_sandboxes.id,
           deployment_sandboxes.sandbox_id,
           deployment_sandboxes.fingerprint,
           sqlc.arg(initial_version_id),
           coalesce(sqlc.arg(external_id)::text, ''),
           coalesce(sqlc.arg(create_idempotency_key)::text, ''),
           sqlc.narg(create_idempotency_expires_at),
           coalesce(sqlc.arg(create_request_fingerprint)::text, ''),
           coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
           coalesce(sqlc.arg(retention_policy)::jsonb, '{}'::jsonb)
      FROM deployment_sandboxes
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
       AND deployments.status = 'deployed'
      JOIN projects
        ON projects.org_id = deployment_sandboxes.org_id
       AND projects.id = deployment_sandboxes.project_id
     WHERE deployment_sandboxes.org_id = sqlc.arg(org_id)
       AND deployment_sandboxes.project_id = sqlc.arg(project_id)
       AND deployment_sandboxes.environment_id = sqlc.arg(environment_id)
       AND deployment_sandboxes.id = sqlc.arg(deployment_sandbox_id)
    RETURNING *
),
created_version AS (
    INSERT INTO workspace_versions (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        workspace_id,
        kind,
        state,
        artifact_id,
        artifact_encoding,
        artifact_entry_count,
        content_digest,
        size_bytes,
        message,
        promoted_at
    )
    SELECT sqlc.arg(initial_version_id),
           sqlc.arg(initial_version_public_id),
           created_workspace.org_id,
           created_workspace.project_id,
           created_workspace.environment_id,
           created_workspace.id,
           'system'::workspace_version_kind,
           'ready'::workspace_version_state,
           sqlc.arg(initial_artifact_id),
           sqlc.arg(initial_artifact_encoding),
           sqlc.arg(initial_artifact_entry_count),
           sqlc.arg(initial_content_digest),
           sqlc.arg(initial_size_bytes),
           'initial empty workspace',
           now()
      FROM created_workspace
    RETURNING *
)
SELECT created_workspace.*
  FROM created_workspace
  JOIN created_version
    ON created_version.org_id = created_workspace.org_id
   AND created_version.project_id = created_workspace.project_id
   AND created_version.environment_id = created_workspace.environment_id
   AND created_version.workspace_id = created_workspace.id;

-- name: ResolveDeploymentSandboxForWorkspaceCreate :one
SELECT deployment_sandboxes.*
  FROM deployment_sandboxes
  JOIN deployments
    ON deployments.org_id = deployment_sandboxes.org_id
   AND deployments.project_id = deployment_sandboxes.project_id
   AND deployments.environment_id = deployment_sandboxes.environment_id
   AND deployments.id = deployment_sandboxes.deployment_id
  JOIN projects
    ON projects.org_id = deployment_sandboxes.org_id
   AND projects.id = deployment_sandboxes.project_id
  JOIN environments
    ON environments.org_id = deployment_sandboxes.org_id
   AND environments.project_id = deployment_sandboxes.project_id
   AND environments.id = deployment_sandboxes.environment_id
 WHERE deployment_sandboxes.org_id = sqlc.arg(org_id)
   AND deployment_sandboxes.project_id = sqlc.arg(project_id)
   AND deployment_sandboxes.environment_id = sqlc.arg(environment_id)
   AND deployment_sandboxes.sandbox_id = sqlc.arg(sandbox_id)
   AND deployments.status = 'deployed'
   AND (
       (sqlc.narg(deployment_id)::uuid IS NOT NULL AND deployments.id = sqlc.narg(deployment_id)::uuid)
       OR
       (sqlc.narg(deployment_id)::uuid IS NULL AND environments.current_deployment_id = deployments.id)
   )
 LIMIT 1;

-- name: GetWorkspace :one
SELECT *
  FROM workspaces
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND deleted_at IS NULL;

-- name: GetWorkspaceByOrgAndID :one
SELECT *
  FROM workspaces
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND deleted_at IS NULL;

-- name: GetWorkspaceByCreateIdempotency :one
SELECT *
  FROM workspaces
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND create_idempotency_key = sqlc.arg(idempotency_key)
   AND create_idempotency_expires_at > now()
   AND deleted_at IS NULL;

-- name: ClearExpiredWorkspaceCreateIdempotency :exec
UPDATE workspaces
   SET create_idempotency_key = '',
       create_idempotency_expires_at = NULL,
       create_request_fingerprint = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND create_idempotency_key = sqlc.arg(idempotency_key)
   AND create_idempotency_expires_at <= now();

-- name: ListWorkspaces :many
SELECT workspaces.*
  FROM workspaces
WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.deleted_at IS NULL
   AND (sqlc.narg(state)::workspace_state IS NULL OR workspaces.state = sqlc.narg(state)::workspace_state)
   AND (sqlc.narg(external_id)::text IS NULL OR workspaces.external_id = sqlc.narg(external_id)::text)
   AND (sqlc.narg(tag)::text IS NULL OR workspaces.tags @> ARRAY[sqlc.narg(tag)::text])
 ORDER BY workspaces.updated_at DESC, workspaces.id DESC
 LIMIT sqlc.arg(limit_count);

-- name: PatchWorkspace :one
UPDATE workspaces
   SET metadata = coalesce(sqlc.narg(metadata)::jsonb, workspaces.metadata),
       tags = coalesce(sqlc.narg(tags)::text[], workspaces.tags),
       updated_at = now()
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.deleted_at IS NULL
RETURNING *;

-- name: SetWorkspaceDesiredStopped :one
UPDATE workspaces
   SET desired_state = 'stopped',
       updated_at = now()
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.state = 'active'
   AND workspaces.archived_at IS NULL
   AND workspaces.deleted_at IS NULL
RETURNING *;

-- name: ArchiveWorkspace :one
UPDATE workspaces
   SET desired_state = 'archived',
       state = 'archived',
       archived_at = coalesce(workspaces.archived_at, now()),
       updated_at = now()
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.deleted_at IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_mounts
        WHERE workspace_mounts.org_id = workspaces.org_id
          AND workspace_mounts.project_id = workspaces.project_id
          AND workspace_mounts.environment_id = workspaces.environment_id
          AND workspace_mounts.workspace_id = workspaces.id
          AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
   )
RETURNING *;
