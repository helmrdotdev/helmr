CREATE TABLE organizations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    slug TEXT NOT NULL UNIQUE CHECK (btrim(slug) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
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

CREATE TABLE waitpoint_policies (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    label TEXT NOT NULL DEFAULT '',
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, name)
);

CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    is_default BOOLEAN NOT NULL DEFAULT false,
    archived_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, slug)
);

CREATE TABLE environments (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    is_default BOOLEAN NOT NULL DEFAULT false,
    archived_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, id),
    UNIQUE (org_id, project_id, slug),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE
);

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    user_id UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (org_id, user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE CASCADE
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
    'bootstrap_owner',
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
    created_by_user_id UUID,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    key_prefix TEXT NOT NULL CHECK (btrim(key_prefix) <> ''),
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    FOREIGN KEY (org_id, created_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (created_by_user_id)
);

CREATE TABLE api_key_grants (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    api_key_id UUID NOT NULL,
    project_id UUID,
    environment_id UUID,
    permission TEXT NOT NULL CHECK (btrim(permission) <> ''),
    created_by_user_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (project_id IS NULL AND environment_id IS NULL)
        OR (project_id IS NOT NULL AND environment_id IS NULL)
        OR (project_id IS NOT NULL AND environment_id IS NOT NULL)
    ),
    FOREIGN KEY (org_id, api_key_id)
        REFERENCES api_keys(org_id, id)
        ON DELETE CASCADE,
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
    key_id TEXT NOT NULL CHECK (btrim(key_id) <> ''),
    nonce BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    UNIQUE (org_id, project_id, environment_id, name),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE github_app_installations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL,
    account_login TEXT NOT NULL CHECK (btrim(account_login) <> ''),
    account_type TEXT NOT NULL,
    repository_selection TEXT,
    html_url TEXT,
    suspended_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, installation_id)
);

CREATE TABLE github_repositories (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    installation_id BIGINT NOT NULL,
    github_repository_id BIGINT NOT NULL,
    owner_login TEXT NOT NULL CHECK (btrim(owner_login) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    full_name TEXT NOT NULL,
    private BOOLEAN NOT NULL DEFAULT false,
    archived BOOLEAN NOT NULL DEFAULT false,
    default_branch TEXT,
    html_url TEXT,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, github_repository_id),
    UNIQUE (org_id, installation_id, github_repository_id),
    FOREIGN KEY (org_id, installation_id)
        REFERENCES github_app_installations(org_id, installation_id)
        ON DELETE CASCADE
);

CREATE TABLE github_repository_connections (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    github_repository_id BIGINT NOT NULL,
    enabled_by_user_id UUID,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, github_repository_id),
    FOREIGN KEY (org_id, github_repository_id)
        REFERENCES github_repositories(org_id, github_repository_id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, enabled_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (enabled_by_user_id)
);

CREATE TABLE project_workspace_repositories (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    github_repository_id BIGINT NOT NULL,
    enabled_by_user_id UUID,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, github_repository_id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, github_repository_id)
        REFERENCES github_repositories(org_id, github_repository_id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, enabled_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (enabled_by_user_id)
);

