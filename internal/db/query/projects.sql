-- name: CreateProject :one
INSERT INTO projects (id, org_id, slug, name, is_default)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(is_default)
)
RETURNING *;

-- name: CreateProjectWithDefaultEnvironment :one
WITH project AS (
    INSERT INTO projects (id, org_id, slug, name, is_default)
    VALUES (
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(slug),
        sqlc.arg(name),
        sqlc.arg(is_default)::boolean OR NOT EXISTS (
            SELECT 1
              FROM projects
             WHERE projects.org_id = sqlc.arg(org_id)
               AND projects.archived_at IS NULL
        )
    )
    RETURNING *
),
environment AS (
    INSERT INTO environments (id, org_id, project_id, slug, name, is_default)
    SELECT sqlc.arg(environment_id), project.org_id, project.id, 'production', 'Production', true
      FROM project
    RETURNING id
),
worker_group AS (
    INSERT INTO worker_groups (org_id, project_id, environment_id, slug, name, provisioning_mode, queue_name, region, capabilities, metadata)
    SELECT project.org_id,
           project.id,
           environment.id,
           'default',
           'Default',
           'customer_managed',
           project.slug || '/production',
           '',
           '{}'::jsonb,
           '{}'::jsonb
      FROM project
      JOIN environment ON true
    RETURNING id
),
registration_token AS (
    INSERT INTO worker_registration_tokens (id, org_id, project_id, environment_id, worker_group_id, token_hash)
    SELECT
        sqlc.arg(registration_token_id),
        project.org_id,
        project.id,
        environment.id,
        worker_group.id,
        sqlc.arg(registration_token_hash)::bytea
      FROM project
      JOIN environment ON true
      JOIN worker_group ON true
     WHERE project.is_default
       AND sqlc.arg(registration_token_hash)::bytea IS NOT NULL
    ON CONFLICT (token_hash) DO UPDATE
       SET org_id = excluded.org_id,
           project_id = excluded.project_id,
           environment_id = excluded.environment_id,
           worker_group_id = excluded.worker_group_id,
           revoked_at = NULL
    RETURNING id
)
SELECT project.*
  FROM project
  JOIN environment ON true
  JOIN worker_group ON true
  LEFT JOIN registration_token ON true;

-- name: GetProject :one
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND archived_at IS NULL;

-- name: GetProjectBySlug :one
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND slug = sqlc.arg(slug)
   AND archived_at IS NULL;

-- name: UpdateProjectDetails :one
UPDATE projects
   SET slug = sqlc.arg(slug),
       name = sqlc.arg(name)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND archived_at IS NULL
RETURNING *;

-- name: ListProjects :many
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND archived_at IS NULL
 ORDER BY is_default DESC, lower(slug), created_at ASC;

-- name: CreateEnvironment :one
INSERT INTO environments (id, org_id, project_id, slug, name, is_default)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(is_default)
)
RETURNING *;

-- name: CreateEnvironmentWithDefaultWorkerGroup :one
WITH environment AS (
    INSERT INTO environments (id, org_id, project_id, slug, name, is_default)
    VALUES (
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(slug),
        sqlc.arg(name),
        false
    )
    RETURNING *
),
worker_group AS (
    INSERT INTO worker_groups (org_id, project_id, environment_id, slug, name, provisioning_mode, queue_name, region, capabilities, metadata)
    SELECT environment.org_id,
           environment.project_id,
           environment.id,
           'default',
           'Default',
           'customer_managed',
           projects.slug || '/' || environment.slug,
           '',
           '{}'::jsonb,
           '{}'::jsonb
      FROM environment
      JOIN projects ON projects.org_id = environment.org_id
                   AND projects.id = environment.project_id
    RETURNING id
)
SELECT environment.*
  FROM environment
  JOIN worker_group ON true;

-- name: GetEnvironment :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(id)
   AND archived_at IS NULL;

-- name: GetEnvironmentBySlug :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND slug = sqlc.arg(slug)
   AND archived_at IS NULL;

-- name: GetDefaultEnvironment :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND is_default
   AND archived_at IS NULL
 LIMIT 1;

-- name: ListEnvironments :many
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND archived_at IS NULL
 ORDER BY is_default DESC, lower(slug), created_at ASC;
