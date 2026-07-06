-- name: CreateWorkspaceFromSandbox :one
WITH created_workspace AS (
    INSERT INTO workspaces (
        id,
        public_id,
        org_id,
        worker_group_id,
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
           sqlc.arg(public_id),
           deployment_sandboxes.org_id,
           sqlc.arg(worker_group_id),
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
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
       AND deployments.status = 'deployed'
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_project.default_region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON placement_worker_group.region_id = placement_project.default_region_id
) AS project_worker_group_placement
        ON project_worker_group_placement.org_id = deployment_sandboxes.org_id
       AND project_worker_group_placement.project_id = deployment_sandboxes.project_id
       AND project_worker_group_placement.environment_id = deployment_sandboxes.environment_id
       AND project_worker_group_placement.worker_group_id = sqlc.arg(worker_group_id)
       AND project_worker_group_placement.worker_group_state = 'active'
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.region_id = project_worker_group_placement.region_id
                AND worker_groups.state = 'active'
                AND worker_groups.health_state IN ('healthy', 'degraded')
                AND worker_groups.routing_fresh_until > now()
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
        promoted_at,
        created_by_subject_type
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
  JOIN projects
    ON projects.org_id = deployment_sandboxes.org_id
   AND projects.id = deployment_sandboxes.project_id
  JOIN environments
    ON environments.org_id = deployment_sandboxes.org_id
   AND environments.project_id = deployment_sandboxes.project_id
   AND environments.id = deployment_sandboxes.environment_id
  JOIN worker_groups
    ON worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.region_id = projects.default_region_id
   AND worker_groups.state = 'active'
   AND worker_groups.health_state IN ('healthy', 'degraded')
   AND worker_groups.routing_fresh_until > now()
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
SELECT workspaces.*
  FROM workspaces
WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1
         FROM (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
         JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                   AND worker_groups.region_id = project_worker_group_placement.region_id
                   AND worker_groups.state IN ('active', 'draining')
        WHERE project_worker_group_placement.org_id = workspaces.org_id
          AND project_worker_group_placement.project_id = workspaces.project_id
          AND project_worker_group_placement.environment_id = workspaces.environment_id
          AND project_worker_group_placement.worker_group_id = workspaces.worker_group_id
          AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
   )
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
   AND workspaces.worker_group_id = sqlc.arg(worker_group_id)
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
   AND workspaces.worker_group_id = sqlc.arg(worker_group_id)
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
   AND workspaces.worker_group_id = sqlc.arg(worker_group_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(id)
   AND workspaces.deleted_at IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_mounts
        WHERE workspace_mounts.org_id = workspaces.org_id
          AND workspace_mounts.worker_group_id = workspaces.worker_group_id
          AND workspace_mounts.project_id = workspaces.project_id
          AND workspace_mounts.environment_id = workspaces.environment_id
          AND workspace_mounts.workspace_id = workspaces.id
          AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
   )
RETURNING *;

-- name: GetWorkspaceOperationIdempotency :one
UPDATE workspace_operation_idempotencies
   SET last_used_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND operation_kind = sqlc.arg(operation_kind)::workspace_operation_idempotency_kind
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
   AND operation_kind = sqlc.arg(operation_kind)::workspace_operation_idempotency_kind
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND expires_at > now()
RETURNING *;

-- name: EnsureWorkspaceOperationIdempotency :one
WITH replaced AS (
    UPDATE workspace_operation_idempotencies
       SET request_fingerprint = sqlc.arg(request_fingerprint),
           response_resource_type = sqlc.arg(response_resource_type),
           response_resource_id = sqlc.narg(response_resource_id),
           response_body = coalesce(sqlc.arg(response_body)::jsonb, '{}'::jsonb),
           expires_at = sqlc.arg(expires_at),
           created_at = now(),
           last_used_at = now()
     WHERE workspace_operation_idempotencies.org_id = sqlc.arg(org_id)
       AND workspace_operation_idempotencies.project_id = sqlc.arg(project_id)
       AND workspace_operation_idempotencies.environment_id = sqlc.arg(environment_id)
       AND workspace_operation_idempotencies.operation_kind = sqlc.arg(operation_kind)::workspace_operation_idempotency_kind
       AND workspace_operation_idempotencies.idempotency_key = sqlc.arg(idempotency_key)
       AND (
           (sqlc.narg(workspace_id)::uuid IS NULL AND workspace_operation_idempotencies.workspace_id IS NULL)
           OR workspace_operation_idempotencies.workspace_id = sqlc.narg(workspace_id)::uuid
       )
       AND workspace_operation_idempotencies.expires_at <= now()
    RETURNING workspace_operation_idempotencies.*, TRUE::boolean AS inserted
),
inserted AS (
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
    )
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.narg(workspace_id),
        sqlc.arg(operation_kind)::workspace_operation_idempotency_kind,
        sqlc.arg(idempotency_key),
        sqlc.arg(request_fingerprint),
        sqlc.arg(response_resource_type),
        sqlc.narg(response_resource_id),
        coalesce(sqlc.arg(response_body)::jsonb, '{}'::jsonb),
        sqlc.arg(expires_at)
     WHERE NOT EXISTS (SELECT 1 FROM replaced)
    ON CONFLICT DO NOTHING
    RETURNING workspace_operation_idempotencies.*, TRUE::boolean AS inserted
),
existing AS (
    UPDATE workspace_operation_idempotencies
       SET last_used_at = now()
     WHERE workspace_operation_idempotencies.org_id = sqlc.arg(org_id)
       AND workspace_operation_idempotencies.project_id = sqlc.arg(project_id)
       AND workspace_operation_idempotencies.environment_id = sqlc.arg(environment_id)
       AND workspace_operation_idempotencies.operation_kind = sqlc.arg(operation_kind)::workspace_operation_idempotency_kind
       AND workspace_operation_idempotencies.idempotency_key = sqlc.arg(idempotency_key)
       AND (
           (sqlc.narg(workspace_id)::uuid IS NULL AND workspace_operation_idempotencies.workspace_id IS NULL)
           OR workspace_operation_idempotencies.workspace_id = sqlc.narg(workspace_id)::uuid
       )
       AND workspace_operation_idempotencies.expires_at > now()
       AND NOT EXISTS (SELECT 1 FROM replaced)
       AND NOT EXISTS (SELECT 1 FROM inserted)
    RETURNING workspace_operation_idempotencies.*, FALSE::boolean AS inserted
)
SELECT * FROM replaced
UNION ALL
SELECT * FROM inserted
UNION ALL
SELECT * FROM existing
LIMIT 1;

