CREATE TYPE task_schedule_type AS ENUM (
    'imperative',
    'declarative'
);

CREATE TABLE task_schedules (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    schedule_type task_schedule_type NOT NULL DEFAULT 'imperative',
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    dedup_key TEXT NOT NULL CHECK (btrim(dedup_key) <> ''),
    user_dedup_key TEXT CHECK (user_dedup_key IS NULL OR btrim(user_dedup_key) <> ''),
    external_id TEXT,
    cron TEXT NOT NULL CHECK (btrim(cron) <> ''),
    timezone TEXT NOT NULL DEFAULT 'UTC' CHECK (btrim(timezone) <> ''),
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT task_schedules_scope_id_key UNIQUE (org_id, project_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE
);

CREATE UNIQUE INDEX task_schedules_internal_dedup_active_idx
    ON task_schedules (org_id, project_id, schedule_type, dedup_key);

CREATE UNIQUE INDEX task_schedules_user_dedup_active_idx
    ON task_schedules (org_id, project_id, user_dedup_key)
    WHERE user_dedup_key IS NOT NULL;

CREATE TABLE task_schedule_instances (
    id UUID PRIMARY KEY,
    schedule_id UUID NOT NULL,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    active BOOLEAN NOT NULL DEFAULT true,
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    next_fire_at TIMESTAMPTZ,
    last_fire_at TIMESTAMPTZ,
    retry_after TIMESTAMPTZ,
    trigger_attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (trigger_attempt_count >= 0),
    trigger_error_kind TEXT NOT NULL DEFAULT '',
    trigger_error_message TEXT NOT NULL DEFAULT '',
    last_trigger_run_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (schedule_id, environment_id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE CASCADE,
    CONSTRAINT task_schedule_instances_scope_schedule_fkey
        FOREIGN KEY (org_id, project_id, schedule_id)
        REFERENCES task_schedules(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE INDEX task_schedules_scope_created_idx
    ON task_schedules (org_id, project_id, created_at DESC, id DESC);

CREATE INDEX task_schedule_instances_environment_idx
    ON task_schedule_instances (org_id, project_id, environment_id, active);

CREATE INDEX task_schedule_instances_index_due_idx
    ON task_schedule_instances (coalesce(retry_after, next_fire_at), id)
    WHERE active AND next_fire_at IS NOT NULL;

CREATE FUNCTION delete_orphan_task_schedule_after_instance_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    DELETE FROM task_schedules
     WHERE id = OLD.schedule_id
       AND NOT EXISTS (
           SELECT 1
             FROM task_schedule_instances
            WHERE schedule_id = OLD.schedule_id
       );
    RETURN OLD;
END;
$$;

CREATE TRIGGER task_schedule_instances_delete_orphan_schedule
AFTER DELETE ON task_schedule_instances
FOR EACH ROW
EXECUTE FUNCTION delete_orphan_task_schedule_after_instance_delete();

ALTER TABLE runs
    ADD COLUMN schedule_id UUID,
    ADD COLUMN schedule_instance_id UUID,
    ADD COLUMN scheduled_at TIMESTAMPTZ,
    ADD CONSTRAINT runs_schedule_id_fkey
        FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE SET NULL (schedule_id),
    ADD CONSTRAINT runs_schedule_instance_id_fkey
        FOREIGN KEY (org_id, project_id, environment_id, schedule_instance_id)
        REFERENCES task_schedule_instances(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (schedule_instance_id);

CREATE INDEX runs_schedule_idx
    ON runs (org_id, project_id, environment_id, schedule_id, created_at DESC)
    WHERE schedule_id IS NOT NULL;

CREATE INDEX runs_schedule_id_idx
    ON runs (schedule_id)
    WHERE schedule_id IS NOT NULL;

CREATE INDEX runs_schedule_instance_id_idx
    ON runs (org_id, project_id, environment_id, schedule_instance_id)
    WHERE schedule_instance_id IS NOT NULL;
