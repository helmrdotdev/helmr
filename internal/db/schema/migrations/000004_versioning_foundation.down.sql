ALTER TABLE run_execution_sessions
    DROP COLUMN IF EXISTS worker_protocol_version;

ALTER TABLE worker_instances
    DROP COLUMN IF EXISTS supported_protocol_versions,
    DROP COLUMN IF EXISTS protocol_version,
    DROP COLUMN IF EXISTS worker_version;

ALTER TABLE runs
    DROP COLUMN IF EXISTS cli_version,
    DROP COLUMN IF EXISTS sdk_version,
    DROP COLUMN IF EXISTS api_version,
    DROP COLUMN IF EXISTS deployment_version;

ALTER TABLE deployment_tasks
    DROP COLUMN IF EXISTS bundle_format_version;

ALTER TABLE deployments
    DROP COLUMN IF EXISTS worker_protocol_version,
    DROP COLUMN IF EXISTS bundle_format_version,
    DROP COLUMN IF EXISTS cli_version,
    DROP COLUMN IF EXISTS sdk_version,
    DROP COLUMN IF EXISTS api_version;
