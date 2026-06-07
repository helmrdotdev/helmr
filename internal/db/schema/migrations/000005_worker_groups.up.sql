CREATE TABLE worker_groups (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name)
);

CREATE TRIGGER worker_groups_set_updated_at
    BEFORE UPDATE ON worker_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO worker_groups (id, name, description)
VALUES (uuidv7(), 'default', 'Default worker group')
ON CONFLICT (name) DO UPDATE
   SET description = worker_groups.description;

ALTER TABLE worker_instances
    ADD COLUMN worker_group_id UUID;

UPDATE worker_instances
   SET worker_group_id = (SELECT id FROM worker_groups WHERE name = 'default')
 WHERE worker_group_id IS NULL;

ALTER TABLE worker_instances
    ALTER COLUMN worker_group_id SET NOT NULL,
    ADD CONSTRAINT worker_instances_worker_group_fk
        FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT;

ALTER TABLE worker_bootstrap_tokens
    ADD COLUMN worker_group_id UUID;

UPDATE worker_bootstrap_tokens
   SET worker_group_id = (SELECT id FROM worker_groups WHERE name = 'default')
 WHERE worker_group_id IS NULL;

ALTER TABLE worker_bootstrap_tokens
    ALTER COLUMN worker_group_id SET NOT NULL,
    ADD CONSTRAINT worker_bootstrap_tokens_worker_group_fk
        FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT;

ALTER TABLE deployments
    ADD COLUMN worker_group_id UUID;

UPDATE deployments
   SET worker_group_id = (SELECT id FROM worker_groups WHERE name = 'default')
 WHERE worker_group_id IS NULL;

ALTER TABLE deployments
    ALTER COLUMN worker_group_id SET NOT NULL,
    ADD CONSTRAINT deployments_worker_group_fk
        FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT;

ALTER TABLE run_runtime_requirements
    ADD COLUMN worker_group_id UUID;

UPDATE run_runtime_requirements
   SET worker_group_id = (SELECT id FROM worker_groups WHERE name = 'default')
 WHERE worker_group_id IS NULL;

ALTER TABLE run_runtime_requirements
    ALTER COLUMN worker_group_id SET NOT NULL,
    ADD CONSTRAINT run_runtime_requirements_worker_group_fk
        FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT;

ALTER TABLE run_executions
    ADD COLUMN worker_group_id UUID;

UPDATE run_executions
   SET worker_group_id = worker_instances.worker_group_id
  FROM worker_instances
 WHERE run_executions.worker_instance_id = worker_instances.id
   AND run_executions.worker_group_id IS NULL;

UPDATE run_executions
   SET worker_group_id = (SELECT id FROM worker_groups WHERE name = 'default')
 WHERE worker_group_id IS NULL;

ALTER TABLE run_executions
    ALTER COLUMN worker_group_id SET NOT NULL,
    ADD CONSTRAINT run_executions_worker_group_fk
        FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT;

ALTER TABLE worker_instances
    DROP CONSTRAINT IF EXISTS worker_instances_resource_id_key,
    ADD CONSTRAINT worker_instances_worker_group_resource_id_key UNIQUE (worker_group_id, resource_id),
    ADD CONSTRAINT worker_instances_id_worker_group_key UNIQUE (id, worker_group_id);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_build_worker_instance_group_fk
        FOREIGN KEY (build_worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT;

ALTER TABLE run_executions
    ADD CONSTRAINT run_executions_worker_instance_group_fk
        FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT;

CREATE INDEX worker_instances_worker_group_status_seen_idx
    ON worker_instances(worker_group_id, status, last_seen_at DESC);

CREATE INDEX deployments_worker_group_status_idx
    ON deployments(worker_group_id, status, created_at)
    WHERE status IN ('queued', 'building');

CREATE INDEX run_runtime_requirements_worker_group_idx
    ON run_runtime_requirements(worker_group_id);

CREATE INDEX run_runtime_requirements_worker_scope_idx
    ON run_runtime_requirements(worker_group_id, org_id, run_id);

CREATE INDEX run_executions_worker_group_idx
    ON run_executions(worker_group_id);
