-- name: CreateWorkspaceFromRunDeployment :one
WITH selected_definition AS (
    SELECT deployment_definitions.environment_id,
           deployment_definitions.id AS deployment_definition_id,
           deployment_definitions.declared_id AS sandbox_declared_id,
           projects.default_region_id
      FROM runs
      JOIN deployment_definitions
        ON deployment_definitions.environment_id = runs.environment_id
       AND deployment_definitions.deployment_id = runs.deployment_id
       AND deployment_definitions.kind = 'sandbox'
       AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
      JOIN environments
        ON environments.id = runs.environment_id
      JOIN projects
        ON projects.id = environments.project_id
     WHERE runs.environment_id = sqlc.arg(environment_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status IN ('queued', 'running', 'waiting', 'retry_delayed')
     FOR UPDATE OF runs
), created_workspace AS (
    INSERT INTO computers (
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
    RETURNING computers.id, computers.environment_id, computers.region_id, computers.sandbox_declared_id, computers.deployment_definition_id, computers.key, computers.revision, computers.owner_session_id, computers.owner_run_id, computers.ownership_generation, computers.writer_generation, computers.head_version_id, computers.status, computers.desired_state, computers.dirty_state, computers.last_activity_at, computers.created_at, computers.updated_at, computers.deleted_at
), created_version AS (
    INSERT INTO computer_versions (
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
           'initializing',
           NULL,
           0,
           0,
           0,
           0,
           NULL
      FROM created_workspace
    RETURNING workspace_id
)
SELECT created_workspace.*
  FROM created_workspace
  JOIN created_version ON created_version.workspace_id = created_workspace.id;
