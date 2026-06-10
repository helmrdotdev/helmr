CREATE TABLE organizations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    slug TEXT NOT NULL UNIQUE CHECK (btrim(slug) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    display_name TEXT NOT NULL CHECK (btrim(display_name) <> ''),
    profile_image_url TEXT CHECK (profile_image_url IS NULL OR btrim(profile_image_url) <> ''),
    primary_email TEXT,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_primary_email_lower_idx
    ON users (lower(primary_email))
    WHERE primary_email IS NOT NULL AND disabled_at IS NULL;

CREATE TABLE auth_identities (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (btrim(provider) <> ''),
    subject TEXT NOT NULL CHECK (btrim(subject) <> ''),
    email TEXT,
    claims JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    UNIQUE (provider, subject)
);

CREATE TYPE org_member_role AS ENUM (
    'owner',
    'admin',
    'developer',
    'viewer'
);

CREATE TABLE org_members (
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role org_member_role NOT NULL,
    display_name TEXT,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TYPE deletion_job_status AS ENUM (
    'queued',
    'running',
    'completed',
    'failed'
);

CREATE TABLE deletion_jobs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    target_type TEXT NOT NULL CHECK (target_type IN ('project', 'environment')),
    target_id UUID NOT NULL,
    target_project_id UUID,
    target_slug TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',
    requested_by_principal TEXT NOT NULL DEFAULT '',
    status deletion_job_status NOT NULL DEFAULT 'queued',
    failure TEXT NOT NULL DEFAULT '',
    deleted_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id)
);

CREATE TABLE environments (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    color_hex TEXT NOT NULL CHECK (color_hex ~ '^#[0-9A-Fa-f]{6}$'),
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE
);

