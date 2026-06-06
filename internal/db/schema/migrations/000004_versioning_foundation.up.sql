ALTER TABLE deployments
    ADD COLUMN api_version TEXT NOT NULL DEFAULT '2026-06-06' CHECK (btrim(api_version) <> ''),
    ADD COLUMN sdk_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN cli_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN bundle_format_version INTEGER NOT NULL DEFAULT 1 CHECK (bundle_format_version > 0),
    ADD COLUMN worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (btrim(worker_protocol_version) <> '');

ALTER TABLE deployment_tasks
    ADD COLUMN bundle_format_version INTEGER NOT NULL DEFAULT 1 CHECK (bundle_format_version > 0);

ALTER TABLE runs
    ADD COLUMN deployment_version TEXT NOT NULL DEFAULT 'unknown' CHECK (btrim(deployment_version) <> ''),
    ADD COLUMN api_version TEXT NOT NULL DEFAULT '2026-06-06' CHECK (btrim(api_version) <> ''),
    ADD COLUMN sdk_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN cli_version TEXT NOT NULL DEFAULT '';

UPDATE runs
   SET deployment_version = deployments.version,
       api_version = deployments.api_version,
       sdk_version = deployments.sdk_version,
       cli_version = deployments.cli_version
  FROM deployments
 WHERE runs.org_id = deployments.org_id
   AND runs.project_id = deployments.project_id
   AND runs.environment_id = deployments.environment_id
   AND runs.deployment_id = deployments.id;

ALTER TABLE worker_instances
    ADD COLUMN worker_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (btrim(protocol_version) <> ''),
    ADD COLUMN supported_protocol_versions JSONB NOT NULL DEFAULT '["helmr.worker.v0"]'::jsonb;

ALTER TABLE run_executions
    ADD COLUMN worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (btrim(worker_protocol_version) <> '');