CREATE TABLE cas_objects (
    digest TEXT PRIMARY KEY,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE run_status AS ENUM (
    'queued',
    'claimed',
    'running',
    'waiting',
    'succeeded',
    'failed',
    'cancelled'
);

CREATE TYPE run_execution_status AS ENUM (
    'claimed',
    'running',
    'detached',
    'released',
    'lost'
);

CREATE TYPE worker_status AS ENUM (
    'active',
    'draining'
);

CREATE TABLE worker_pools (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, slug),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE worker_pool_registration_tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    worker_pool_id UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    last_used_by_worker_id TEXT,
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_pool_id)
        REFERENCES worker_pools(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, worker_pool_id)
        REFERENCES worker_pools(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE worker_credentials (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    worker_pool_id UUID NOT NULL,
    worker_id TEXT NOT NULL CHECK (btrim(worker_id) <> ''),
    key_prefix TEXT NOT NULL CHECK (btrim(key_prefix) <> ''),
    secret_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    UNIQUE (org_id, worker_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_pool_id)
        REFERENCES worker_pools(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, worker_pool_id)
        REFERENCES worker_pools(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TYPE waitpoint_kind AS ENUM (
    'approval',
    'message'
);

CREATE TYPE waitpoint_status AS ENUM (
    'creating',
    'pending',
    'resolved',
    'cancelled'
);

CREATE TYPE waitpoint_response_token_status AS ENUM (
    'pending',
    'completed',
    'revoked'
);

CREATE TYPE waitpoint_delivery_status AS ENUM (
    'queued',
    'sent',
    'failed'
);

CREATE TYPE checkpoint_status AS ENUM (
    'creating',
    'ready',
    'restoring',
    'invalid'
);

CREATE TYPE task_deployment_status AS ENUM (
    'creating',
    'active',
    'archived'
);

CREATE TABLE task_deployments (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    source_digest TEXT NOT NULL REFERENCES cas_objects(digest),
    status task_deployment_status NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deployed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE deployed_tasks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    module_path TEXT NOT NULL DEFAULT '',
    export_name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, deployment_id, id),
    UNIQUE (org_id, deployment_id, id, task_id),
    UNIQUE (org_id, deployment_id, task_id),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES task_deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE runs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_deployment_id UUID NOT NULL,
    deployed_task_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    status run_status NOT NULL DEFAULT 'queued',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    output JSONB,
    secret_bindings JSONB NOT NULL DEFAULT '{}'::jsonb,
    workspace_repository TEXT NOT NULL,
    workspace_installation_id BIGINT NOT NULL,
    workspace_github_repository_id BIGINT NOT NULL,
    workspace_ref TEXT NOT NULL CHECK (btrim(workspace_ref) <> ''),
    workspace_sha TEXT NOT NULL,
    workspace_subpath TEXT NOT NULL DEFAULT '',
    max_duration_seconds INTEGER NOT NULL CHECK (max_duration_seconds BETWEEN 5 AND 86400),
    current_execution_id UUID,
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
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, task_deployment_id)
        REFERENCES task_deployments(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, task_deployment_id, deployed_task_id, task_id)
        REFERENCES deployed_tasks(org_id, deployment_id, id, task_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, workspace_installation_id)
        REFERENCES github_app_installations(org_id, installation_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, workspace_github_repository_id)
        REFERENCES github_repositories(org_id, github_repository_id)
        ON DELETE RESTRICT
);

CREATE TABLE workers (
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    worker_pool_id UUID NOT NULL,
    id TEXT NOT NULL CHECK (btrim(id) <> ''),
    status worker_status NOT NULL DEFAULT 'active',
    runtime_arch TEXT NOT NULL CHECK (btrim(runtime_arch) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    max_vcpus INTEGER NOT NULL CHECK (max_vcpus > 0),
    max_memory_mib INTEGER NOT NULL CHECK (max_memory_mib > 0),
    slots_available INTEGER NOT NULL DEFAULT 1 CHECK (slots_available >= 0),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, worker_pool_id)
        REFERENCES worker_pools(org_id, id)
        ON DELETE RESTRICT
);

CREATE TABLE run_events (
    id BIGSERIAL PRIMARY KEY,
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (btrim(kind) <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE
);

CREATE TYPE run_log_stream AS ENUM (
    'stdout',
    'stderr'
);

CREATE TABLE run_log_chunks (
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    execution_id UUID NOT NULL,
    stream run_log_stream NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    observed_seq BIGINT NOT NULL CHECK (observed_seq >= 0),
    content BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, stream, seq)
);

CREATE TABLE run_executions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    worker_pool_id UUID NOT NULL,
    worker_id TEXT NOT NULL CHECK (btrim(worker_id) <> ''),
    status run_execution_status NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    active_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_duration_ms >= 0),
    restore_checkpoint_id UUID,
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (run_id, id),
    FOREIGN KEY (org_id, worker_id)
        REFERENCES workers(org_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, worker_pool_id)
        REFERENCES worker_pools(org_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE
);