CREATE TABLE waitpoint_policies (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    label TEXT NOT NULL DEFAULT '',
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, name),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID REFERENCES organizations(id) ON DELETE SET NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE invitations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    invitee_email TEXT NOT NULL,
    role org_member_role NOT NULL,
    invited_by_user_id UUID,
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    accepted_by_user_id UUID,
    revoked_at TIMESTAMPTZ,
    revoked_by_user_id UUID,
    FOREIGN KEY (org_id, invited_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (invited_by_user_id),
    FOREIGN KEY (org_id, accepted_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (accepted_by_user_id)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, revoked_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (revoked_by_user_id)
);

CREATE TYPE magic_link_purpose AS ENUM (
    'login',
    'invite_accept'
);

CREATE TABLE magic_links (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    purpose magic_link_purpose NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    email TEXT NOT NULL,
    org_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    invitation_id UUID REFERENCES invitations(id) ON DELETE CASCADE,
    redirect_after TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    sent_at TIMESTAMPTZ,
    delivery_failed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    created_by_user_id UUID,
    role org_member_role NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    key_prefix TEXT NOT NULL CHECK (btrim(key_prefix) <> ''),
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, created_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (created_by_user_id)
);

CREATE TABLE api_key_grants (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    api_key_id UUID NOT NULL,
    permission TEXT NOT NULL CHECK (btrim(permission) <> ''),
    created_by_user_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, api_key_id)
        REFERENCES api_keys(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, created_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (created_by_user_id)
);

CREATE TYPE device_code_status AS ENUM (
    'pending',
    'approved',
    'denied',
    'consumed'
);

CREATE TABLE device_codes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    user_code_hash BYTEA NOT NULL UNIQUE,
    device_code_hash BYTEA NOT NULL UNIQUE,
    decided_by_user_id UUID,
    status device_code_status NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    poll_interval_seconds INTEGER NOT NULL CHECK (poll_interval_seconds > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    FOREIGN KEY (org_id, decided_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (decided_by_user_id)
);

CREATE TABLE secrets (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    key_id TEXT NOT NULL CHECK (btrim(key_id) <> ''),
    nonce BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at TIMESTAMPTZ,
    UNIQUE (org_id, project_id, environment_id, name),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE cas_objects (
    digest TEXT PRIMARY KEY,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE artifact_kind AS ENUM (
    'deployment_source',
    'build_manifest',
    'deployment_manifest',
    'task_bundle',
    'checkpoint_runtime_config',
    'checkpoint_vmstate',
    'checkpoint_memory',
    'checkpoint_scratch_disk',
    'checkpoint_workspace'
);

CREATE TYPE worker_instance_status AS ENUM (
    'active',
    'draining',
    'unschedulable',
    'offline'
);

CREATE TABLE runtime_releases (
    runtime_id TEXT PRIMARY KEY CHECK (btrim(runtime_id) <> ''),
    runtime_arch TEXT NOT NULL CHECK (btrim(runtime_arch) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    initramfs_digest TEXT NOT NULL CHECK (btrim(initramfs_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE runtime_release_selections (
    runtime_id TEXT NOT NULL REFERENCES runtime_releases(runtime_id) ON DELETE RESTRICT,
    selected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER runtime_release_selections_set_updated_at
    BEFORE UPDATE ON runtime_release_selections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE worker_instances (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    resource_id TEXT NOT NULL CHECK (btrim(resource_id) <> ''),
    status worker_instance_status NOT NULL DEFAULT 'active',
    region TEXT NOT NULL DEFAULT '',
    total_milli_cpu BIGINT NOT NULL CHECK (total_milli_cpu > 0),
    total_memory_mib BIGINT NOT NULL CHECK (total_memory_mib > 0),
    total_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (total_disk_mib >= 0),
    total_execution_slots INTEGER NOT NULL CHECK (total_execution_slots > 0),
    available_milli_cpu BIGINT NOT NULL CHECK (available_milli_cpu >= 0),
    available_memory_mib BIGINT NOT NULL CHECK (available_memory_mib >= 0),
    available_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (available_disk_mib >= 0),
    available_execution_slots INTEGER NOT NULL CHECK (available_execution_slots >= 0),
    labels JSONB NOT NULL DEFAULT '{}'::jsonb,
    heartbeat JSONB NOT NULL DEFAULT '{}'::jsonb,
    runtime_id TEXT NOT NULL DEFAULT '',
    runtime_arch TEXT NOT NULL DEFAULT '',
    runtime_abi TEXT NOT NULL DEFAULT '',
    kernel_digest TEXT NOT NULL DEFAULT '',
    initramfs_digest TEXT NOT NULL DEFAULT '',
    rootfs_digest TEXT NOT NULL DEFAULT '',
    cni_profile TEXT NOT NULL DEFAULT '',
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    drained_at TIMESTAMPTZ,
    UNIQUE (resource_id)
);

CREATE TABLE worker_bootstrap_tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    last_used_by_worker_instance_id UUID REFERENCES worker_instances(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE worker_instance_credentials (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    worker_instance_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE CASCADE,
    key_prefix TEXT NOT NULL CHECK (btrim(key_prefix) <> ''),
    secret_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    UNIQUE (worker_instance_id, id)
);

CREATE TABLE artifacts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    digest TEXT NOT NULL REFERENCES cas_objects(digest) ON DELETE RESTRICT,
    kind artifact_kind NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL CHECK (btrim(media_type) <> ''),
    created_by_worker_instance_id UUID REFERENCES worker_instances(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TYPE waitpoint_kind AS ENUM (
    'human',
    'delay'
);

CREATE TYPE waitpoint_status AS ENUM (
    'pending',
    'completed',
    'expired',
    'cancelled'
);

CREATE TYPE run_wait_status AS ENUM (
    'opening',
    'waiting',
    'resuming',
    'restored',
    'cancelled',
    'failed'
);

CREATE TYPE waitpoint_response_token_status AS ENUM (
    'pending',
    'completed',
    'revoked'
);

CREATE TYPE waitpoint_delivery_status AS ENUM (
    'queued',
    'sending',
    'retrying',
    'sent',
    'failed'
);

CREATE TYPE checkpoint_status AS ENUM (
    'creating',
    'ready',
    'restoring',
    'invalid'
);

CREATE TYPE run_status AS ENUM (
    'queued',
    'running',
    'waiting',
    'succeeded',
    'failed',
    'cancelled',
    'expired'
);

CREATE TYPE run_execution_status AS ENUM (
    'created',
    'queued',
    'leased',
    'executing',
    'suspended',
    'pending_cancel',
    'finished'
);

CREATE TYPE run_terminal_outcome AS ENUM (
    'succeeded',
    'failed',
    'cancelled',
    'expired',
    'dead_lettered'
);

CREATE TYPE run_execution_session_status AS ENUM (
    'leased',
    'running',
    'detached',
    'released',
    'lost',
    'cancelled'
);

CREATE TYPE run_attempt_status AS ENUM (
    'queued',
    'running',
    'waiting',
    'succeeded',
    'failed',
    'cancelled',
    'expired'
);

CREATE TYPE run_queue_status AS ENUM (
    'queued',
    'published',
    'reserved',
    'suspended',
    'completed',
    'cancelled',
    'dead_lettered'
);

CREATE TYPE run_operation_kind AS ENUM (
    'cancel',
    'replay'
);

CREATE TYPE run_operation_status AS ENUM (
    'requested',
    'applied',
    'rejected'
);

CREATE TYPE run_retry_decision_kind AS ENUM (
    'retry',
    'fail_run',
    'cancel_run'
);

CREATE TYPE deployment_status AS ENUM (
    'queued',
    'building',
    'deployed',
    'failed'
);

CREATE TABLE deployments (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    version TEXT NOT NULL CHECK (btrim(version) <> ''),
    content_hash TEXT NOT NULL CHECK (btrim(content_hash) <> ''),
    deployment_source_artifact_id UUID NOT NULL,
    build_manifest_artifact_id UUID,
    deployment_manifest_artifact_id UUID,
    status deployment_status NOT NULL DEFAULT 'queued',
    failure JSONB NOT NULL DEFAULT '{}'::jsonb,
    build_lease_id TEXT,
    build_worker_instance_id UUID REFERENCES worker_instances(id),
    build_lease_expires_at TIMESTAMPTZ,
    build_attempt INTEGER NOT NULL DEFAULT 0 CHECK (build_attempt >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    building_at TIMESTAMPTZ,
    built_at TIMESTAMPTZ,
    deployed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, version),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_source_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, project_id, environment_id, build_manifest_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_manifest_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE environments
    ADD COLUMN current_deployment_id UUID;

CREATE TABLE deployment_version_counters (
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    prefix TEXT NOT NULL CHECK (btrim(prefix) <> ''),
    next_ordinal INTEGER NOT NULL DEFAULT 2 CHECK (next_ordinal >= 2),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, project_id, environment_id, prefix),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE deployment_promotions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    previous_deployment_id UUID,
    promoted_by_principal TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, previous_deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (previous_deployment_id)
);

ALTER TABLE environments
    ADD CONSTRAINT environments_current_deployment_fk
    FOREIGN KEY (org_id, project_id, id, current_deployment_id)
    REFERENCES deployments(org_id, project_id, environment_id, id)
    ON DELETE SET NULL (current_deployment_id);

CREATE TABLE deployment_tasks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    file_path TEXT NOT NULL DEFAULT '',
    export_name TEXT NOT NULL DEFAULT '',
    handler_entrypoint TEXT NOT NULL DEFAULT '',
    bundle_artifact_id UUID NOT NULL,
    requested_milli_cpu BIGINT NOT NULL DEFAULT 2000 CHECK (requested_milli_cpu > 0),
    requested_memory_mib BIGINT NOT NULL DEFAULT 2048 CHECK (requested_memory_mib > 0),
    secret_declarations JSONB NOT NULL DEFAULT '[]'::jsonb,
    resource_requirements JSONB NOT NULL DEFAULT '{}'::jsonb,
    network_policy JSONB NOT NULL DEFAULT '{"internet": true}'::jsonb,
    schedule_declarations JSONB NOT NULL DEFAULT '[]'::jsonb,
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    queue_concurrency_limit INTEGER,
    ttl TEXT NOT NULL DEFAULT '',
    max_duration_seconds INTEGER NOT NULL CHECK (max_duration_seconds > 0),
    retry_policy JSONB NOT NULL DEFAULT 'false'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, deployment_id, id),
    UNIQUE (org_id, deployment_id, id, task_id),
    UNIQUE (org_id, deployment_id, task_id),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, bundle_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE runs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    deployment_task_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    status run_status NOT NULL DEFAULT 'queued',
    execution_status run_execution_status NOT NULL DEFAULT 'queued',
    terminal_outcome run_terminal_outcome,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    output JSONB,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    idempotency_key TEXT,
    idempotency_key_expires_at TIMESTAMPTZ,
    idempotency_key_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_request_hash TEXT,
    locked_retry_policy JSONB NOT NULL DEFAULT 'false'::jsonb,
    replayed_from_run_id UUID,
    replay_operation_id UUID,
    replay_operation_kind run_operation_kind NOT NULL DEFAULT 'replay' CHECK (replay_operation_kind = 'replay'),
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    queue_concurrency_limit INTEGER,
    concurrency_key TEXT,
    priority INTEGER NOT NULL DEFAULT 0,
    queue_timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl TEXT NOT NULL DEFAULT '',
    queued_expires_at TIMESTAMPTZ,
    max_duration_seconds INTEGER NOT NULL,
    usage_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (usage_duration_ms >= 0),
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    root_span_id TEXT NOT NULL CHECK (root_span_id ~ '^[0-9a-f]{16}$' AND root_span_id <> '0000000000000000'),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    current_attempt_id UUID,
    current_attempt_number INTEGER CHECK (current_attempt_number IS NULL OR current_attempt_number > 0),
    current_session_id UUID,
    latest_checkpoint_id UUID,
    exit_code INTEGER,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, deployment_id, deployment_task_id, task_id)
        REFERENCES deployment_tasks(org_id, deployment_id, id, task_id)
        ON DELETE CASCADE
);

CREATE TABLE run_operations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    kind run_operation_kind NOT NULL,
    status run_operation_status NOT NULL DEFAULT 'requested',
    actor_kind TEXT NOT NULL DEFAULT '',
    actor_id TEXT NOT NULL DEFAULT '',
    api_key_id UUID,
    reason TEXT NOT NULL DEFAULT '',
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    result JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at TIMESTAMPTZ,
    rejected_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, run_id, id, kind),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE UNIQUE INDEX run_operations_idempotency_idx
    ON run_operations (org_id, project_id, environment_id, run_id, kind, idempotency_key)
    WHERE idempotency_key <> '';

ALTER TABLE runs
    ADD CONSTRAINT runs_replayed_from_run_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, replayed_from_run_id)
    REFERENCES runs(org_id, project_id, environment_id, id)
    DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT runs_replay_operation_id_fkey
    FOREIGN KEY (org_id, replayed_from_run_id, replay_operation_id, replay_operation_kind)
    REFERENCES run_operations(org_id, run_id, id, kind)
    ON DELETE SET NULL (replayed_from_run_id, replay_operation_id),
    ADD CONSTRAINT runs_replay_source_operation_pair
    CHECK (
        (replayed_from_run_id IS NULL AND replay_operation_id IS NULL)
        OR (replayed_from_run_id IS NOT NULL AND replay_operation_id IS NOT NULL)
    ),
    ADD CONSTRAINT runs_terminal_outcome_requires_finished
    CHECK (
        (terminal_outcome IS NULL AND status NOT IN ('succeeded', 'failed', 'cancelled', 'expired'))
        OR (
            terminal_outcome IS NOT NULL
            AND (
                execution_status = 'finished'
                OR (terminal_outcome = 'cancelled' AND execution_status = 'pending_cancel')
            )
        )
    );

CREATE TABLE run_attempts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    status run_attempt_status NOT NULL DEFAULT 'queued',
    cause TEXT NOT NULL DEFAULT 'original' CHECK (cause IN ('original', 'auto_retry', 'replay', 'resume', 'system_recovery')),
    previous_attempt_id UUID,
    output JSONB,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, run_id, attempt_number),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE
);

ALTER TABLE run_attempts
    ADD CONSTRAINT run_attempts_previous_attempt_id_fkey
    FOREIGN KEY (org_id, run_id, previous_attempt_id)
    REFERENCES run_attempts(org_id, run_id, id)
    ON DELETE SET NULL;

ALTER TABLE runs
    ADD CONSTRAINT runs_current_attempt_id_fkey
    FOREIGN KEY (org_id, id, current_attempt_id)
    REFERENCES run_attempts(org_id, run_id, id)
    ON DELETE SET NULL (current_attempt_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE run_runtime_requirements (
    run_id UUID PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    org_id UUID NOT NULL,
    requested_milli_cpu BIGINT NOT NULL CHECK (requested_milli_cpu > 0),
    requested_memory_mib BIGINT NOT NULL CHECK (requested_memory_mib > 0),
    requested_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (requested_disk_mib >= 0),
    requested_execution_slots INTEGER NOT NULL DEFAULT 1 CHECK (requested_execution_slots > 0),
    runtime_id TEXT NOT NULL CHECK (btrim(runtime_id) <> ''),
    runtime_arch TEXT NOT NULL CHECK (btrim(runtime_arch) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    initramfs_digest TEXT NOT NULL CHECK (btrim(initramfs_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    network_policy JSONB NOT NULL DEFAULT '{"internet": true}'::jsonb,
    placement JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_id),
    FOREIGN KEY (runtime_id)
        REFERENCES runtime_releases(runtime_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE
);

CREATE TABLE run_queue_items (
    run_id UUID PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    org_id UUID NOT NULL,
    status run_queue_status NOT NULL DEFAULT 'queued',
    priority INTEGER NOT NULL DEFAULT 0,
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    concurrency_key TEXT,
    queue_timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued_expires_at TIMESTAMPTZ,
    dispatch_message_id TEXT,
    reserved_by_worker_instance_id UUID,
    reservation_expires_at TIMESTAMPTZ,
    dispatch_generation BIGINT NOT NULL DEFAULT 0 CHECK (dispatch_generation >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id)
        REFERENCES run_runtime_requirements(org_id, run_id)
        ON DELETE CASCADE,
    FOREIGN KEY (reserved_by_worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE SET NULL (reserved_by_worker_instance_id)
);

CREATE TYPE event_subject_type AS ENUM (
    'run',
    'deployment'
);

CREATE TABLE events (
    id BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    subject_type event_subject_type GENERATED ALWAYS AS (
        CASE
            WHEN run_id IS NOT NULL THEN 'run'::event_subject_type
            WHEN deployment_id IS NOT NULL THEN 'deployment'::event_subject_type
        END
    ) STORED,
    subject_id UUID GENERATED ALWAYS AS (COALESCE(run_id, deployment_id)) STORED,
    seq BIGINT NOT NULL CHECK (seq > 0),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID,
    deployment_id UUID,
    attempt_id UUID,
    session_id UUID,
    attempt_number INTEGER CHECK (attempt_number IS NULL OR attempt_number > 0),
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    parent_span_id TEXT CHECK (parent_span_id IS NULL OR (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000')),
    traceparent TEXT CHECK (
        traceparent IS NULL
        OR (
            span_id IS NOT NULL
            AND traceparent = '00-' || trace_id || '-' || span_id || '-01'
        )
    ),
    category TEXT NOT NULL DEFAULT 'system',
    severity TEXT NOT NULL DEFAULT 'info',
    source TEXT NOT NULL DEFAULT 'control',
    kind TEXT NOT NULL CHECK (btrim(kind) <> ''),
    message TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    redaction_class TEXT NOT NULL DEFAULT 'internal',
    snapshot_version BIGINT CHECK (snapshot_version IS NULL OR snapshot_version > 0),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subject_type, subject_id, seq),
    CHECK (
        (run_id IS NOT NULL AND deployment_id IS NULL)
        OR (deployment_id IS NOT NULL AND run_id IS NULL)
    ),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, attempt_id)
        REFERENCES run_attempts(org_id, run_id, id)
        ON DELETE SET NULL (attempt_id)
);

CREATE INDEX events_scope_created_idx
    ON events (org_id, project_id, environment_id, created_at DESC);

CREATE INDEX events_trace_idx
    ON events (trace_id, created_at)
    WHERE trace_id IS NOT NULL;

CREATE TABLE event_subject_cursors (
    org_id UUID NOT NULL,
    subject_type event_subject_type NOT NULL,
    subject_id UUID NOT NULL,
    last_seq BIGINT NOT NULL CHECK (last_seq > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, subject_type, subject_id)
);

CREATE TABLE event_outbox (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_record_id BIGINT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    stream_key TEXT NOT NULL CHECK (btrim(stream_key) <> ''),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX event_outbox_event_record_id_idx ON event_outbox(event_record_id);
CREATE INDEX event_outbox_ready_idx
    ON event_outbox (created_at, id)
    WHERE published_at IS NULL;

CREATE TYPE run_log_stream AS ENUM (
    'stdout',
    'stderr'
);

CREATE TABLE run_log_chunks (
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    session_id UUID NOT NULL,
    attempt_number INTEGER NOT NULL DEFAULT 1 CHECK (attempt_number > 0),
    stream run_log_stream NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    observed_seq BIGINT NOT NULL CHECK (observed_seq >= 0),
    content BYTEA NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    source TEXT NOT NULL DEFAULT 'worker' CHECK (source IN ('worker', 'guest', 'runtime')),
    redaction_class TEXT NOT NULL DEFAULT 'sensitive' CHECK (redaction_class IN ('public', 'internal', 'sensitive')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, stream, seq),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE
);

CREATE TABLE run_execution_sessions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    worker_instance_id UUID NOT NULL,
    dispatch_message_id TEXT NOT NULL CHECK (btrim(dispatch_message_id) <> ''),
    dispatch_lease_id TEXT NOT NULL CHECK (btrim(dispatch_lease_id) <> ''),
    dispatch_attempt INTEGER NOT NULL CHECK (dispatch_attempt > 0),
    status run_execution_session_status NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    runtime_id TEXT NOT NULL CHECK (btrim(runtime_id) <> ''),
    active_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_duration_ms >= 0),
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    span_id TEXT NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000'),
    parent_span_id TEXT NOT NULL CHECK (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000'),
    traceparent TEXT NOT NULL CHECK (traceparent = '00-' || trace_id || '-' || span_id || '-01'),
    restore_checkpoint_id UUID,
    leased_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (run_id, id),
    FOREIGN KEY (runtime_id)
        REFERENCES runtime_releases(runtime_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, attempt_id)
        REFERENCES run_attempts(org_id, run_id, id)
        ON DELETE CASCADE
);

CREATE TABLE run_snapshots (
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    status run_status NOT NULL,
    execution_status run_execution_status NOT NULL DEFAULT 'queued',
    terminal_outcome run_terminal_outcome,
    attempt_id UUID,
    session_id UUID,
    operation_id UUID,
    previous_version BIGINT CHECK (previous_version IS NULL OR previous_version > 0),
    transition TEXT NOT NULL CHECK (btrim(transition) <> ''),
    reason JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, version),
    UNIQUE (org_id, run_id, version),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, attempt_id)
        REFERENCES run_attempts(org_id, run_id, id)
        ON DELETE SET NULL (attempt_id),
    FOREIGN KEY (org_id, run_id, session_id)
        REFERENCES run_execution_sessions(org_id, run_id, id)
        ON DELETE SET NULL (session_id)
);

ALTER TABLE run_snapshots
    ADD CONSTRAINT run_snapshots_operation_id_fkey
    FOREIGN KEY (org_id, run_id, operation_id)
    REFERENCES run_operations(org_id, run_id, id)
    ON DELETE SET NULL (operation_id);

CREATE TABLE run_retry_decisions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    session_id UUID,
    snapshot_version BIGINT NOT NULL CHECK (snapshot_version > 0),
    decision run_retry_decision_kind NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    error_class TEXT NOT NULL DEFAULT '',
    retry_after TIMESTAMPTZ,
    next_attempt_number INTEGER CHECK (next_attempt_number IS NULL OR next_attempt_number > 0),
    policy_snapshot JSONB NOT NULL DEFAULT 'false'::jsonb,
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_id, attempt_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, attempt_id)
        REFERENCES run_attempts(org_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, session_id)
        REFERENCES run_execution_sessions(org_id, run_id, id)
        ON DELETE SET NULL (session_id),
    FOREIGN KEY (org_id, run_id, snapshot_version)
        REFERENCES run_snapshots(org_id, run_id, version)
        ON DELETE CASCADE
);

CREATE TABLE run_queue_concurrency_leases (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    session_id UUID NOT NULL,
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    concurrency_key TEXT,
    slot_ordinal INTEGER NOT NULL CHECK (slot_ordinal > 0),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, session_id),
    FOREIGN KEY (org_id, run_id, session_id)
        REFERENCES run_execution_sessions(org_id, run_id, id)
        ON DELETE CASCADE
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE run_log_chunks
    ADD CONSTRAINT run_log_chunks_session_id_fkey
    FOREIGN KEY (org_id, run_id, session_id)
    REFERENCES run_execution_sessions(org_id, run_id, id)
    ON DELETE CASCADE;

ALTER TABLE events
    ADD CONSTRAINT events_session_id_fkey
    FOREIGN KEY (org_id, run_id, session_id)
    REFERENCES run_execution_sessions(org_id, run_id, id)
    ON DELETE SET NULL (session_id);

ALTER TABLE runs
    ADD CONSTRAINT runs_current_session_id_fkey
    FOREIGN KEY (org_id, id, current_session_id)
    REFERENCES run_execution_sessions(org_id, run_id, id)
    ON DELETE SET NULL (current_session_id);

CREATE TABLE checkpoints (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    status checkpoint_status NOT NULL DEFAULT 'creating',
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, project_id, environment_id, run_id, id),
    UNIQUE (org_id, run_id, session_id, id),
    FOREIGN KEY (org_id, run_id, session_id)
        REFERENCES run_execution_sessions(org_id, run_id, id)
        ON DELETE CASCADE
);

CREATE TABLE checkpoint_runtime_snapshots (
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    checkpoint_id UUID NOT NULL,
    runtime_backend TEXT NOT NULL CHECK (btrim(runtime_backend) <> ''),
    runtime_id TEXT NOT NULL CHECK (btrim(runtime_id) <> ''),
    runtime_arch TEXT NOT NULL CHECK (btrim(runtime_arch) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    initramfs_digest TEXT NOT NULL CHECK (btrim(initramfs_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    runtime_vcpus INTEGER CHECK (runtime_vcpus IS NULL OR runtime_vcpus > 0),
    runtime_memory_mib INTEGER CHECK (runtime_memory_mib IS NULL OR runtime_memory_mib > 0),
    runtime_scratch_disk_mib INTEGER CHECK (runtime_scratch_disk_mib IS NULL OR runtime_scratch_disk_mib > 0),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    image_key TEXT,
    runtime_config_artifact_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, checkpoint_id),
    FOREIGN KEY (runtime_id)
        REFERENCES runtime_releases(runtime_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, run_id, checkpoint_id)
        REFERENCES checkpoints(org_id, project_id, environment_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, runtime_config_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE checkpoint_workspace_snapshots (
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    checkpoint_id UUID NOT NULL,
    workspace_artifact_id UUID,
    workspace_artifact_encoding TEXT,
    workspace_mount_path TEXT,
    workspace_volume_kind TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, checkpoint_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id, checkpoint_id)
        REFERENCES checkpoints(org_id, project_id, environment_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TYPE checkpoint_artifact_role AS ENUM (
    'runtime_config',
    'runtime_vmstate',
    'runtime_memory',
    'runtime_scratch_disk'
);

CREATE TABLE checkpoint_artifacts (
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    checkpoint_id UUID NOT NULL,
    role checkpoint_artifact_role NOT NULL,
    ordinal INTEGER NOT NULL DEFAULT 0 CHECK (ordinal >= 0),
    artifact_id UUID NOT NULL,
    encrypt_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (encrypt_duration_ms >= 0),
    store_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (store_duration_ms >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, checkpoint_id, role, ordinal),
    FOREIGN KEY (org_id, project_id, environment_id, run_id, checkpoint_id)
        REFERENCES checkpoints(org_id, project_id, environment_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE runs
    ADD CONSTRAINT runs_latest_checkpoint_id_fkey
    FOREIGN KEY (org_id, id, latest_checkpoint_id)
    REFERENCES checkpoints(org_id, run_id, id)
    ON DELETE SET NULL (latest_checkpoint_id);

ALTER TABLE run_execution_sessions
    ADD CONSTRAINT run_execution_sessions_restore_checkpoint_id_fkey
    FOREIGN KEY (org_id, run_id, restore_checkpoint_id)
    REFERENCES checkpoints(org_id, run_id, id)
    ON DELETE SET NULL (restore_checkpoint_id);

CREATE TABLE run_usage_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_id UUID,
    session_id UUID,
    checkpoint_id UUID,
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    source TEXT NOT NULL CHECK (source IN ('control', 'worker', 'guest', 'runtime', 'checkpoint')),
    cause TEXT NOT NULL DEFAULT 'original' CHECK (cause IN ('original', 'auto_retry', 'replay', 'resume', 'cancel_grace', 'system_recovery')),
    snapshot_version BIGINT NOT NULL CHECK (snapshot_version > 0),
    kind TEXT NOT NULL CHECK (kind IN ('active_time', 'log_bytes', 'output_bytes', 'checkpoint_bytes')),
    quantity BIGINT NOT NULL CHECK (quantity >= 0),
    unit TEXT NOT NULL CHECK (unit IN ('ms', 'bytes')),
    measured_to TIMESTAMPTZ,
    attributes JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, id),
    UNIQUE (org_id, run_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, attempt_id)
        REFERENCES run_attempts(org_id, run_id, id)
        ON DELETE SET NULL (attempt_id),
    FOREIGN KEY (org_id, run_id, session_id)
        REFERENCES run_execution_sessions(org_id, run_id, id)
        ON DELETE SET NULL (session_id),
    FOREIGN KEY (org_id, run_id, checkpoint_id)
        REFERENCES checkpoints(org_id, run_id, id)
        ON DELETE SET NULL (checkpoint_id),
    FOREIGN KEY (org_id, run_id, snapshot_version)
        REFERENCES run_snapshots(org_id, run_id, version)
        ON DELETE CASCADE
        DEFERRABLE INITIALLY DEFERRED
);

CREATE UNIQUE INDEX run_usage_events_idempotency_idx
    ON run_usage_events (org_id, run_id, idempotency_key)
    WHERE idempotency_key <> '';

CREATE INDEX run_usage_events_scope_created_idx
    ON run_usage_events (org_id, project_id, environment_id, created_at DESC);

CREATE INDEX run_usage_events_trace_idx
    ON run_usage_events (trace_id, created_at);

CREATE TABLE waitpoints (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    kind waitpoint_kind NOT NULL,
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    display_text TEXT NOT NULL DEFAULT '',
    status waitpoint_status NOT NULL DEFAULT 'pending',
    output JSONB,
    resolution JSONB,
    output_is_error BOOLEAN NOT NULL DEFAULT false,
    resolution_kind TEXT,
    expires_at TIMESTAMPTZ,
    idempotency_key TEXT,
    idempotency_request_hash TEXT,
    idempotency_key_expires_at TIMESTAMPTZ,
    idempotency_key_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id)
);

CREATE UNIQUE INDEX waitpoints_active_idempotency_idx
    ON waitpoints (org_id, project_id, environment_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE run_waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    checkpoint_id UUID NOT NULL,
    correlation_id TEXT NOT NULL,
    status run_wait_status NOT NULL DEFAULT 'opening',
    timeout_seconds INTEGER,
    policy_name TEXT,
    policy_snapshot JSONB,
    active_duration_ms BIGINT NOT NULL DEFAULT 0,
    failure JSONB NOT NULL DEFAULT '{}'::jsonb,
    resolution_kind TEXT,
    resolution JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    waiting_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    restored_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, session_id)
        REFERENCES run_execution_sessions(org_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, session_id, checkpoint_id)
        REFERENCES checkpoints(org_id, run_id, session_id, id)
        ON DELETE CASCADE,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, run_id, id)
);

CREATE TABLE run_wait_dependencies (
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_wait_id UUID NOT NULL,
    waitpoint_id UUID NOT NULL,
    ordinal INTEGER NOT NULL DEFAULT 0,
    dependency_key TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_wait_id, waitpoint_id),
    UNIQUE (org_id, run_wait_id, ordinal),
    UNIQUE (org_id, run_id, run_wait_id, waitpoint_id),
    UNIQUE (org_id, project_id, environment_id, run_wait_id, waitpoint_id),
    FOREIGN KEY (org_id, run_id, run_wait_id)
        REFERENCES run_waits(org_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, run_wait_id)
        REFERENCES run_waits(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, waitpoint_id)
        REFERENCES waitpoints(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, waitpoint_id)
        REFERENCES waitpoints(org_id, id)
        ON DELETE CASCADE
);

CREATE TABLE waitpoint_response_tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    waitpoint_id UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    status waitpoint_response_token_status NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    completed_by_principal TEXT,
    completed_via TEXT,
    external_subject TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, waitpoint_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, waitpoint_id)
        REFERENCES waitpoints(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE waitpoint_responses (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    waitpoint_id UUID NOT NULL,
    response_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    action TEXT NOT NULL,
    resolution_kind TEXT,
    resolution JSONB NOT NULL DEFAULT '{}'::jsonb,
    event_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    completed_by_principal TEXT,
    completed_via TEXT,
    external_subject TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, waitpoint_id, response_key),
    FOREIGN KEY (org_id, project_id, environment_id, waitpoint_id)
        REFERENCES waitpoints(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE waitpoint_deliveries (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    run_wait_id UUID NOT NULL,
    waitpoint_id UUID NOT NULL,
    response_token_id UUID,
    channel TEXT NOT NULL,
    recipient_kind TEXT NOT NULL,
    recipient TEXT NOT NULL,
    status waitpoint_delivery_status NOT NULL DEFAULT 'queued',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_attempt_at TIMESTAMPTZ,
    sending_started_at TIMESTAMPTZ,
    last_error TEXT,
    message_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, run_id, run_wait_id, waitpoint_id)
        REFERENCES run_wait_dependencies(org_id, run_id, run_wait_id, waitpoint_id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, response_token_id)
        REFERENCES waitpoint_response_tokens(org_id, id)
        ON DELETE SET NULL (response_token_id)
);

CREATE UNIQUE INDEX projects_one_default_idx ON projects(org_id)
    WHERE is_default;
CREATE UNIQUE INDEX environments_one_default_idx ON environments(org_id, project_id)
    WHERE is_default;
CREATE UNIQUE INDEX projects_org_slug_idx ON projects(org_id, slug);
CREATE UNIQUE INDEX environments_org_project_slug_idx ON environments(org_id, project_id, slug);
CREATE INDEX deletion_jobs_org_status_requested_idx ON deletion_jobs(org_id, status, requested_at DESC);
CREATE INDEX runs_org_created_idx ON runs(org_id, created_at DESC);
CREATE INDEX runs_org_status_created_idx ON runs(org_id, status, created_at DESC);
CREATE INDEX runs_scope_created_idx ON runs(org_id, project_id, environment_id, created_at DESC);
CREATE INDEX runs_scope_status_created_idx ON runs(org_id, project_id, environment_id, status, created_at DESC);
CREATE UNIQUE INDEX runs_scope_task_idempotency_key_idx
    ON runs(org_id, project_id, environment_id, task_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX runs_queued_expiry_idx
    ON runs(org_id, queued_expires_at)
    WHERE status = 'queued' AND queued_expires_at IS NOT NULL;
CREATE INDEX runs_queued_queue_scope_idx
    ON runs(org_id, project_id, environment_id, queue_name, priority DESC, queue_timestamp, id)
    WHERE status = 'queued' AND current_session_id IS NULL;
CREATE INDEX run_queue_items_status_priority_idx ON run_queue_items(org_id, status, queue_timestamp, priority DESC, enqueued_at)
    WHERE status IN ('queued', 'published', 'reserved');
CREATE INDEX run_queue_items_active_scope_idx
    ON run_queue_items(status, org_id, queue_name, run_id)
    WHERE status IN ('queued', 'published', 'reserved');
CREATE INDEX run_queue_items_queued_expiry_idx ON run_queue_items(org_id, queued_expires_at)
    WHERE status IN ('queued', 'published') AND queued_expires_at IS NOT NULL;
CREATE INDEX run_queue_items_reservation_expiry_idx ON run_queue_items(org_id, reservation_expires_at)
    WHERE status = 'reserved' AND reservation_expires_at IS NOT NULL;
CREATE INDEX run_queue_concurrency_leases_active_idx ON run_queue_concurrency_leases(org_id, environment_id, queue_name, COALESCE(concurrency_key, ''))
    WHERE released_at IS NULL;
CREATE UNIQUE INDEX run_queue_concurrency_leases_active_scope_slot_idx ON run_queue_concurrency_leases(org_id, environment_id, queue_name, COALESCE(concurrency_key, ''), slot_ordinal)
    WHERE released_at IS NULL;
CREATE INDEX org_members_user_active_idx ON org_members(user_id, org_id) WHERE disabled_at IS NULL;
CREATE INDEX sessions_user_active_idx ON sessions(user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_active_idx ON sessions(expires_at) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX invitations_pending_invitee_idx ON invitations(org_id, invitee_email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;
CREATE INDEX invitations_email_lookup_idx ON invitations(org_id, invitee_email);
CREATE INDEX magic_links_active_token_idx ON magic_links(token_hash)
    WHERE sent_at IS NOT NULL AND consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX magic_links_email_purpose_recent_idx ON magic_links(email, purpose, created_at DESC)
    WHERE delivery_failed_at IS NULL;
CREATE INDEX magic_links_invitation_active_idx ON magic_links(invitation_id, created_at DESC)
    WHERE invitation_id IS NOT NULL AND sent_at IS NOT NULL AND consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX api_keys_org_active_idx ON api_keys(org_id, created_at DESC) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX api_keys_scope_active_name_idx ON api_keys(org_id, project_id, environment_id, name) WHERE revoked_at IS NULL;
CREATE INDEX api_key_grants_key_idx ON api_key_grants(org_id, api_key_id, permission);
CREATE UNIQUE INDEX api_key_grants_unique_idx ON api_key_grants(org_id, api_key_id, permission);
CREATE INDEX device_codes_pending_expiry_idx ON device_codes(expires_at) WHERE status = 'pending';
CREATE INDEX worker_bootstrap_tokens_active_idx ON worker_bootstrap_tokens(created_at)
    WHERE revoked_at IS NULL;
CREATE INDEX worker_instances_status_seen_idx ON worker_instances(status, last_seen_at DESC);
CREATE INDEX worker_instances_capacity_idx ON worker_instances(available_milli_cpu, available_memory_mib, available_execution_slots)
    WHERE status = 'active';
CREATE UNIQUE INDEX runtime_release_selections_singleton_idx ON runtime_release_selections((true));
CREATE INDEX worker_instance_credentials_worker_instance_active_idx ON worker_instance_credentials(worker_instance_id)
    WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX worker_instance_credentials_worker_instance_one_active_idx ON worker_instance_credentials(worker_instance_id)
    WHERE revoked_at IS NULL;
CREATE INDEX environments_current_deployment_idx
    ON environments(org_id, project_id, current_deployment_id)
    WHERE current_deployment_id IS NOT NULL;
CREATE INDEX deployment_promotions_deployment_idx
    ON deployment_promotions(org_id, project_id, environment_id, deployment_id);
CREATE INDEX deployment_promotions_environment_created_idx
    ON deployment_promotions(org_id, project_id, environment_id, created_at DESC);
CREATE UNIQUE INDEX deployments_reusable_build_key_idx
    ON deployments(org_id, project_id, environment_id, content_hash)
    WHERE status IN ('queued', 'building');
CREATE INDEX artifacts_scope_kind_created_idx
    ON artifacts(org_id, project_id, environment_id, kind, created_at DESC);
CREATE INDEX artifacts_digest_idx
    ON artifacts(digest);
CREATE INDEX deployment_tasks_lookup_idx
    ON deployment_tasks(org_id, project_id, environment_id, task_id);
CREATE UNIQUE INDEX run_log_chunks_observed_idx ON run_log_chunks(org_id, run_id, session_id, stream, observed_seq);
CREATE INDEX run_log_chunks_attempt_idx ON run_log_chunks(org_id, run_id, attempt_number, stream, seq);
CREATE INDEX events_run_session_idx ON events(org_id, run_id, session_id, seq)
    WHERE session_id IS NOT NULL;
CREATE INDEX events_run_attempt_idx ON events(org_id, run_id, attempt_number, seq)
    WHERE attempt_number IS NOT NULL;
CREATE INDEX run_attempts_run_status_idx ON run_attempts(org_id, run_id, status, attempt_number);
CREATE UNIQUE INDEX run_execution_sessions_one_active_per_run_idx ON run_execution_sessions(run_id)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_execution_sessions_attempt_idx ON run_execution_sessions(org_id, run_id, attempt_id, leased_at DESC);
CREATE INDEX run_execution_sessions_active_lease_idx ON run_execution_sessions(org_id, status, lease_expires_at)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_execution_sessions_worker_instance_status_idx ON run_execution_sessions(org_id, worker_instance_id, status);
CREATE INDEX run_snapshots_run_created_idx ON run_snapshots(org_id, run_id, created_at DESC);
CREATE INDEX checkpoints_run_status_idx ON checkpoints(run_id, status, created_at DESC);
CREATE INDEX checkpoint_artifacts_checkpoint_role_idx ON checkpoint_artifacts(org_id, run_id, checkpoint_id, role, ordinal);
CREATE UNIQUE INDEX run_waits_one_active_per_session_idx ON run_waits(run_id, session_id)
    WHERE status IN ('opening', 'waiting', 'resuming');
CREATE UNIQUE INDEX run_waits_open_correlation_idx ON run_waits(run_id, correlation_id)
    WHERE status IN ('opening', 'waiting');
CREATE INDEX run_waits_run_status_idx ON run_waits(run_id, status, waiting_at DESC);
CREATE INDEX run_waits_due_idx ON run_waits(org_id, waiting_at, timeout_seconds)
    WHERE status = 'waiting' AND timeout_seconds IS NOT NULL;
CREATE INDEX run_wait_dependencies_waitpoint_idx ON run_wait_dependencies(org_id, waitpoint_id, run_wait_id);
CREATE INDEX waitpoints_scope_status_idx ON waitpoints(org_id, project_id, environment_id, status, created_at DESC);
CREATE INDEX waitpoint_response_tokens_hash_active_idx ON waitpoint_response_tokens(token_hash)
    WHERE status = 'pending';
CREATE INDEX waitpoint_response_tokens_waitpoint_status_idx ON waitpoint_response_tokens(org_id, waitpoint_id, status, created_at DESC);
CREATE INDEX waitpoint_responses_waitpoint_idx ON waitpoint_responses(org_id, waitpoint_id, created_at);
CREATE INDEX waitpoint_deliveries_waitpoint_status_idx ON waitpoint_deliveries(org_id, run_id, run_wait_id, waitpoint_id, status, created_at DESC);
CREATE UNIQUE INDEX waitpoint_deliveries_email_recipient_idx ON waitpoint_deliveries(org_id, run_id, run_wait_id, waitpoint_id, channel, recipient_kind, recipient)
    WHERE channel = 'email' AND recipient_kind = 'email' AND status <> 'failed';
CREATE INDEX waitpoint_deliveries_due_idx ON waitpoint_deliveries(status, next_attempt_at, created_at)
    WHERE status IN ('queued', 'retrying');
CREATE INDEX waitpoint_policies_scope_name_idx ON waitpoint_policies(org_id, project_id, environment_id, name);

CREATE TRIGGER organizations_set_updated_at
    BEFORE UPDATE ON organizations
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER auth_identities_set_updated_at
    BEFORE UPDATE ON auth_identities
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER org_members_set_updated_at
    BEFORE UPDATE ON org_members
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER waitpoint_policies_set_updated_at
    BEFORE UPDATE ON waitpoint_policies
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER deletion_jobs_set_updated_at
    BEFORE UPDATE ON deletion_jobs
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER projects_set_updated_at
    BEFORE UPDATE ON projects
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER environments_set_updated_at
    BEFORE UPDATE ON environments
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER secrets_set_updated_at
    BEFORE UPDATE ON secrets
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER deployments_set_updated_at
    BEFORE UPDATE ON deployments
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER runs_set_updated_at
    BEFORE UPDATE ON runs
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER run_runtime_requirements_set_updated_at
    BEFORE UPDATE ON run_runtime_requirements
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER run_queue_items_set_updated_at
    BEFORE UPDATE ON run_queue_items
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER waitpoint_responses_set_updated_at
    BEFORE UPDATE ON waitpoint_responses
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER waitpoint_deliveries_set_updated_at
    BEFORE UPDATE ON waitpoint_deliveries
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
