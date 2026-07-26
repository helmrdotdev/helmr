-- name: CreateWorkspaceFromRunDeployment :one
WITH selected_definition AS (
    SELECT deployment_definitions.environment_id,
           deployment_definitions.id AS deployment_definition_id,
           deployment_definitions.declared_id AS workspace_declared_id,
           runs.org_id,
           runs.project_id,
           projects.default_region_id
      FROM runs
      JOIN deployment_definitions
        ON deployment_definitions.environment_id = runs.environment_id
       AND deployment_definitions.deployment_id = runs.deployment_id
       AND deployment_definitions.kind = 'workspace'
       AND deployment_definitions.declared_id = sqlc.arg(workspace_declared_id)
      JOIN projects
        ON projects.id = runs.project_id
       AND projects.org_id = runs.org_id
     WHERE runs.environment_id = sqlc.arg(environment_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status IN ('queued', 'running', 'waiting', 'retry_delayed')
     FOR UPDATE OF runs
), created_workspace AS (
    INSERT INTO workspaces (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        region_id,
        declaration_kind,
        workspace_declared_id,
        deployment_definition_id,
        head_version_id,
        key
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           selected_definition.org_id,
           selected_definition.project_id,
           selected_definition.environment_id,
           selected_definition.default_region_id,
           'workspace',
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
        org_id,
        project_id,
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
           created_workspace.org_id,
           created_workspace.project_id,
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
