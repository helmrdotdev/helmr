DROP INDEX IF EXISTS run_executions_worker_group_idx;
DROP INDEX IF EXISTS run_runtime_requirements_worker_group_idx;
DROP INDEX IF EXISTS deployments_worker_group_status_idx;
DROP INDEX IF EXISTS worker_instances_worker_group_status_seen_idx;

ALTER TABLE run_executions
    DROP CONSTRAINT IF EXISTS run_executions_worker_instance_group_fk;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_build_worker_instance_group_fk;

ALTER TABLE worker_instances
    DROP CONSTRAINT IF EXISTS worker_instances_worker_group_resource_id_key,
    DROP CONSTRAINT IF EXISTS worker_instances_id_worker_group_key,
    ADD CONSTRAINT worker_instances_resource_id_key UNIQUE (resource_id);

ALTER TABLE run_executions
    DROP CONSTRAINT IF EXISTS run_executions_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE run_runtime_requirements
    DROP CONSTRAINT IF EXISTS run_runtime_requirements_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE worker_bootstrap_tokens
    DROP CONSTRAINT IF EXISTS worker_bootstrap_tokens_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE worker_instances
    DROP CONSTRAINT IF EXISTS worker_instances_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

DROP TRIGGER IF EXISTS worker_groups_set_updated_at ON worker_groups;
DROP TABLE IF EXISTS worker_groups;
