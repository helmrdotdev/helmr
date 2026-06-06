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
        )
    )
    RETURNING *
),
environment AS (
    INSERT INTO environments (id, org_id, project_id, slug, name, is_default)
    SELECT initial_environment.id, project.org_id, project.id, initial_environment.slug, initial_environment.name, initial_environment.is_default
      FROM project
      CROSS JOIN (
          VALUES
              (sqlc.arg(environment_id)::uuid, 'production'::text, 'Production'::text, true),
              (sqlc.arg(staging_environment_id)::uuid, 'staging'::text, 'Staging'::text, false)
      ) AS initial_environment(id, slug, name, is_default)
    RETURNING id
)
SELECT project.*
  FROM project
 WHERE (SELECT count(*) FROM environment) = 2;

-- name: GetProject :one
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: GetProjectBySlug :one
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND slug = sqlc.arg(slug);

-- name: UpdateProjectDetails :one
UPDATE projects
   SET slug = sqlc.arg(slug),
       name = sqlc.arg(name)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: DeleteProject :one
DELETE FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: ClearDefaultProject :execrows
UPDATE projects
   SET is_default = false
 WHERE org_id = sqlc.arg(org_id)
   AND is_default;

-- name: SetDefaultProject :execrows
UPDATE projects
   SET is_default = true
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: ListProjects :many
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
 ORDER BY is_default DESC, lower(slug), created_at ASC;

-- name: ListProjectsForUpdate :many
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
 ORDER BY is_default DESC, lower(slug), created_at ASC
 FOR UPDATE;

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

-- name: UpdateEnvironmentDetails :one
UPDATE environments
   SET slug = sqlc.arg(slug),
       name = sqlc.arg(name)
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: DeleteEnvironment :one
DELETE FROM environments
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND environments.id = sqlc.arg(id)
   AND environments.slug NOT IN ('production', 'staging')
RETURNING *;

-- name: GetEnvironment :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(id);

-- name: GetEnvironmentBySlug :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND slug = sqlc.arg(slug);

-- name: GetDefaultEnvironment :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND is_default
 LIMIT 1;

-- name: ListEnvironments :many
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
 ORDER BY is_default DESC, lower(slug), created_at ASC;