-- name: CompleteWorkspaceOperationIdempotency :one
UPDATE workspace_operation_idempotencies
   SET response_resource_type = sqlc.arg(response_resource_type),
       response_resource_id = sqlc.arg(response_resource_id),
       response_body = coalesce(sqlc.arg(response_body)::jsonb, '{}'::jsonb),
       last_used_at = now()
 WHERE workspace_operation_idempotencies.org_id = sqlc.arg(org_id)
   AND workspace_operation_idempotencies.project_id = sqlc.arg(project_id)
   AND workspace_operation_idempotencies.environment_id = sqlc.arg(environment_id)
   AND workspace_operation_idempotencies.operation_kind = sqlc.arg(operation_kind)::workspace_operation_idempotency_kind
   AND workspace_operation_idempotencies.workspace_id IS NULL
   AND workspace_operation_idempotencies.idempotency_key = sqlc.arg(idempotency_key)
   AND workspace_operation_idempotencies.request_fingerprint = sqlc.arg(request_fingerprint)
   AND workspace_operation_idempotencies.response_resource_id IS NULL
   AND workspace_operation_idempotencies.expires_at > now()
RETURNING *;

-- name: CompleteWorkspaceScopedOperationIdempotency :one
UPDATE workspace_operation_idempotencies
   SET response_resource_type = sqlc.arg(response_resource_type),
       response_resource_id = sqlc.arg(response_resource_id),
       response_body = coalesce(sqlc.arg(response_body)::jsonb, '{}'::jsonb),
       last_used_at = now()
 WHERE workspace_operation_idempotencies.org_id = sqlc.arg(org_id)
   AND workspace_operation_idempotencies.project_id = sqlc.arg(project_id)
   AND workspace_operation_idempotencies.environment_id = sqlc.arg(environment_id)
   AND workspace_operation_idempotencies.operation_kind = sqlc.arg(operation_kind)::workspace_operation_idempotency_kind
   AND workspace_operation_idempotencies.workspace_id = sqlc.arg(workspace_id)
   AND workspace_operation_idempotencies.idempotency_key = sqlc.arg(idempotency_key)
   AND workspace_operation_idempotencies.request_fingerprint = sqlc.arg(request_fingerprint)
   AND workspace_operation_idempotencies.response_resource_id IS NULL
   AND workspace_operation_idempotencies.expires_at > now()
RETURNING *;
