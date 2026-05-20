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
        false
    )
    RETURNING *
),
environment AS (
    INSERT INTO environments (id, org_id, project_id, slug, name, is_default)
    SELECT sqlc.arg(environment_id), project.org_id, project.id, 'default', 'Default', true
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
           project.slug || '/default',
           '',
           '{}'::jsonb,
           '{}'::jsonb
      FROM project
      JOIN environment ON true
    RETURNING id
)
SELECT project.*
  FROM project
  JOIN environment ON true
  JOIN worker_group ON true;

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

-- name: ListEnvironments :many
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND archived_at IS NULL
 ORDER BY is_default DESC, lower(slug), created_at ASC;
