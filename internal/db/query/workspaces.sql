-- name: CreateWorkspaceFromSandbox :one
WITH created_workspace AS (
    INSERT INTO workspaces (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_sandbox_id,
        sandbox_id,
        sandbox_fingerprint,
        current_version_id,
        external_id,
        metadata,
        tags,
        retention_policy
    )
    SELECT sqlc.arg(id),
           deployment_sandboxes.org_id,
           deployment_sandboxes.project_id,
           deployment_sandboxes.environment_id,
           deployment_sandboxes.id,
           deployment_sandboxes.sandbox_id,
           deployment_sandboxes.fingerprint,
           sqlc.arg(initial_version_id),
           coalesce(sqlc.arg(external_id)::text, ''),
           coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
           coalesce(sqlc.arg(retention_policy)::jsonb, '{}'::jsonb)
      FROM deployment_sandboxes
     WHERE deployment_sandboxes.org_id = sqlc.arg(org_id)
       AND deployment_sandboxes.project_id = sqlc.arg(project_id)
       AND deployment_sandboxes.environment_id = sqlc.arg(environment_id)
       AND deployment_sandboxes.id = sqlc.arg(deployment_sandbox_id)
    RETURNING *
),
created_version AS (
    INSERT INTO workspace_versions (
        id,
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
        promoted_at,
        created_by_subject_type
    )
    SELECT sqlc.arg(initial_version_id),
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
           now(),
           'system'
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

-- name: ListWorkspaces :many
SELECT *
  FROM workspaces
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deleted_at IS NULL
   AND (sqlc.narg(state)::workspace_state IS NULL OR state = sqlc.narg(state)::workspace_state)
   AND (sqlc.narg(external_id)::text IS NULL OR external_id = sqlc.narg(external_id)::text)
   AND (sqlc.narg(tag)::text IS NULL OR tags @> ARRAY[sqlc.narg(tag)::text])
 ORDER BY updated_at DESC, id DESC
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
         FROM workspace_materializations
        WHERE workspace_materializations.org_id = workspaces.org_id
          AND workspace_materializations.project_id = workspaces.project_id
          AND workspace_materializations.environment_id = workspaces.environment_id
          AND workspace_materializations.workspace_id = workspaces.id
          AND workspace_materializations.state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
   )
RETURNING *;

-- name: GetWorkspaceOperationIdempotency :one
UPDATE workspace_operation_idempotencies
   SET last_used_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND operation_kind = sqlc.arg(operation_kind)
   AND workspace_id IS NULL
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND expires_at > now()
RETURNING *;

-- name: GetWorkspaceScopedOperationIdempotency :one
UPDATE workspace_operation_idempotencies
   SET last_used_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND operation_kind = sqlc.arg(operation_kind)
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND expires_at > now()
RETURNING *;

-- name: CreateWorkspaceOperationIdempotency :one
INSERT INTO workspace_operation_idempotencies (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    operation_kind,
    idempotency_key,
    request_fingerprint,
    response_resource_type,
    response_resource_id,
    response_body,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.narg(workspace_id),
    sqlc.arg(operation_kind),
    sqlc.arg(idempotency_key),
    sqlc.arg(request_fingerprint),
    sqlc.arg(response_resource_type),
    sqlc.narg(response_resource_id),
    coalesce(sqlc.arg(response_body)::jsonb, '{}'::jsonb),
    sqlc.arg(expires_at)
)
RETURNING *;

-- name: CompleteWorkspaceOperationIdempotency :one
UPDATE workspace_operation_idempotencies
   SET response_resource_type = sqlc.arg(response_resource_type),
       response_resource_id = sqlc.arg(response_resource_id),
       response_body = coalesce(sqlc.arg(response_body)::jsonb, '{}'::jsonb),
       last_used_at = now()
 WHERE workspace_operation_idempotencies.org_id = sqlc.arg(org_id)
   AND workspace_operation_idempotencies.project_id = sqlc.arg(project_id)
   AND workspace_operation_idempotencies.environment_id = sqlc.arg(environment_id)
   AND workspace_operation_idempotencies.operation_kind = sqlc.arg(operation_kind)
   AND workspace_operation_idempotencies.workspace_id IS NULL
   AND workspace_operation_idempotencies.idempotency_key = sqlc.arg(idempotency_key)
   AND workspace_operation_idempotencies.request_fingerprint = sqlc.arg(request_fingerprint)
   AND workspace_operation_idempotencies.response_resource_id IS NULL
   AND workspace_operation_idempotencies.expires_at > now()
RETURNING *;
