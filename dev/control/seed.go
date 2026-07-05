package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedDevData(ctx context.Context, pool *pgxpool.Pool, cfg devConfig) error {
	_, err := pool.Exec(ctx, devSeedSQL, cfg.cellID, cfg.defaultRegionID)
	return err
}

const devSeedSQL = `
BEGIN;
SET CONSTRAINTS ALL DEFERRED;
SELECT set_config('helmr.seed_cell_id', $1, true),
       set_config('helmr.seed_region_id', $2, true);

INSERT INTO users (id, public_id, display_name, primary_email)
VALUES ('00000000-0000-0000-0000-000000000101', 'usr_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'Local Developer', 'dev@helmr.local')
ON CONFLICT (id) DO UPDATE
   SET display_name = EXCLUDED.display_name,
       primary_email = EXCLUDED.primary_email,
       disabled_at = NULL,
       updated_at = now();

INSERT INTO organizations (id, public_id, name, slug)
VALUES ('00000000-0000-0000-0000-000000000201', 'org_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'Helmr Local', 'local-dev')
ON CONFLICT (id) DO UPDATE
   SET name = EXCLUDED.name,
       slug = EXCLUDED.slug,
       updated_at = now();

INSERT INTO org_members (org_id, user_id, role, display_name)
VALUES ('00000000-0000-0000-0000-000000000201', '00000000-0000-0000-0000-000000000101', 'owner', 'Local Developer')
ON CONFLICT (org_id, user_id) DO UPDATE
   SET role = EXCLUDED.role,
       display_name = EXCLUDED.display_name,
       disabled_at = NULL,
       updated_at = now();

INSERT INTO org_cells (org_id, cell_id, role, state)
VALUES ('00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), 'home', 'active')
ON CONFLICT (org_id, cell_id, role) DO UPDATE
   SET state = EXCLUDED.state,
       updated_at = now();

INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name, is_default)
VALUES ('00000000-0000-0000-0000-000000000301', 'prj_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_region_id'), 'console-demo', 'Console Demo', true)
ON CONFLICT (id) DO UPDATE
   SET default_region_id = EXCLUDED.default_region_id,
       slug = EXCLUDED.slug,
       name = EXCLUDED.name,
       is_default = EXCLUDED.is_default,
       updated_at = now();

INSERT INTO environments (id, public_id, org_id, project_id, default_region_id, slug, name, color_hex, is_default)
VALUES
    ('00000000-0000-0000-0000-000000000401', 'env_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', '00000000-0000-0000-0000-000000000301', current_setting('helmr.seed_region_id'), 'production', 'Production', '#315FCE', true),
    ('00000000-0000-0000-0000-000000000402', 'env_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', '00000000-0000-0000-0000-000000000301', current_setting('helmr.seed_region_id'), 'staging', 'Staging', '#F59E0B', false)
ON CONFLICT (id) DO UPDATE
   SET default_region_id = EXCLUDED.default_region_id,
       slug = EXCLUDED.slug,
       name = EXCLUDED.name,
       color_hex = EXCLUDED.color_hex,
       is_default = EXCLUDED.is_default,
       updated_at = now();

INSERT INTO environment_cells (org_id, project_id, environment_id, region_id, cell_id, route_state, route_generation)
VALUES
    ('00000000-0000-0000-0000-000000000201', '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', current_setting('helmr.seed_region_id'), current_setting('helmr.seed_cell_id'), 'active', 1),
    ('00000000-0000-0000-0000-000000000201', '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000402', current_setting('helmr.seed_region_id'), current_setting('helmr.seed_cell_id'), 'active', 1)
ON CONFLICT (org_id, project_id, environment_id, region_id, route_generation) DO UPDATE
   SET route_state = EXCLUDED.route_state,
       cell_id = EXCLUDED.cell_id,
       updated_at = now();

INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type)
VALUES
    ('00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), 'sha256:dev-deployment-source', 128, 'application/vnd.helmr.dev-source'),
    ('00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), 'sha256:dev-deployment-manifest', 256, 'application/vnd.helmr.deployment-manifest+json'),
    ('00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), 'sha256:dev-task-bundle', 512, 'application/vnd.helmr.task-bundle'),
    ('00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), 'sha256:dev-sandbox-rootfs', 1048576, 'application/vnd.helmr.sandbox-rootfs'),
    ('00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), 'sha256:dev-workspace-version', 1024, 'application/vnd.helmr.workspace-version')
ON CONFLICT (org_id, cell_id, digest) DO UPDATE
   SET size_bytes = EXCLUDED.size_bytes,
       media_type = EXCLUDED.media_type;

INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type)
VALUES
    ('00000000-0000-0000-0000-000000000501', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'sha256:dev-deployment-source', 'deployment_source', 128, 'application/vnd.helmr.dev-source'),
    ('00000000-0000-0000-0000-000000000502', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'sha256:dev-deployment-manifest', 'deployment_manifest', 256, 'application/vnd.helmr.deployment-manifest+json'),
    ('00000000-0000-0000-0000-000000000503', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'sha256:dev-task-bundle', 'task_bundle', 512, 'application/vnd.helmr.task-bundle'),
    ('00000000-0000-0000-0000-000000000504', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'sha256:dev-sandbox-rootfs', 'sandbox_image', 1048576, 'application/vnd.helmr.sandbox-rootfs'),
    ('00000000-0000-0000-0000-000000000505', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'sha256:dev-workspace-version', 'workspace_version', 1024, 'application/vnd.helmr.workspace-version')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       digest = EXCLUDED.digest,
       kind = EXCLUDED.kind,
       size_bytes = EXCLUDED.size_bytes,
       media_type = EXCLUDED.media_type;

INSERT INTO deployments (
    id, public_id, org_id, cell_id, project_id, environment_id, worker_group_id, version, content_hash,
    deployment_source_artifact_id, deployment_manifest_artifact_id, status, built_at, deployed_at
)
SELECT '00000000-0000-0000-0000-000000000601',
       'dep_aaaaaaaaaaaaaaaaaaaaaaaaaa',
       '00000000-0000-0000-0000-000000000201',
       current_setting('helmr.seed_cell_id'),
       '00000000-0000-0000-0000-000000000301',
       '00000000-0000-0000-0000-000000000401',
       worker_groups.id,
       'dev-2026-06-22',
       'sha256:dev-console-demo',
       '00000000-0000-0000-0000-000000000501',
       '00000000-0000-0000-0000-000000000502',
       'deployed',
       now() - interval '3 hours',
       now() - interval '3 hours'
  FROM worker_groups
 WHERE worker_groups.name = 'default'
   AND worker_groups.cell_id = current_setting('helmr.seed_cell_id')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       version = EXCLUDED.version,
       content_hash = EXCLUDED.content_hash,
       status = EXCLUDED.status,
       built_at = EXCLUDED.built_at,
       deployed_at = EXCLUDED.deployed_at,
       updated_at = now();

UPDATE environments
   SET current_deployment_id = '00000000-0000-0000-0000-000000000601',
       updated_at = now()
 WHERE id = '00000000-0000-0000-0000-000000000401';

INSERT INTO deployment_sandboxes (
    id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, sandbox_id,
    image_artifact_id, image_artifact_format, rootfs_digest, image_digest,
    image_format, workspace_mount_path, runtime_abi, guestd_abi, adapter_abi,
    filesystem_format, default_uid, default_gid, default_workdir, contract_version, fingerprint
)
VALUES (
    '00000000-0000-0000-0000-000000000701',
    'sbx_aaaaaaaaaaaaaaaaaaaaaaaaaa',
    '00000000-0000-0000-0000-000000000201',
    current_setting('helmr.seed_cell_id'),
    '00000000-0000-0000-0000-000000000301',
    '00000000-0000-0000-0000-000000000401',
    '00000000-0000-0000-0000-000000000601',
    'node-22',
    '00000000-0000-0000-0000-000000000504',
    'rootfs-tar',
    'sha256:dev-sandbox-rootfs',
    'sha256:dev-sandbox-rootfs',
    'rootfs',
    '/workspace',
    'helmr.runtime.v1',
    'helmr.guestd.v1',
    'helmr.adapter.v1',
    'tar',
    1000,
    1000,
    '/workspace',
    1,
    'sha256:dev-sandbox-contract'
)
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       sandbox_id = EXCLUDED.sandbox_id,
       image_artifact_id = EXCLUDED.image_artifact_id,
       rootfs_digest = EXCLUDED.rootfs_digest,
       image_digest = EXCLUDED.image_digest,
       workspace_mount_path = EXCLUDED.workspace_mount_path,
       fingerprint = EXCLUDED.fingerprint;

INSERT INTO tasks (id, public_id, org_id, cell_id, project_id, environment_id, task_id, metadata)
VALUES
    ('00000000-0000-0000-0000-000000000801', 'task_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'code-review', '{"title":"Review branch"}'),
    ('00000000-0000-0000-0000-000000000802', 'task_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'approval-message', '{"title":"Collect approval"}'),
    ('00000000-0000-0000-0000-000000000803', 'task_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'failure-boundary', '{"title":"Failure example"}'),
    ('00000000-0000-0000-0000-000000000804', 'task_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'release-summary', '{"title":"Release summary"}')
ON CONFLICT (environment_id, task_id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       metadata = EXCLUDED.metadata,
       archived_at = NULL,
       updated_at = now();

INSERT INTO deployment_queues (id, org_id, cell_id, project_id, environment_id, deployment_id, name)
VALUES
    ('00000000-0000-0000-0000-000000000810', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', 'default')
ON CONFLICT (org_id, project_id, environment_id, deployment_id, name) DO NOTHING;

INSERT INTO deployment_tasks (
    id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_sandbox_id, task_id,
    file_path, export_name, handler_entrypoint, bundle_artifact_id, queue_name, max_active_duration_ms
)
VALUES
    ('00000000-0000-0000-0000-000000000811', 'dtask_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000701', 'code-review', 'tasks/code-review.ts', 'default', 'tasks/code-review.ts#default', '00000000-0000-0000-0000-000000000503', 'default', 1800000),
    ('00000000-0000-0000-0000-000000000812', 'dtask_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000701', 'approval-message', 'tasks/approval-message.ts', 'default', 'tasks/approval-message.ts#default', '00000000-0000-0000-0000-000000000503', 'default', 900000),
    ('00000000-0000-0000-0000-000000000813', 'dtask_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000701', 'failure-boundary', 'tasks/failure-boundary.ts', 'default', 'tasks/failure-boundary.ts#default', '00000000-0000-0000-0000-000000000503', 'default', 600000),
    ('00000000-0000-0000-0000-000000000814', 'dtask_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000701', 'release-summary', 'tasks/release-summary.ts', 'default', 'tasks/release-summary.ts#default', '00000000-0000-0000-0000-000000000503', 'default', 1200000)
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       task_id = EXCLUDED.task_id,
       file_path = EXCLUDED.file_path,
       export_name = EXCLUDED.export_name,
       handler_entrypoint = EXCLUDED.handler_entrypoint,
       queue_name = EXCLUDED.queue_name,
       max_active_duration_ms = EXCLUDED.max_active_duration_ms;

INSERT INTO workspaces (id, public_id, org_id, cell_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint, external_id, metadata, tags)
VALUES
    ('00000000-0000-0000-0000-000000000901', 'wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000701', 'node-22', 'sha256:dev-sandbox-contract', 'dev/code-review', '{"purpose":"active review"}', ARRAY['demo']),
    ('00000000-0000-0000-0000-000000000902', 'wsp_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000701', 'node-22', 'sha256:dev-sandbox-contract', 'dev/approval', '{"purpose":"waiting approval"}', ARRAY['demo']),
    ('00000000-0000-0000-0000-000000000903', 'wsp_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000701', 'node-22', 'sha256:dev-sandbox-contract', 'dev/failure', '{"purpose":"failed run"}', ARRAY['demo']),
    ('00000000-0000-0000-0000-000000000904', 'wsp_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000701', 'node-22', 'sha256:dev-sandbox-contract', 'dev/release-summary', '{"purpose":"completed summary"}', ARRAY['demo'])
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       metadata = EXCLUDED.metadata,
       tags = EXCLUDED.tags,
       updated_at = now();

INSERT INTO workspace_versions (id, public_id, org_id, cell_id, project_id, environment_id, workspace_id, kind, state, artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, message, promoted_at, created_by_subject_type, created_by_subject_id)
VALUES
    ('00000000-0000-0000-0000-000000000911', 'wsv_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000901', 'system', 'ready', '00000000-0000-0000-0000-000000000505', 'tar', 8, 'sha256:dev-workspace-version', 1024, 'Initial dev workspace', now() - interval '3 hours', 'system', 'dev-seed'),
    ('00000000-0000-0000-0000-000000000912', 'wsv_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000902', 'system', 'ready', '00000000-0000-0000-0000-000000000505', 'tar', 8, 'sha256:dev-workspace-version', 1024, 'Initial dev workspace', now() - interval '2 hours', 'system', 'dev-seed'),
    ('00000000-0000-0000-0000-000000000913', 'wsv_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000903', 'system', 'ready', '00000000-0000-0000-0000-000000000505', 'tar', 8, 'sha256:dev-workspace-version', 1024, 'Initial dev workspace', now() - interval '90 minutes', 'system', 'dev-seed'),
    ('00000000-0000-0000-0000-000000000914', 'wsv_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000904', 'system', 'ready', '00000000-0000-0000-0000-000000000505', 'tar', 8, 'sha256:dev-workspace-version', 1024, 'Initial dev workspace', now() - interval '1 hour', 'system', 'dev-seed')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       state = EXCLUDED.state,
       artifact_id = EXCLUDED.artifact_id,
       artifact_encoding = EXCLUDED.artifact_encoding,
       content_digest = EXCLUDED.content_digest,
       size_bytes = EXCLUDED.size_bytes,
       promoted_at = EXCLUDED.promoted_at;

UPDATE workspaces
   SET current_version_id = CASE id
       WHEN '00000000-0000-0000-0000-000000000901' THEN '00000000-0000-0000-0000-000000000911'::uuid
       WHEN '00000000-0000-0000-0000-000000000902' THEN '00000000-0000-0000-0000-000000000912'::uuid
       WHEN '00000000-0000-0000-0000-000000000903' THEN '00000000-0000-0000-0000-000000000913'::uuid
       WHEN '00000000-0000-0000-0000-000000000904' THEN '00000000-0000-0000-0000-000000000914'::uuid
   END,
   updated_at = now()
 WHERE id IN (
    '00000000-0000-0000-0000-000000000901',
    '00000000-0000-0000-0000-000000000902',
    '00000000-0000-0000-0000-000000000903',
    '00000000-0000-0000-0000-000000000904'
 );

INSERT INTO sessions (
    id, public_id, org_id, cell_id, project_id, environment_id, task_id, initial_deployment_id,
    active_deployment_id, status, current_run_id, workspace_id, metadata, tags,
    closed_at, closed_reason, terminal_reason, result, created_at, updated_at
)
VALUES
    ('00000000-0000-0000-0000-000000001001', 'ses_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'code-review', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000601', 'open', '00000000-0000-0000-0000-000000001101', '00000000-0000-0000-0000-000000000901', '{"branch":"feature/workspace-runtime"}', ARRAY['demo','review'], NULL, '', '{}', NULL, now() - interval '45 minutes', now() - interval '3 minutes'),
    ('00000000-0000-0000-0000-000000001002', 'ses_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'approval-message', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000601', 'open', '00000000-0000-0000-0000-000000001102', '00000000-0000-0000-0000-000000000902', '{"channel":"approval"}', ARRAY['demo','waiting'], NULL, '', '{}', NULL, now() - interval '35 minutes', now() - interval '18 minutes'),
    ('00000000-0000-0000-0000-000000001003', 'ses_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'failure-boundary', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000601', 'failed', NULL, '00000000-0000-0000-0000-000000000903', '{"case":"bad input"}', ARRAY['demo','failed'], now() - interval '21 minutes', 'failed', '{"kind":"task_failed"}', '{"ok":false}', now() - interval '30 minutes', now() - interval '21 minutes'),
    ('00000000-0000-0000-0000-000000001004', 'ses_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'release-summary', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000601', 'completed', NULL, '00000000-0000-0000-0000-000000000904', '{"release":"dev-2026-06-22"}', ARRAY['demo','completed'], now() - interval '12 minutes', 'completed', '{"kind":"completed"}', '{"ok":true}', now() - interval '24 minutes', now() - interval '12 minutes')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       status = EXCLUDED.status,
       current_run_id = EXCLUDED.current_run_id,
       metadata = EXCLUDED.metadata,
       tags = EXCLUDED.tags,
       closed_at = EXCLUDED.closed_at,
       closed_reason = EXCLUDED.closed_reason,
       terminal_reason = EXCLUDED.terminal_reason,
       result = EXCLUDED.result,
       updated_at = EXCLUDED.updated_at;

INSERT INTO runs (
    id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id,
    workspace_id, deployment_version, sdk_version, task_id, session_id,
    status, execution_status, terminal_outcome, payload, output, metadata, tags,
    queue_name, priority, max_active_duration_ms, trace_id, root_span_id,
    current_attempt_id, current_attempt_number, exit_code, error_message,
    created_at, updated_at, started_at, finished_at
)
VALUES
    ('00000000-0000-0000-0000-000000001101', 'run_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000811', '00000000-0000-0000-0000-000000000901', 'dev-2026-06-22', 'dev', 'code-review', '00000000-0000-0000-0000-000000001001', 'running', 'executing', NULL, '{"repository":"helmr"}', NULL, '{"summary":"Reviewing workspace runtime branch"}', ARRAY['demo','review'], 'default', 10, 1800000, '11111111111111111111111111111111', '1111111111111111', '00000000-0000-0000-0000-000000001201', 1, NULL, NULL, now() - interval '45 minutes', now() - interval '3 minutes', now() - interval '44 minutes', NULL),
    ('00000000-0000-0000-0000-000000001102', 'run_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000812', '00000000-0000-0000-0000-000000000902', 'dev-2026-06-22', 'dev', 'approval-message', '00000000-0000-0000-0000-000000001002', 'waiting', 'waiting', NULL, '{"message":"Approve deployment?"}', NULL, '{"summary":"Waiting for approval input"}', ARRAY['demo','waiting'], 'default', 5, 900000, '22222222222222222222222222222222', '2222222222222222', '00000000-0000-0000-0000-000000001202', 1, NULL, NULL, now() - interval '35 minutes', now() - interval '18 minutes', now() - interval '34 minutes', NULL),
    ('00000000-0000-0000-0000-000000001103', 'run_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000813', '00000000-0000-0000-0000-000000000903', 'dev-2026-06-22', 'dev', 'failure-boundary', '00000000-0000-0000-0000-000000001003', 'failed', 'finished', 'failed', '{"mode":"failure"}', '{"ok":false,"error":"fixture failure"}', '{"summary":"Failed before producing output"}', ARRAY['demo','failed'], 'default', 0, 600000, '33333333333333333333333333333333', '3333333333333333', '00000000-0000-0000-0000-000000001203', 1, 1, 'fixture failure', now() - interval '30 minutes', now() - interval '21 minutes', now() - interval '29 minutes', now() - interval '21 minutes'),
    ('00000000-0000-0000-0000-000000001104', 'run_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000000601', '00000000-0000-0000-0000-000000000814', '00000000-0000-0000-0000-000000000904', 'dev-2026-06-22', 'dev', 'release-summary', '00000000-0000-0000-0000-000000001004', 'succeeded', 'finished', 'succeeded', '{"release":"dev-2026-06-22"}', '{"ok":true,"summary":"Release notes generated"}', '{"summary":"Generated release summary"}', ARRAY['demo','completed'], 'default', 0, 1200000, '44444444444444444444444444444444', '4444444444444444', '00000000-0000-0000-0000-000000001204', 1, 0, NULL, now() - interval '24 minutes', now() - interval '12 minutes', now() - interval '23 minutes', now() - interval '12 minutes')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       status = EXCLUDED.status,
       execution_status = EXCLUDED.execution_status,
       terminal_outcome = EXCLUDED.terminal_outcome,
       output = EXCLUDED.output,
       metadata = EXCLUDED.metadata,
       tags = EXCLUDED.tags,
       exit_code = EXCLUDED.exit_code,
       error_message = EXCLUDED.error_message,
       updated_at = EXCLUDED.updated_at,
       started_at = EXCLUDED.started_at,
       finished_at = EXCLUDED.finished_at;

INSERT INTO run_attempts (id, org_id, cell_id, run_id, attempt_number, status, output, error_message, created_at, updated_at, started_at, finished_at)
VALUES
    ('00000000-0000-0000-0000-000000001201', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000001101', 1, 'running', NULL, NULL, now() - interval '45 minutes', now() - interval '3 minutes', now() - interval '44 minutes', NULL),
    ('00000000-0000-0000-0000-000000001202', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000001102', 1, 'waiting', NULL, NULL, now() - interval '35 minutes', now() - interval '18 minutes', now() - interval '34 minutes', NULL),
    ('00000000-0000-0000-0000-000000001203', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000001103', 1, 'failed', '{"ok":false}', 'fixture failure', now() - interval '30 minutes', now() - interval '21 minutes', now() - interval '29 minutes', now() - interval '21 minutes'),
    ('00000000-0000-0000-0000-000000001204', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000001104', 1, 'succeeded', '{"ok":true}', NULL, now() - interval '24 minutes', now() - interval '12 minutes', now() - interval '23 minutes', now() - interval '12 minutes')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       status = EXCLUDED.status,
       output = EXCLUDED.output,
       error_message = EXCLUDED.error_message,
       updated_at = EXCLUDED.updated_at,
       started_at = EXCLUDED.started_at,
       finished_at = EXCLUDED.finished_at;

INSERT INTO session_runs (id, public_id, org_id, cell_id, project_id, environment_id, session_id, run_id, deployment_id, turn_index, reason, ended_at)
VALUES
    ('00000000-0000-0000-0000-000000001301', 'srun_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000001001', '00000000-0000-0000-0000-000000001101', '00000000-0000-0000-0000-000000000601', 0, 'initial', NULL),
    ('00000000-0000-0000-0000-000000001302', 'srun_aaaaaaaaaaaaaaaaaaaaaaaaab', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000001002', '00000000-0000-0000-0000-000000001102', '00000000-0000-0000-0000-000000000601', 0, 'initial', NULL),
    ('00000000-0000-0000-0000-000000001303', 'srun_aaaaaaaaaaaaaaaaaaaaaaaaac', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000001003', '00000000-0000-0000-0000-000000001103', '00000000-0000-0000-0000-000000000601', 0, 'initial', now() - interval '21 minutes'),
    ('00000000-0000-0000-0000-000000001304', 'srun_aaaaaaaaaaaaaaaaaaaaaaaaad', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', '00000000-0000-0000-0000-000000001004', '00000000-0000-0000-0000-000000001104', '00000000-0000-0000-0000-000000000601', 0, 'initial', now() - interval '12 minutes')
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       reason = EXCLUDED.reason,
       ended_at = EXCLUDED.ended_at;

INSERT INTO task_schedules (id, public_id, org_id, cell_id, project_id, schedule_type, task_id, dedup_key, user_dedup_key, external_id, cron, timezone, active)
VALUES ('00000000-0000-0000-0000-000000001401', 'sch_aaaaaaaaaaaaaaaaaaaaaaaaaa', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', 'imperative', 'release-summary', 'dev-release-summary', 'dev-release-summary', 'dev/release-summary', '*/15 * * * *', 'UTC', true)
ON CONFLICT (id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       task_id = EXCLUDED.task_id,
       cron = EXCLUDED.cron,
       timezone = EXCLUDED.timezone,
       active = EXCLUDED.active,
       updated_at = now();

INSERT INTO task_schedule_instances (id, schedule_id, org_id, cell_id, project_id, environment_id, task_id, run_options, active, next_fire_at, last_fire_at, last_trigger_run_id)
VALUES ('00000000-0000-0000-0000-000000001402', '00000000-0000-0000-0000-000000001401', '00000000-0000-0000-0000-000000000201', current_setting('helmr.seed_cell_id'), '00000000-0000-0000-0000-000000000301', '00000000-0000-0000-0000-000000000401', 'release-summary', '{"source":"dev-seed"}', true, now() + interval '11 minutes', now() - interval '4 minutes', '00000000-0000-0000-0000-000000001104')
ON CONFLICT (schedule_id, environment_id) DO UPDATE
   SET cell_id = EXCLUDED.cell_id,
       task_id = EXCLUDED.task_id,
       run_options = EXCLUDED.run_options,
       active = EXCLUDED.active,
       next_fire_at = EXCLUDED.next_fire_at,
       last_fire_at = EXCLUDED.last_fire_at,
       last_trigger_run_id = EXCLUDED.last_trigger_run_id,
       updated_at = now();

COMMIT;
`