ALTER TABLE run_log_chunks
    ADD CONSTRAINT run_log_chunks_execution_id_fkey
    FOREIGN KEY (run_id, execution_id)
    REFERENCES run_executions(run_id, id)
    ON DELETE CASCADE;

ALTER TABLE runs
    ADD CONSTRAINT runs_current_execution_id_fkey
    FOREIGN KEY (org_id, id, current_execution_id)
    REFERENCES run_executions(org_id, run_id, id);

CREATE TABLE checkpoints (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    execution_id UUID NOT NULL,
    status checkpoint_status NOT NULL DEFAULT 'creating',
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    runtime_backend TEXT,
    runtime_arch TEXT,
    runtime_abi TEXT,
    kernel_digest TEXT,
    rootfs_digest TEXT,
    runtime_vcpus INTEGER CHECK (runtime_vcpus IS NULL OR runtime_vcpus > 0),
    runtime_memory_mib INTEGER CHECK (runtime_memory_mib IS NULL OR runtime_memory_mib > 0),
    cni_profile TEXT,
    image_key TEXT,
    runtime_config_digest TEXT,
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, run_id, execution_id, id),
    FOREIGN KEY (org_id, run_id, execution_id)
        REFERENCES run_executions(org_id, run_id, id)
        ON DELETE CASCADE
);

CREATE TYPE checkpoint_artifact_role AS ENUM (
    'manifest',
    'vm_state',
    'workspace_upper',
    'memory'
);

CREATE TABLE checkpoint_artifacts (
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    checkpoint_id UUID NOT NULL,
    role checkpoint_artifact_role NOT NULL,
    ordinal INTEGER NOT NULL DEFAULT 0 CHECK (ordinal >= 0),
    digest TEXT NOT NULL REFERENCES cas_objects(digest),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, checkpoint_id, role, ordinal),
    FOREIGN KEY (org_id, run_id, checkpoint_id)
        REFERENCES checkpoints(org_id, run_id, id)
        ON DELETE CASCADE
);

ALTER TABLE runs
    ADD CONSTRAINT runs_latest_checkpoint_id_fkey
    FOREIGN KEY (org_id, id, latest_checkpoint_id)
    REFERENCES checkpoints(org_id, run_id, id);

ALTER TABLE run_executions
    ADD CONSTRAINT run_executions_restore_checkpoint_id_fkey
    FOREIGN KEY (org_id, run_id, restore_checkpoint_id)
    REFERENCES checkpoints(org_id, run_id, id);

CREATE TABLE waitpoints (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    execution_id UUID NOT NULL,
    checkpoint_id UUID NOT NULL,
    correlation_id TEXT NOT NULL,
    kind waitpoint_kind NOT NULL,
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    display_text TEXT NOT NULL DEFAULT '',
    timeout_seconds INTEGER CHECK (timeout_seconds IS NULL OR timeout_seconds > 0),
    policy_name TEXT,
    policy_snapshot JSONB,
    status waitpoint_status NOT NULL DEFAULT 'creating',
    resolution_kind TEXT,
    resolution JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    requested_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    CHECK (requested_at IS NULL OR resolved_at IS NULL OR requested_at <= resolved_at),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, execution_id)
        REFERENCES run_executions(org_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, execution_id, checkpoint_id)
        REFERENCES checkpoints(org_id, run_id, execution_id, id)
        ON DELETE CASCADE,
    UNIQUE (org_id, run_id, id)
);

CREATE TABLE waitpoint_response_tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    waitpoint_id UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    allowed_actions TEXT[] NOT NULL,
    status waitpoint_response_token_status NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    completed_by_principal TEXT,
    completed_via TEXT,
    external_subject TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_id, waitpoint_id, id),
    FOREIGN KEY (org_id, run_id, waitpoint_id)
        REFERENCES waitpoints(org_id, run_id, id)
        ON DELETE CASCADE
);

