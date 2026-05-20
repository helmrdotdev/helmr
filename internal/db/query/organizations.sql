-- name: EnsureDefaultOrganization :exec
WITH organization AS (
    INSERT INTO organizations (id, slug)
    VALUES ($1, 'default')
    ON CONFLICT (id) DO NOTHING
    RETURNING id
),
target_org AS (
    SELECT id FROM organization
    UNION ALL
    SELECT id FROM organizations WHERE id = $1
    LIMIT 1
),
project AS (
    INSERT INTO projects (org_id, slug, name, is_default)
    SELECT target_org.id, 'default', 'Default', true
      FROM target_org
     WHERE NOT EXISTS (
           SELECT 1
             FROM projects
            WHERE projects.org_id = target_org.id
              AND projects.is_default
              AND projects.archived_at IS NULL
     )
    RETURNING id, org_id
),
target_project AS (
    SELECT id, org_id FROM project
    UNION ALL
    SELECT projects.id, projects.org_id
      FROM projects
      JOIN target_org ON target_org.id = projects.org_id
     WHERE projects.is_default
       AND projects.archived_at IS NULL
    LIMIT 1
),
environment AS (
    INSERT INTO environments (org_id, project_id, slug, name, is_default)
    SELECT target_project.org_id, target_project.id, 'default', 'Default', true
      FROM target_project
     WHERE NOT EXISTS (
           SELECT 1
             FROM environments
            WHERE environments.org_id = target_project.org_id
              AND environments.project_id = target_project.id
              AND environments.is_default
              AND environments.archived_at IS NULL
     )
    RETURNING id, org_id, project_id
),
target_environment AS (
    SELECT id, org_id, project_id FROM environment
    UNION ALL
    SELECT environments.id, environments.org_id, environments.project_id
      FROM environments
      JOIN target_project ON target_project.org_id = environments.org_id
                         AND target_project.id = environments.project_id
     WHERE environments.is_default
       AND environments.archived_at IS NULL
    LIMIT 1
)
INSERT INTO worker_groups (org_id, project_id, environment_id, slug, name, provisioning_mode, queue_name, region, capabilities, metadata)
SELECT target_environment.org_id,
       target_environment.project_id,
       target_environment.id,
       'default',
       'Default',
       'customer_managed',
       'default',
       '',
       '{}'::jsonb,
       '{}'::jsonb
  FROM target_environment
 WHERE NOT EXISTS (
       SELECT 1
         FROM worker_groups
        WHERE worker_groups.org_id = target_environment.org_id
          AND worker_groups.project_id = target_environment.project_id
          AND worker_groups.environment_id = target_environment.id
          AND worker_groups.slug = 'default'
          AND worker_groups.archived_at IS NULL
 )
ON CONFLICT DO NOTHING;

-- name: GetDefaultProjectEnvironment :one
SELECT
    projects.id AS project_id,
    environments.id AS environment_id
  FROM projects
  JOIN environments
    ON environments.org_id = projects.org_id
   AND environments.project_id = projects.id
   AND environments.is_default
   AND environments.archived_at IS NULL
 WHERE projects.org_id = sqlc.arg(org_id)
   AND projects.is_default
   AND projects.archived_at IS NULL
 LIMIT 1;

-- name: ListOrganizationIDs :many
SELECT id
  FROM organizations
 ORDER BY id ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListOrganizationIDsPage :many
SELECT id
  FROM organizations
 WHERE sqlc.narg(after_id)::uuid IS NULL
    OR id > sqlc.narg(after_id)::uuid
 ORDER BY id ASC
 LIMIT sqlc.arg(row_limit);
