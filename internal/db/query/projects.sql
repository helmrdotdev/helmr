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
)
SELECT project.*
  FROM project
  JOIN environment ON true;

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