CREATE TABLE waitpoint_deliveries (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    waitpoint_id UUID NOT NULL,
    response_token_id UUID,
    channel TEXT NOT NULL,
    recipient_kind TEXT NOT NULL,
    recipient TEXT NOT NULL,
    status waitpoint_delivery_status NOT NULL DEFAULT 'queued',
    last_error TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, run_id, waitpoint_id)
        REFERENCES waitpoints(org_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, waitpoint_id, response_token_id)
        REFERENCES waitpoint_response_tokens(org_id, run_id, waitpoint_id, id)
        ON DELETE SET NULL (response_token_id)
);

CREATE UNIQUE INDEX projects_one_default_idx ON projects(org_id)
    WHERE is_default AND archived_at IS NULL;
CREATE UNIQUE INDEX environments_one_default_idx ON environments(org_id, project_id)
    WHERE is_default AND archived_at IS NULL;
CREATE UNIQUE INDEX worker_pools_one_default_idx ON worker_pools(org_id, project_id, environment_id)
    WHERE is_default AND archived_at IS NULL;
CREATE INDEX runs_org_created_idx ON runs(org_id, created_at DESC);
CREATE INDEX runs_org_status_created_idx ON runs(org_id, status, created_at DESC);
CREATE INDEX runs_scope_created_idx ON runs(org_id, project_id, environment_id, created_at DESC);
CREATE INDEX runs_scope_status_created_idx ON runs(org_id, project_id, environment_id, status, created_at DESC);
CREATE INDEX org_members_user_active_idx ON org_members(user_id, org_id) WHERE disabled_at IS NULL;
CREATE INDEX sessions_user_active_idx ON sessions(org_id, user_id) WHERE revoked_at IS NULL;
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
CREATE UNIQUE INDEX api_keys_org_active_name_idx ON api_keys(org_id, name) WHERE revoked_at IS NULL;
CREATE INDEX api_key_grants_key_idx ON api_key_grants(org_id, api_key_id, permission);
CREATE UNIQUE INDEX api_key_grants_org_unique_idx ON api_key_grants(org_id, api_key_id, permission)
    WHERE project_id IS NULL AND environment_id IS NULL;
CREATE UNIQUE INDEX api_key_grants_project_unique_idx ON api_key_grants(org_id, api_key_id, project_id, permission)
    WHERE project_id IS NOT NULL AND environment_id IS NULL;
CREATE UNIQUE INDEX api_key_grants_environment_unique_idx ON api_key_grants(org_id, api_key_id, project_id, environment_id, permission)
    WHERE environment_id IS NOT NULL;
CREATE INDEX device_codes_pending_expiry_idx ON device_codes(expires_at) WHERE status = 'pending';
CREATE INDEX worker_pool_registration_tokens_scope_active_idx ON worker_pool_registration_tokens(org_id, project_id, environment_id, worker_pool_id, created_at)
    WHERE revoked_at IS NULL;
CREATE INDEX worker_credentials_org_worker_active_idx ON worker_credentials(org_id, worker_id)
    WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX worker_credentials_org_worker_one_active_idx ON worker_credentials(org_id, worker_id)
    WHERE revoked_at IS NULL;
CREATE INDEX worker_credentials_scope_worker_active_idx ON worker_credentials(org_id, project_id, environment_id, worker_pool_id, worker_id)
    WHERE revoked_at IS NULL;
CREATE INDEX github_app_installations_org_account_idx ON github_app_installations(org_id, lower(account_login));
CREATE UNIQUE INDEX github_app_installations_org_active_account_idx ON github_app_installations(org_id, lower(account_login))
    WHERE suspended_at IS NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX github_app_installations_active_installation_idx ON github_app_installations(installation_id)
    WHERE deleted_at IS NULL;
CREATE INDEX github_repositories_org_full_name_idx ON github_repositories(org_id, lower(full_name));
CREATE UNIQUE INDEX github_repositories_org_active_full_name_idx ON github_repositories(org_id, lower(full_name))
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX github_repositories_installation_full_name_idx ON github_repositories(org_id, installation_id, lower(full_name))
    WHERE deleted_at IS NULL;
