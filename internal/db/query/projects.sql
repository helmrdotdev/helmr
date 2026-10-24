-- name: CreateProject :one
INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(default_region_id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(is_default)
)
RETURNING *;

-- name: CreateProjectWithDefaultEnvironment :one
WITH project AS (
    INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default)
    VALUES (
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(default_region_id),
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
    INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default,
        max_outstanding_admissions, max_causal_depth, max_resident_computers, max_cpu_millis, max_memory_bytes, max_reserved_storage_bytes, preparation_timeout_ms, admission_rate_per_second, admission_burst, admission_tokens, admission_refilled_at)
    SELECT initial_environment.id, project.org_id, project.id, initial_environment.slug, initial_environment.name, initial_environment.color_hex, initial_environment.is_default,
        sqlc.arg(max_outstanding_admissions), sqlc.arg(max_causal_depth), sqlc.arg(max_resident_computers), sqlc.arg(max_cpu_millis), sqlc.arg(max_memory_bytes), sqlc.arg(max_reserved_storage_bytes), sqlc.arg(preparation_timeout_ms), sqlc.arg(admission_rate_per_second), sqlc.arg(admission_burst)::bigint, sqlc.arg(admission_burst)::bigint::numeric, clock_timestamp()
      FROM project
      CROSS JOIN (
          VALUES
              (sqlc.arg(environment_id)::uuid, 'production'::text, 'Production'::text, '#315FCE'::text, true),
              (sqlc.arg(staging_environment_id)::uuid, 'staging'::text, 'Staging'::text, '#F59E0B'::text, false)
      ) AS initial_environment(id, slug, name, color_hex, is_default)
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
   SET name = sqlc.arg(name),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: ListProjects :many
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND (
       NOT sqlc.arg(has_after)::boolean
       OR ((NOT is_default), slug, id) >
          ((NOT sqlc.arg(after_is_default)::boolean), sqlc.arg(after_slug)::text, sqlc.arg(after_id)::uuid)
   )
 ORDER BY is_default DESC, slug ASC, id ASC
 LIMIT sqlc.arg(row_limit);

-- name: CreateEnvironment :one
INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default,
    max_outstanding_admissions, max_causal_depth, max_resident_computers, max_cpu_millis, max_memory_bytes, max_reserved_storage_bytes, preparation_timeout_ms, admission_rate_per_second, admission_burst, admission_tokens, admission_refilled_at)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(color_hex),
    sqlc.arg(is_default),
    sqlc.arg(max_outstanding_admissions), sqlc.arg(max_causal_depth), sqlc.arg(max_resident_computers), sqlc.arg(max_cpu_millis), sqlc.arg(max_memory_bytes), sqlc.arg(max_reserved_storage_bytes), sqlc.arg(preparation_timeout_ms), sqlc.arg(admission_rate_per_second), sqlc.arg(admission_burst)::bigint, sqlc.arg(admission_burst)::bigint::numeric, clock_timestamp()
)
RETURNING *;

-- name: UpdateEnvironmentDetails :one
UPDATE environments
   SET name = sqlc.arg(name),
       color_hex = sqlc.arg(color_hex),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: GetEnvironment :one
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(id);

-- name: ListEnvironments :many
SELECT *
  FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
 ORDER BY is_default DESC, lower(slug), created_at ASC;

-- name: GetEnvironmentBySlug :one
SELECT * FROM environments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND slug = sqlc.arg(slug);

-- name: ListAuthorizedProjects :many
SELECT *
  FROM projects
 WHERE org_id = sqlc.arg(org_id)
   AND (sqlc.arg(all_projects)::boolean OR id=ANY(sqlc.arg(project_ids)::uuid[]))
   AND (
       NOT sqlc.arg(has_after)::boolean
       OR ((NOT is_default), slug, id) >
          ((NOT sqlc.arg(after_is_default)::boolean), sqlc.arg(after_slug)::text, sqlc.arg(after_id)::uuid)
   )
 ORDER BY is_default DESC, slug ASC, id ASC
 LIMIT sqlc.arg(row_limit);
