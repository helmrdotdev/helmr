CREATE TYPE task_schedule_fire_status AS ENUM (
    'pending',
    'leased',
    'created',
    'failed',
    'exhausted',
    'superseded'
);

CREATE TYPE task_schedule_type AS ENUM (
    'imperative',
    'declarative'
);

CREATE TYPE task_schedule_generator_type AS ENUM (
    'cron'
);

CREATE TABLE task_schedules (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    schedule_type task_schedule_type NOT NULL DEFAULT 'imperative',
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    dedup_key TEXT NOT NULL CHECK (btrim(dedup_key) <> ''),
    external_id TEXT,
    generator_type task_schedule_generator_type NOT NULL DEFAULT 'cron',
    generator_expression TEXT NOT NULL CHECK (btrim(generator_expression) <> ''),
    generator_description TEXT NOT NULL DEFAULT '',
    timezone TEXT NOT NULL DEFAULT 'UTC' CHECK (btrim(timezone) <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_bindings JSONB NOT NULL DEFAULT '{}'::jsonb,
    workspace JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    active BOOLEAN NOT NULL DEFAULT true,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE
);

CREATE UNIQUE INDEX task_schedules_dedup_active_idx
    ON task_schedules (org_id, project_id, dedup_key)
    WHERE deleted_at IS NULL;

CREATE TABLE task_schedule_instances (
    id UUID PRIMARY KEY,
    schedule_id UUID NOT NULL,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    active BOOLEAN NOT NULL DEFAULT true,
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    next_scheduled_at TIMESTAMPTZ,
    next_due_at TIMESTAMPTZ,
    last_scheduled_at TIMESTAMPTZ,
    materialize_lease_id UUID,
    materialize_lease_expires_at TIMESTAMPTZ,
    materialize_attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (materialize_attempt_count >= 0),
    materialize_error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((materialize_lease_id IS NULL) = (materialize_lease_expires_at IS NULL)),
    UNIQUE (schedule_id, environment_id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE CASCADE,
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

CREATE INDEX task_schedule_instances_due_unleased_idx
    ON task_schedule_instances (next_due_at, id)
    WHERE active AND next_due_at IS NOT NULL AND materialize_lease_id IS NULL;

CREATE INDEX task_schedule_instances_due_leased_idx
    ON task_schedule_instances (materialize_lease_expires_at, next_due_at, id)
    WHERE active AND next_due_at IS NOT NULL AND materialize_lease_id IS NOT NULL;

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
    retention_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (status = 'leased' AND lease_id IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'leased' AND lease_id IS NULL AND lease_expires_at IS NULL)
    ),
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

CREATE INDEX task_schedule_fires_schedule_idx
    ON task_schedule_fires (schedule_id);

CREATE INDEX task_schedule_fires_run_idx
    ON task_schedule_fires (org_id, run_id)
    WHERE run_id IS NOT NULL;

CREATE INDEX task_schedule_fires_pending_claim_idx
    ON task_schedule_fires (next_attempt_at, scheduled_at)
    WHERE status IN ('pending', 'failed');

CREATE INDEX task_schedule_fires_leased_claim_idx
    ON task_schedule_fires (lease_expires_at, scheduled_at)
    WHERE status = 'leased';

CREATE INDEX task_schedule_fires_retention_idx
    ON task_schedule_fires (retention_expires_at, schedule_instance_id, scheduled_at)
    WHERE retention_expires_at IS NOT NULL;

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

CREATE INDEX runs_schedule_id_idx
    ON runs (schedule_id)
    WHERE schedule_id IS NOT NULL;

CREATE INDEX runs_schedule_instance_id_idx
    ON runs (org_id, project_id, environment_id, schedule_instance_id)
    WHERE schedule_instance_id IS NOT NULL;