CREATE INDEX github_repositories_installation_active_idx ON github_repositories(org_id, installation_id, lower(full_name))
    WHERE deleted_at IS NULL;
CREATE INDEX github_repository_connections_active_idx ON github_repository_connections(org_id, github_repository_id)
    WHERE disabled_at IS NULL;
CREATE INDEX project_workspace_repositories_project_active_idx ON project_workspace_repositories(org_id, project_id, github_repository_id)
    WHERE disabled_at IS NULL;
CREATE INDEX project_workspace_repositories_repository_active_idx ON project_workspace_repositories(org_id, github_repository_id)
    WHERE disabled_at IS NULL;
CREATE UNIQUE INDEX task_deployments_one_active_idx
    ON task_deployments(org_id, project_id, environment_id)
    WHERE status = 'active';
CREATE INDEX deployed_tasks_active_lookup_idx
    ON deployed_tasks(org_id, project_id, environment_id, task_id);
CREATE INDEX run_events_run_id_id_idx ON run_events(run_id, id);
CREATE UNIQUE INDEX run_log_chunks_observed_idx ON run_log_chunks(run_id, execution_id, stream, observed_seq);
CREATE UNIQUE INDEX run_executions_one_active_per_run_idx ON run_executions(run_id)
    WHERE status IN ('claimed', 'running');
CREATE INDEX run_executions_active_lease_idx ON run_executions(org_id, status, lease_expires_at)
    WHERE status IN ('claimed', 'running');
CREATE INDEX run_executions_worker_status_idx ON run_executions(org_id, worker_pool_id, worker_id, status);
CREATE INDEX workers_org_status_seen_idx ON workers(org_id, status, last_seen_at DESC);
CREATE INDEX workers_pool_status_seen_idx ON workers(org_id, worker_pool_id, status, last_seen_at DESC);
CREATE INDEX checkpoints_run_status_idx ON checkpoints(run_id, status, created_at DESC);
CREATE INDEX checkpoint_artifacts_checkpoint_role_idx ON checkpoint_artifacts(org_id, run_id, checkpoint_id, role, ordinal);
CREATE UNIQUE INDEX waitpoints_one_open_per_run_idx ON waitpoints(run_id)
    WHERE status IN ('creating', 'pending');
CREATE UNIQUE INDEX waitpoints_open_correlation_idx ON waitpoints(run_id, correlation_id)
    WHERE status IN ('creating', 'pending');
CREATE INDEX waitpoints_run_status_idx ON waitpoints(run_id, status, requested_at DESC);
CREATE INDEX waitpoints_due_idx ON waitpoints(org_id, requested_at, timeout_seconds)
    WHERE status = 'pending' AND timeout_seconds IS NOT NULL;
CREATE INDEX waitpoint_response_tokens_hash_active_idx ON waitpoint_response_tokens(token_hash)
    WHERE status = 'pending';
CREATE INDEX waitpoint_response_tokens_waitpoint_status_idx ON waitpoint_response_tokens(org_id, run_id, waitpoint_id, status, created_at DESC);
CREATE INDEX waitpoint_deliveries_waitpoint_status_idx ON waitpoint_deliveries(org_id, run_id, waitpoint_id, status, created_at DESC);
CREATE INDEX waitpoint_policies_org_name_idx ON waitpoint_policies(org_id, name);

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

CREATE TRIGGER github_app_installations_set_updated_at
    BEFORE UPDATE ON github_app_installations
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER github_repositories_set_updated_at
    BEFORE UPDATE ON github_repositories
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER github_repository_connections_set_updated_at
    BEFORE UPDATE ON github_repository_connections
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER project_workspace_repositories_set_updated_at
    BEFORE UPDATE ON project_workspace_repositories
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER worker_pools_set_updated_at
    BEFORE UPDATE ON worker_pools
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER runs_set_updated_at
    BEFORE UPDATE ON runs
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER waitpoint_deliveries_set_updated_at
    BEFORE UPDATE ON waitpoint_deliveries
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
