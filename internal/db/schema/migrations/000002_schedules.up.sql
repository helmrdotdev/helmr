CREATE TYPE task_schedule_fire_status AS ENUM (
    'pending',
    'leased',
    'created',
    'failed',
    'superseded'
);

CREATE TABLE task_schedules (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    dedup_key TEXT NOT NULL CHECK (btrim(dedup_key) <> ''),
    cron_expression TEXT NOT NULL CHECK (btrim(cron_expression) <> ''),
    timezone TEXT NOT NULL DEFAULT 'UTC' CHECK (btrim(timezone) <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_bindings JSONB NOT NULL DEFAULT '{}'::jsonb,
    workspace JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, environment_id, dedup_key),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE task_schedule_instances (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    schedule_id UUID NOT NULL,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    active BOOLEAN NOT NULL DEFAULT true,
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    next_scheduled_at TIMESTAMPTZ,
    next_due_at TIMESTAMPTZ,
    last_scheduled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (schedule_id, environment_id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE INDEX task_schedule_instances_due_idx
    ON task_schedule_instances (next_due_at, id)
    WHERE active AND next_due_at IS NOT NULL;

CREATE TABLE task_schedule_fires (
    schedule_instance_id UUID NOT NULL,
    scheduled_at TIMESTAMPTZ NOT NULL,
    schedule_id UUID NOT NULL,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_bindings JSONB NOT NULL DEFAULT '{}'::jsonb,
    workspace JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_id UUID,
    status task_schedule_fire_status NOT NULL DEFAULT 'pending',
    lease_id UUID,
    lease_expires_at TIMESTAMPTZ,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    error_message TEXT NOT NULL DEFAULT '',
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schedule_instance_id, scheduled_at),
    FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, schedule_instance_id)
        REFERENCES task_schedule_instances(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE SET NULL (run_id)
);

CREATE INDEX task_schedule_fires_claim_idx
    ON task_schedule_fires (next_attempt_at, scheduled_at)
    WHERE status IN ('pending', 'failed', 'leased');

ALTER TABLE runs
    ADD COLUMN schedule_id UUID,
    ADD COLUMN schedule_instance_id UUID,
    ADD COLUMN scheduled_at TIMESTAMPTZ,
    ADD CONSTRAINT runs_schedule_id_fkey
        FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE SET NULL,
    ADD CONSTRAINT runs_schedule_instance_id_fkey
        FOREIGN KEY (org_id, project_id, environment_id, schedule_instance_id)
        REFERENCES task_schedule_instances(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (schedule_instance_id);

CREATE INDEX runs_schedule_idx
    ON runs (org_id, project_id, environment_id, schedule_id, created_at DESC)
    WHERE schedule_id IS NOT NULL;
