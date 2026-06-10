DROP INDEX IF EXISTS run_execution_sessions_worker_group_idx;
DROP INDEX IF EXISTS run_runtime_requirements_worker_scope_idx;
DROP INDEX IF EXISTS run_runtime_requirements_worker_group_idx;
DROP INDEX IF EXISTS deployments_worker_group_status_idx;
DROP INDEX IF EXISTS worker_instances_worker_group_status_seen_idx;
DROP INDEX IF EXISTS deployments_reusable_build_key_idx;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM deployments
         WHERE status IN ('queued', 'building')
         GROUP BY org_id, project_id, environment_id, content_hash
        HAVING count(DISTINCT worker_group_id) > 1
    ) THEN
        RAISE EXCEPTION 'cannot remove worker groups while reusable deployment build keys differ only by worker_group_id';
    END IF;
END;
$$;

ALTER TABLE run_execution_sessions
    DROP CONSTRAINT IF EXISTS run_execution_sessions_worker_instance_group_fk;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_build_worker_instance_group_fk;

ALTER TABLE worker_instances
    DROP CONSTRAINT IF EXISTS worker_instances_worker_group_resource_id_key,
    DROP CONSTRAINT IF EXISTS worker_instances_id_worker_group_key,
    ADD CONSTRAINT worker_instances_resource_id_key UNIQUE (resource_id);

ALTER TABLE run_execution_sessions
    DROP CONSTRAINT IF EXISTS run_execution_sessions_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE run_runtime_requirements
    DROP CONSTRAINT IF EXISTS run_runtime_requirements_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

CREATE UNIQUE INDEX deployments_reusable_build_key_idx
    ON deployments(org_id, project_id, environment_id, content_hash)
    WHERE status IN ('queued', 'building');

ALTER TABLE worker_bootstrap_tokens
    DROP CONSTRAINT IF EXISTS worker_bootstrap_tokens_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

ALTER TABLE worker_instances
    DROP CONSTRAINT IF EXISTS worker_instances_worker_group_fk,
    DROP COLUMN IF EXISTS worker_group_id;

DROP TRIGGER IF EXISTS worker_groups_set_updated_at ON worker_groups;
DROP TABLE IF EXISTS worker_groups;
