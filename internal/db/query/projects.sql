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
SELECT projects.*
  FROM projects
  JOIN project ON project.org_id = projects.org_id
              AND project.id = projects.id
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

-- name: ArchiveProjectWithEnvironments :one
WITH active_projects AS (
    SELECT projects.id
      FROM projects
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.archived_at IS NULL
     FOR UPDATE
),
archived_project AS (
    UPDATE projects
       SET archived_at = now()
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.id = sqlc.arg(id)
       AND projects.archived_at IS NULL
       AND projects.is_default = false
       AND EXISTS (
           SELECT 1
             FROM active_projects
            WHERE active_projects.id = projects.id
       )
       AND (
           SELECT count(*)::int
             FROM active_projects
       ) > 1
    RETURNING *
),
archived_environments AS (
    UPDATE environments
       SET archived_at = now()
      FROM archived_project
     WHERE environments.org_id = archived_project.org_id
       AND environments.project_id = archived_project.id
       AND environments.archived_at IS NULL
    RETURNING environments.id
)
SELECT projects.*
  FROM projects
  JOIN archived_project ON archived_project.org_id = projects.org_id
                       AND archived_project.id = projects.id;

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

-- name: UpdateEnvironmentDetails :one
UPDATE environments
   SET slug = sqlc.arg(slug),
       name = sqlc.arg(name)
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(id)
   AND archived_at IS NULL
RETURNING *;

-- name: ArchiveEnvironment :one
WITH active_environments AS (
    SELECT environments.id
      FROM environments
     WHERE environments.org_id = sqlc.arg(org_id)
       AND environments.project_id = sqlc.arg(project_id)
       AND environments.archived_at IS NULL
     FOR UPDATE
)
UPDATE environments
   SET archived_at = now()
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND environments.id = sqlc.arg(id)
   AND environments.archived_at IS NULL
   AND environments.is_default = false
   AND EXISTS (
       SELECT 1
         FROM active_environments
        WHERE active_environments.id = environments.id
   )
   AND (
       SELECT count(*)::int
         FROM active_environments
   ) > 1
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
