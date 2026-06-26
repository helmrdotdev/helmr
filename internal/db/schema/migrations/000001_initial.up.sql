CREATE EXTENSION IF NOT EXISTS btree_gist;

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

CREATE TYPE deletion_job_target_type AS ENUM (
    'project',
    'environment'
);

CREATE TABLE deletion_jobs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    target_type deletion_job_target_type NOT NULL,
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

CREATE TABLE auth_sessions (
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
    UNIQUE (key_id, nonce),
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
    'sandbox_image',
    'task_bundle',
    'runtime_checkpoint_config',
    'runtime_checkpoint_vm_state',
    'runtime_checkpoint_memory',
    'runtime_checkpoint_scratch_disk',
    'workspace_version'
);

CREATE TYPE worker_instance_status AS ENUM (
    'active',
    'draining',
    'unschedulable',
    'offline'
);

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
    worker_group_id UUID NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
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
    worker_version TEXT NOT NULL DEFAULT '',
    protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v1' CHECK (btrim(protocol_version) <> ''),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    drained_at TIMESTAMPTZ,
    UNIQUE (worker_group_id, resource_id),
    UNIQUE (id, worker_group_id)
);

CREATE TABLE worker_bootstrap_tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    token_hash BYTEA NOT NULL UNIQUE,
    worker_group_id UUID NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
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
    UNIQUE (org_id, project_id, environment_id, id, digest),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TYPE stream_direction AS ENUM (
    'input',
    'output'
);

CREATE TYPE stream_record_source_type AS ENUM (
    'api_key',
    'public_access_token',
    'worker_lease',
    'session',
    'system'
);

CREATE TYPE token_state AS ENUM (
    'pending',
    'completed',
    'expired',
    'cancelled'
);

CREATE TYPE public_access_token_state AS ENUM (
    'active',
    'revoked',
    'expired'
);

CREATE TYPE public_access_token_scope_type AS ENUM (
    'token.complete',
    'session.input.send',
    'session.output.read'
);

CREATE TYPE run_wait_kind AS ENUM (
    'stream',
    'token',
    'timer'
);

CREATE TYPE run_wait_state AS ENUM (
    'parking',
    'waiting',
    'resolved',
    'expired',
    'resuming',
    'resumed',
    'cancelled',
    'failed'
);

CREATE TYPE runtime_checkpoint_state AS ENUM (
    'creating',
    'ready',
    'restoring',
    'invalid',
    'deleted'
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
    'waiting',
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

CREATE TYPE run_lease_status AS ENUM (
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
    'parked',
    'completed',
    'cancelled',
    'dead_lettered'
);

CREATE TYPE run_operation_kind AS ENUM (
    'cancel'
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

CREATE TYPE session_status AS ENUM (
    'open',
    'closed',
    'cancelled'
);

CREATE TYPE workspace_state AS ENUM (
    'active',
    'deleting',
    'recovery_required',
    'archived',
    'deleted'
);

CREATE TYPE workspace_desired_state AS ENUM (
    'active',
    'stopped',
    'archived',
    'deleted'
);

CREATE TYPE workspace_dirty_state AS ENUM (
    'clean',
    'dirty',
    'capturing',
    'capture_failed',
    'dirty_state_lost'
);

CREATE TYPE workspace_version_state AS ENUM (
    'capturing',
    'artifact_verified',
    'ready',
    'failed',
    'deleted'
);

CREATE TYPE workspace_version_kind AS ENUM (
    'user',
    'system'
);

CREATE TYPE workspace_materialization_state AS ENUM (
    'requested',
    'materializing',
    'restoring',
    'running',
    'pausing',
    'paused',
    'capturing',
    'stopping',
    'stopped',
    'lost',
    'failed'
);

CREATE TYPE workspace_materialization_operation_state AS ENUM (
    'queued',
    'claimed',
    'running',
    'completed',
    'failed',
    'cancelled',
    'lost',
    'expired'
);

CREATE TYPE workspace_materialization_operation_kind AS ENUM (
    'start_exec',
    'create_pty',
    'resize_pty',
    'close_pty'
);

CREATE TYPE workspace_resource_kind AS ENUM (
    'workspace_exec',
    'workspace_pty'
);

CREATE TYPE workspace_stream_notification_kind AS ENUM (
    'chunk',
    'terminal'
);

CREATE TYPE workspace_operation_idempotency_kind AS ENUM (
    'workspace_create',
    'workspace_stop',
    'workspace_exec_create',
    'workspace_pty_create'
);

CREATE TYPE workspace_lease_kind AS ENUM (
    'instance',
    'write'
);

CREATE TYPE workspace_lease_state AS ENUM (
    'active',
    'releasing',
    'released',
    'expired',
    'lost'
);

CREATE TYPE workspace_filesystem_mode AS ENUM (
    'write'
);

CREATE TYPE workspace_exec_state AS ENUM (
    'queued',
    'materializing',
    'running',
    'exited',
    'terminated',
    'lost',
    'failed'
);

CREATE TYPE workspace_exec_stream AS ENUM (
    'stdin',
    'stdout',
    'stderr'
);

CREATE TYPE workspace_pty_state AS ENUM (
    'creating',
    'open',
    'resizing',
    'closing',
    'closed',
    'lost',
    'failed'
);

CREATE TYPE workspace_pty_stream AS ENUM (
    'input',
    'output'
);

CREATE TYPE workspace_port_protocol AS ENUM (
    'http',
    'tcp'
);

CREATE TYPE workspace_port_state AS ENUM (
    'exposing',
    'open',
    'closing',
    'closed',
    'expired',
    'failed'
);

CREATE TYPE workspace_port_auth_mode AS ENUM (
    'private',
    'public_token'
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
    worker_group_id UUID NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    version TEXT NOT NULL CHECK (btrim(version) <> ''),
    content_hash TEXT NOT NULL CHECK (btrim(content_hash) <> ''),
    api_version TEXT NOT NULL DEFAULT '2026-06-06' CHECK (btrim(api_version) <> ''),
    sdk_version TEXT NOT NULL DEFAULT '',
    cli_version TEXT NOT NULL DEFAULT '',
    bundle_format_version INTEGER NOT NULL DEFAULT 2 CHECK (bundle_format_version > 0),
    worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v1' CHECK (btrim(worker_protocol_version) <> ''),
    deployment_source_artifact_id UUID NOT NULL,
    build_manifest_artifact_id UUID,
    deployment_manifest_artifact_id UUID,
    status deployment_status NOT NULL DEFAULT 'queued',
    failure JSONB NOT NULL DEFAULT '{}'::jsonb,
    build_lease_id TEXT,
    build_worker_instance_id UUID,
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
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (build_worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
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

CREATE TABLE tasks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    UNIQUE (environment_id, task_id),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, task_id),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE deployment_sandboxes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    sandbox_id TEXT NOT NULL CHECK (btrim(sandbox_id) <> ''),
    image_artifact_id UUID NOT NULL,
    image_artifact_format TEXT NOT NULL CHECK (btrim(image_artifact_format) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    image_digest TEXT NOT NULL CHECK (btrim(image_digest) <> ''),
    image_format TEXT NOT NULL CHECK (btrim(image_format) <> ''),
    workspace_mount_path TEXT NOT NULL CHECK (btrim(workspace_mount_path) <> ''),
    resource_floor JSONB NOT NULL DEFAULT '{}'::jsonb,
    disk_floor_mib BIGINT NOT NULL DEFAULT 0 CHECK (disk_floor_mib >= 0),
    network_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    guestd_abi TEXT NOT NULL CHECK (btrim(guestd_abi) <> ''),
    adapter_abi TEXT NOT NULL CHECK (btrim(adapter_abi) <> ''),
    filesystem_format TEXT NOT NULL CHECK (btrim(filesystem_format) <> ''),
    default_uid BIGINT,
    default_gid BIGINT,
    default_workdir TEXT NOT NULL DEFAULT '',
    contract_version INTEGER NOT NULL CHECK (contract_version > 0),
    fingerprint TEXT NOT NULL CHECK (btrim(fingerprint) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, deployment_id, sandbox_id),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, image_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, image_artifact_id, image_digest)
        REFERENCES artifacts(org_id, project_id, environment_id, id, digest)
        ON DELETE RESTRICT
);

CREATE TABLE deployment_queues (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    concurrency_limit INTEGER CHECK (concurrency_limit IS NULL OR concurrency_limit > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, deployment_id, name),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE deployment_tasks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    deployment_sandbox_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    file_path TEXT NOT NULL DEFAULT '',
    export_name TEXT NOT NULL DEFAULT '',
    handler_entrypoint TEXT NOT NULL DEFAULT '',
    bundle_artifact_id UUID NOT NULL,
    bundle_format_version INTEGER NOT NULL DEFAULT 2 CHECK (bundle_format_version > 0),
    requested_milli_cpu BIGINT NOT NULL DEFAULT 2000 CHECK (requested_milli_cpu > 0),
    requested_memory_mib BIGINT NOT NULL DEFAULT 2048 CHECK (requested_memory_mib > 0),
    requested_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (requested_disk_mib >= 0),
    secret_declarations JSONB NOT NULL DEFAULT '[]'::jsonb,
    resource_requirements JSONB NOT NULL DEFAULT '{}'::jsonb,
    network_policy JSONB NOT NULL DEFAULT '{"internet": true}'::jsonb,
    schedule_declarations JSONB NOT NULL DEFAULT '[]'::jsonb,
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    queue_concurrency_limit INTEGER,
    ttl TEXT NOT NULL DEFAULT '',
    max_active_duration_ms BIGINT NOT NULL CHECK (max_active_duration_ms > 0),
    retry_policy JSONB NOT NULL DEFAULT '{"enabled": false}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, deployment_id, id),
    UNIQUE (org_id, deployment_id, id, task_id),
    UNIQUE (org_id, deployment_id, task_id),
    FOREIGN KEY (org_id, project_id, environment_id, task_id)
        REFERENCES tasks(org_id, project_id, environment_id, task_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id, queue_name)
        REFERENCES deployment_queues(org_id, project_id, environment_id, deployment_id, name)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, bundle_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

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

CREATE TABLE task_schedule_instances (
    id UUID PRIMARY KEY,
    schedule_id UUID NOT NULL,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
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
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, task_id)
        REFERENCES tasks(org_id, project_id, environment_id, task_id)
        ON DELETE CASCADE
);

CREATE TABLE workspaces (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_sandbox_id UUID NOT NULL,
    sandbox_id TEXT NOT NULL CHECK (btrim(sandbox_id) <> ''),
    sandbox_fingerprint TEXT NOT NULL CHECK (btrim(sandbox_fingerprint) <> ''),
    external_id TEXT NOT NULL DEFAULT '' CHECK (external_id = btrim(external_id) AND octet_length(external_id) <= 512),
    current_version_id UUID,
    current_version_required_state workspace_version_state GENERATED ALWAYS AS ('ready'::workspace_version_state) STORED,
    state workspace_state NOT NULL DEFAULT 'active',
    desired_state workspace_desired_state NOT NULL DEFAULT 'active',
    dirty_state workspace_dirty_state NOT NULL DEFAULT 'clean',
    last_materialization_id UUID,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    retention_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    auto_stop_at TIMESTAMPTZ,
    auto_archive_at TIMESTAMPTZ,
    auto_delete_at TIMESTAMPTZ,
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    initial_deployment_id UUID NOT NULL,
    active_deployment_id UUID NOT NULL,
    external_id TEXT NOT NULL DEFAULT '' CHECK (external_id = btrim(external_id) AND octet_length(external_id) <= 512),
    start_fingerprint TEXT NOT NULL DEFAULT '',
    status session_status NOT NULL DEFAULT 'open',
    current_run_id UUID,
    current_run_version BIGINT NOT NULL DEFAULT 1 CHECK (current_run_version > 0),
    workspace_id UUID NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    closed_at TIMESTAMPTZ,
    closed_reason TEXT NOT NULL DEFAULT '',
    cancelled_at TIMESTAMPTZ,
    terminal_reason JSONB NOT NULL DEFAULT '{}'::jsonb,
    result JSONB,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id, task_id),
    FOREIGN KEY (org_id, project_id, environment_id, task_id)
        REFERENCES tasks(org_id, project_id, environment_id, task_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, initial_deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, active_deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE TABLE runs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    deployment_task_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    workspace_materialization_id UUID,
    deployment_version TEXT NOT NULL DEFAULT 'unknown' CHECK (btrim(deployment_version) <> ''),
    api_version TEXT NOT NULL DEFAULT '2026-06-06' CHECK (btrim(api_version) <> ''),
    sdk_version TEXT NOT NULL DEFAULT '',
    cli_version TEXT NOT NULL DEFAULT '',
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    session_id UUID NOT NULL,
    schedule_id UUID,
    schedule_instance_id UUID,
    scheduled_at TIMESTAMPTZ,
    status run_status NOT NULL DEFAULT 'queued',
    execution_status run_execution_status NOT NULL DEFAULT 'queued',
    terminal_outcome run_terminal_outcome,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    output JSONB,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    locked_retry_policy JSONB NOT NULL DEFAULT '{"enabled": false}'::jsonb,
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    queue_concurrency_limit INTEGER,
    concurrency_key TEXT,
    priority INTEGER NOT NULL DEFAULT 0,
    queue_timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl TEXT NOT NULL DEFAULT '',
    queued_expires_at TIMESTAMPTZ,
    max_active_duration_ms BIGINT NOT NULL CHECK (max_active_duration_ms > 0),
    active_elapsed_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_elapsed_ms >= 0),
    active_started_at TIMESTAMPTZ,
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    root_span_id TEXT NOT NULL CHECK (root_span_id ~ '^[0-9a-f]{16}$' AND root_span_id <> '0000000000000000'),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    current_attempt_id UUID,
    current_attempt_number INTEGER CHECK (current_attempt_number IS NULL OR current_attempt_number > 0),
    current_run_lease_id UUID,
    latest_runtime_checkpoint_id UUID,
    exit_code INTEGER,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
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
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, session_id, task_id)
        REFERENCES sessions(org_id, project_id, environment_id, id, task_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (schedule_id)
        REFERENCES task_schedules(id)
        ON DELETE SET NULL (schedule_id),
    FOREIGN KEY (org_id, project_id, environment_id, schedule_instance_id)
        REFERENCES task_schedule_instances(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (schedule_instance_id)
);

ALTER TABLE sessions
    ADD CONSTRAINT sessions_current_run_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, current_run_id)
    REFERENCES runs(org_id, project_id, environment_id, id)
    ON DELETE SET NULL (current_run_id)
    DEFERRABLE INITIALLY DEFERRED;

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

CREATE TABLE session_runs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    run_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    previous_run_id UUID,
    turn_index INTEGER NOT NULL CHECK (turn_index >= 0),
    reason TEXT NOT NULL CHECK (reason IN ('initial', 'input')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at TIMESTAMPTZ,
    UNIQUE (org_id, session_id, run_id),
    UNIQUE (org_id, session_id, turn_index),
    FOREIGN KEY (org_id, project_id, environment_id, session_id)
        REFERENCES sessions(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, previous_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (previous_run_id)
);

CREATE TABLE session_start_idempotencies (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    idempotency_key TEXT NOT NULL CHECK (btrim(idempotency_key) <> ''),
    request_fingerprint TEXT NOT NULL CHECK (btrim(request_fingerprint) <> ''),
    session_id UUID NOT NULL,
    first_run_id UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, environment_id, task_id, idempotency_key),
    FOREIGN KEY (org_id, project_id, environment_id, task_id)
        REFERENCES tasks(org_id, project_id, environment_id, task_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, session_id)
        REFERENCES sessions(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, first_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE TABLE workspace_materializations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    deployment_sandbox_id UUID NOT NULL,
    sandbox_fingerprint TEXT NOT NULL CHECK (btrim(sandbox_fingerprint) <> ''),
    base_version_id UUID,
    worker_instance_id UUID,
    reservation_token TEXT NOT NULL DEFAULT '',
    reservation_expires_at TIMESTAMPTZ,
    claim_attempt INTEGER NOT NULL DEFAULT 0 CHECK (claim_attempt >= 0),
    dead_lettered_at TIMESTAMPTZ,
    priority INTEGER NOT NULL DEFAULT 0,
    requested_cpu_millis INTEGER NOT NULL CHECK (requested_cpu_millis > 0),
    requested_memory_mib INTEGER NOT NULL CHECK (requested_memory_mib > 0),
    requested_disk_mib BIGINT NOT NULL CHECK (requested_disk_mib >= 0),
    requested_execution_slots INTEGER NOT NULL DEFAULT 1 CHECK (requested_execution_slots > 0),
    reserved_cpu_millis INTEGER NOT NULL DEFAULT 0 CHECK (reserved_cpu_millis >= 0),
    reserved_memory_mib INTEGER NOT NULL DEFAULT 0 CHECK (reserved_memory_mib >= 0),
    reserved_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (reserved_disk_mib >= 0),
    reserved_execution_slots INTEGER NOT NULL DEFAULT 0 CHECK (reserved_execution_slots >= 0),
    capacity_reservation_id UUID,
    guestd_channel_token_hash TEXT NOT NULL DEFAULT '',
    guestd_channel_token_expires_at TIMESTAMPTZ,
    runtime_id TEXT NOT NULL DEFAULT '',
    state workspace_materialization_state NOT NULL DEFAULT 'requested',
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    lease_generation BIGINT NOT NULL DEFAULT 1 CHECK (lease_generation > 0),
    dirty_generation BIGINT NOT NULL DEFAULT 0 CHECK (dirty_generation >= 0),
    fencing_generation BIGINT NOT NULL DEFAULT 1 CHECK (fencing_generation > 0),
    network_namespace TEXT NOT NULL DEFAULT '',
    port_namespace TEXT NOT NULL DEFAULT '',
    image_artifact_id UUID NOT NULL,
    image_artifact_format TEXT NOT NULL CHECK (btrim(image_artifact_format) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    image_digest TEXT NOT NULL CHECK (btrim(image_digest) <> ''),
    image_format TEXT NOT NULL CHECK (btrim(image_format) <> ''),
    workspace_artifact_id UUID NOT NULL,
    workspace_artifact_encoding TEXT NOT NULL CHECK (btrim(workspace_artifact_encoding) <> ''),
    workspace_artifact_entry_count INTEGER NOT NULL CHECK (workspace_artifact_entry_count >= 0),
    workspace_artifact_digest TEXT NOT NULL CHECK (btrim(workspace_artifact_digest) <> ''),
    workspace_artifact_size_bytes BIGINT NOT NULL CHECK (workspace_artifact_size_bytes >= 0),
    workspace_artifact_media_type TEXT NOT NULL CHECK (btrim(workspace_artifact_media_type) <> ''),
    workspace_mount_path TEXT NOT NULL CHECK (btrim(workspace_mount_path) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    guestd_abi TEXT NOT NULL CHECK (btrim(guestd_abi) <> ''),
    adapter_abi TEXT NOT NULL CHECK (btrim(adapter_abi) <> ''),
    last_heartbeat_at TIMESTAMPTZ,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    materialized_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, image_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, image_artifact_id, image_digest)
        REFERENCES artifacts(org_id, project_id, environment_id, id, digest)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_artifact_id, workspace_artifact_digest)
        REFERENCES artifacts(org_id, project_id, environment_id, id, digest)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE SET NULL
);

ALTER TABLE runs
    ADD CONSTRAINT runs_workspace_materialization_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, workspace_materialization_id)
    REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (workspace_materialization_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_last_materialization_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, id, last_materialization_id)
    REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (last_materialization_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE workspace_leases (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    materialization_id UUID NOT NULL,
    lease_kind workspace_lease_kind NOT NULL,
    state workspace_lease_state NOT NULL DEFAULT 'active',
    owner_run_id UUID,
    owner_exec_id UUID,
    owner_pty_session_id UUID,
    owner_port_id UUID,
    base_version_id UUID,
    acquired_version_id UUID,
    acquired_fencing_generation BIGINT NOT NULL CHECK (acquired_fencing_generation > 0),
    fencing_token TEXT NOT NULL CHECK (btrim(fencing_token) <> ''),
    heartbeat_token TEXT NOT NULL DEFAULT '',
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    CHECK (num_nonnulls(owner_run_id, owner_exec_id, owner_pty_session_id, owner_port_id) = 1),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, owner_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_execs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    materialization_id UUID,
    instance_lease_id UUID,
    write_lease_id UUID,
    command JSONB NOT NULL,
    cwd TEXT NOT NULL DEFAULT '',
    env_shape JSONB NOT NULL DEFAULT '{}'::jsonb,
    filesystem_mode workspace_filesystem_mode NOT NULL DEFAULT 'write',
    state workspace_exec_state NOT NULL DEFAULT 'queued',
    detached BOOLEAN NOT NULL DEFAULT false,
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_fingerprint TEXT NOT NULL DEFAULT '',
    process_id TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    signal TEXT NOT NULL DEFAULT '',
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    stdout_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stdout_cursor >= 0),
    stderr_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stderr_cursor >= 0),
    stdin_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stdin_cursor >= 0),
    stdin_delivered_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stdin_delivered_cursor >= 0 AND stdin_delivered_cursor <= stdin_cursor),
    stdin_closed_at TIMESTAMPTZ,
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    exited_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (materialization_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, instance_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (instance_lease_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, write_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (write_lease_id)
);

CREATE TABLE workspace_pty_sessions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    materialization_id UUID,
    instance_lease_id UUID,
    write_lease_id UUID,
    cwd TEXT NOT NULL DEFAULT '',
    cols INTEGER NOT NULL CHECK (cols > 0),
    rows INTEGER NOT NULL CHECK (rows > 0),
    resize_cols INTEGER CHECK (resize_cols IS NULL OR resize_cols > 0),
    resize_rows INTEGER CHECK (resize_rows IS NULL OR resize_rows > 0),
    filesystem_mode workspace_filesystem_mode NOT NULL DEFAULT 'write',
    state workspace_pty_state NOT NULL DEFAULT 'creating',
    process_id TEXT NOT NULL DEFAULT '',
    output_cursor BIGINT NOT NULL DEFAULT 0 CHECK (output_cursor >= 0),
    input_cursor BIGINT NOT NULL DEFAULT 0 CHECK (input_cursor >= 0),
    input_delivered_cursor BIGINT NOT NULL DEFAULT 0 CHECK (input_delivered_cursor >= 0 AND input_delivered_cursor <= input_cursor),
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	    error JSONB NOT NULL DEFAULT '{}'::jsonb,
	    CHECK (
	        (resize_cols IS NULL AND resize_rows IS NULL)
	        OR (resize_cols IS NOT NULL AND resize_rows IS NOT NULL)
	    ),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (materialization_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, instance_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (instance_lease_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, write_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (write_lease_id)
);

CREATE TABLE workspace_ports (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    materialization_id UUID NOT NULL,
    owner_run_id UUID,
    owner_exec_id UUID,
    owner_pty_session_id UUID,
    port INTEGER NOT NULL CHECK (port > 0 AND port <= 65535),
    protocol workspace_port_protocol NOT NULL DEFAULT 'http',
    state workspace_port_state NOT NULL DEFAULT 'exposing',
    auth_mode workspace_port_auth_mode NOT NULL DEFAULT 'private',
    url TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    opened_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    CHECK (num_nonnulls(owner_run_id, owner_exec_id, owner_pty_session_id) = 1),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, owner_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, owner_exec_id)
        REFERENCES workspace_execs(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, owner_pty_session_id)
        REFERENCES workspace_pty_sessions(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_owner_exec_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, owner_exec_id)
    REFERENCES workspace_execs(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE CASCADE;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_owner_pty_session_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, owner_pty_session_id)
    REFERENCES workspace_pty_sessions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE CASCADE;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_owner_port_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, owner_port_id)
    REFERENCES workspace_ports(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE CASCADE;

CREATE TABLE workspace_versions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    parent_version_id UUID,
    source_materialization_id UUID,
    source_write_lease_id UUID,
    produced_by_run_id UUID,
    produced_by_exec_id UUID,
    kind workspace_version_kind NOT NULL DEFAULT 'user',
    state workspace_version_state NOT NULL DEFAULT 'capturing',
    artifact_id UUID,
    artifact_encoding TEXT NOT NULL DEFAULT '',
    artifact_entry_count INTEGER NOT NULL DEFAULT 0 CHECK (artifact_entry_count >= 0),
    content_digest TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    message TEXT NOT NULL DEFAULT '',
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    promoted_at TIMESTAMPTZ,
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, workspace_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id, state),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, parent_version_id)
        REFERENCES workspace_versions(org_id, workspace_id, id)
        ON DELETE SET NULL (parent_version_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, source_materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (source_materialization_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, source_write_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (source_write_lease_id),
    FOREIGN KEY (org_id, project_id, environment_id, artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (artifact_id)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, project_id, environment_id, produced_by_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (produced_by_run_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, produced_by_exec_id)
        REFERENCES workspace_execs(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (produced_by_exec_id),
    CHECK (
        state NOT IN ('artifact_verified', 'ready')
        OR (
            artifact_id IS NOT NULL
            AND artifact_encoding <> ''
            AND content_digest <> ''
            AND size_bytes >= 0
        )
    )
);

ALTER TABLE workspace_materializations
    ADD CONSTRAINT workspace_materializations_base_version_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, base_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (base_version_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_base_version_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, base_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (base_version_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_acquired_version_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, acquired_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (acquired_version_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_current_version_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, id, current_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (current_version_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_current_version_ready_fkey
    FOREIGN KEY (org_id, project_id, environment_id, id, current_version_id, current_version_required_state)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id, state)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE workspace_exec_stream_chunks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    exec_id UUID NOT NULL,
    stream workspace_exec_stream NOT NULL,
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, exec_id, stream, offset_start),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, exec_id)
        REFERENCES workspace_execs(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_exec_stream_chunk_receipts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    exec_id UUID NOT NULL,
    stream workspace_exec_stream NOT NULL,
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data_sha256 BYTEA NOT NULL CHECK (length(data_sha256) = 32),
    data_size INTEGER NOT NULL CHECK (data_size >= 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, exec_id, stream, offset_start),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, exec_id)
        REFERENCES workspace_execs(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_pty_stream_chunks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    pty_session_id UUID NOT NULL,
    stream workspace_pty_stream NOT NULL,
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, pty_session_id, stream, offset_start),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, pty_session_id)
        REFERENCES workspace_pty_sessions(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_pty_stream_chunk_receipts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    pty_session_id UUID NOT NULL,
    stream workspace_pty_stream NOT NULL,
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data_sha256 BYTEA NOT NULL CHECK (length(data_sha256) = 32),
    data_size INTEGER NOT NULL CHECK (data_size >= 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, pty_session_id, stream, offset_start),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, pty_session_id)
        REFERENCES workspace_pty_sessions(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_operation_idempotencies (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID,
    operation_kind workspace_operation_idempotency_kind NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (btrim(idempotency_key) <> ''),
    request_fingerprint TEXT NOT NULL CHECK (btrim(request_fingerprint) <> ''),
    response_resource_type TEXT NOT NULL DEFAULT '',
    response_resource_id UUID,
    response_body JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_materialization_operations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    materialization_id UUID NOT NULL,
    operation_kind workspace_materialization_operation_kind NOT NULL,
    resource_kind workspace_resource_kind NOT NULL,
    resource_id UUID NOT NULL,
    request_fingerprint TEXT NOT NULL CHECK (btrim(request_fingerprint) <> ''),
    operation_expires_at TIMESTAMPTZ NOT NULL,
    state workspace_materialization_operation_state NOT NULL DEFAULT 'queued',
    priority INTEGER NOT NULL DEFAULT 0,
    instance_lease_id UUID,
    write_lease_id UUID,
    fencing_token TEXT NOT NULL DEFAULT '',
    fencing_generation BIGINT NOT NULL CHECK (fencing_generation > 0),
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    result JSONB NOT NULL DEFAULT '{}'::jsonb,
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    claimed_by_worker_instance_id UUID,
    claim_token TEXT NOT NULL DEFAULT '',
    claim_attempt INTEGER NOT NULL DEFAULT 0 CHECK (claim_attempt >= 0),
    claim_expires_at TIMESTAMPTZ,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    UNIQUE (org_id, materialization_id, id),
    CHECK (
        (
            operation_kind = 'start_exec'
            AND resource_kind = 'workspace_exec'
            AND resource_id IS NOT NULL
        )
        OR (
            operation_kind IN ('create_pty', 'resize_pty', 'close_pty')
            AND resource_kind = 'workspace_pty'
            AND resource_id IS NOT NULL
        )
    ),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, instance_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (instance_lease_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, write_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (write_lease_id),
    FOREIGN KEY (claimed_by_worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE SET NULL
);

CREATE TABLE deployment_streams (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    direction stream_direction NOT NULL,
    schema_fingerprint TEXT NOT NULL DEFAULT '',
    schema_json JSONB NOT NULL DEFAULT 'null'::jsonb,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id, name, direction),
    UNIQUE (org_id, deployment_id, name, direction),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE streams (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    deployment_stream_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    direction stream_direction NOT NULL,
    schema_fingerprint TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    next_sequence BIGINT NOT NULL DEFAULT 1 CHECK (next_sequence > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id, session_id, direction),
    UNIQUE (org_id, session_id, name, direction),
    FOREIGN KEY (org_id, project_id, environment_id, session_id)
        REFERENCES sessions(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_stream_id, name, direction)
        REFERENCES deployment_streams(org_id, project_id, environment_id, id, name, direction)
        ON DELETE CASCADE
);

CREATE TABLE tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    state token_state NOT NULL DEFAULT 'pending',
    timeout_at TIMESTAMPTZ NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    idempotency_key_expires_at TIMESTAMPTZ,
    create_request_fingerprint TEXT NOT NULL DEFAULT '',
    callback_key_id TEXT NOT NULL DEFAULT '',
    callback_secret_fingerprint TEXT NOT NULL DEFAULT '',
    callback_secret_created_at TIMESTAMPTZ,
    completion_fingerprint TEXT NOT NULL DEFAULT '',
    completion_data JSONB,
    completion_content_type TEXT NOT NULL DEFAULT 'application/json',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    expired_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE public_access_tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    state public_access_token_state NOT NULL DEFAULT 'active',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    expired_at TIMESTAMPTZ,
    max_uses INTEGER CHECK (max_uses IS NULL OR max_uses > 0),
    used_count INTEGER NOT NULL DEFAULT 0 CHECK (used_count >= 0),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    CHECK (max_uses IS NULL OR used_count <= max_uses),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE public_access_token_scopes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    public_access_token_id UUID NOT NULL,
    scope_type public_access_token_scope_type NOT NULL,
    token_id UUID,
    stream_id UUID,
    correlation_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    CHECK (
        (
            scope_type = 'token.complete'
            AND token_id IS NOT NULL
            AND stream_id IS NULL
        )
        OR (
            scope_type IN ('session.input.send', 'session.output.read')
            AND token_id IS NULL
            AND stream_id IS NOT NULL
        )
    ),
    FOREIGN KEY (org_id, project_id, environment_id, public_access_token_id)
        REFERENCES public_access_tokens(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, token_id)
        REFERENCES tokens(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, stream_id)
        REFERENCES streams(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE stream_records (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    stream_id UUID NOT NULL,
    direction stream_direction NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    data JSONB NOT NULL DEFAULT 'null'::jsonb,
    correlation_id TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT 'application/json',
    idempotency_key TEXT NOT NULL DEFAULT '',
    idempotency_fingerprint TEXT NOT NULL DEFAULT '',
    source_type stream_record_source_type NOT NULL,
    source_id TEXT NOT NULL DEFAULT '',
    public_access_token_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, stream_id, sequence),
    UNIQUE (org_id, stream_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, stream_id, session_id, direction)
        REFERENCES streams(org_id, project_id, environment_id, id, session_id, direction)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, public_access_token_id)
        REFERENCES public_access_tokens(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (public_access_token_id)
);

CREATE TABLE session_run_requests (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    stream_record_id UUID NOT NULL,
    stream_id UUID NOT NULL,
    cause_kind TEXT NOT NULL CHECK (cause_kind = 'stream_record'),
    status TEXT NOT NULL DEFAULT 'accepted' CHECK (status IN ('accepted', 'claimed', 'created', 'skipped', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ,
    claim_expires_at TIMESTAMPTZ,
    claim_owner TEXT NOT NULL DEFAULT '',
    run_id UUID,
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_id, environment_id, stream_record_id),
    FOREIGN KEY (org_id, project_id, environment_id, session_id)
        REFERENCES sessions(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, stream_record_id)
        REFERENCES stream_records(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, stream_id)
        REFERENCES streams(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (run_id)
);

ALTER TABLE runs
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
    worker_group_id UUID NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
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
    run_lease_id UUID,
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

CREATE TABLE workspace_stream_wakeups (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    resource_kind workspace_resource_kind NOT NULL,
    resource_id UUID NOT NULL,
    stream TEXT NOT NULL CHECK (btrim(stream) <> ''),
    cursor_offset BIGINT NOT NULL CHECK (cursor_offset >= 0),
    notification_kind workspace_stream_notification_kind NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE INDEX workspace_stream_wakeups_ready_idx
    ON workspace_stream_wakeups (locked_until, id)
    WHERE attempts < 25;

CREATE TYPE run_log_stream AS ENUM (
    'stdout',
    'stderr'
);

CREATE TABLE run_log_chunks (
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    run_lease_id UUID NOT NULL,
    attempt_number INTEGER NOT NULL DEFAULT 1 CHECK (attempt_number > 0),
    stream run_log_stream NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    observed_seq BIGINT NOT NULL CHECK (observed_seq >= 0),
    content BYTEA NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, seq),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE
);

CREATE TABLE run_leases (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_group_id UUID NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    dispatch_message_id TEXT NOT NULL CHECK (btrim(dispatch_message_id) <> ''),
    dispatch_lease_id TEXT NOT NULL CHECK (btrim(dispatch_lease_id) <> ''),
    dispatch_attempt INTEGER NOT NULL CHECK (dispatch_attempt > 0),
    status run_lease_status NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    runtime_id TEXT NOT NULL CHECK (btrim(runtime_id) <> ''),
    worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v1' CHECK (btrim(worker_protocol_version) <> ''),
    active_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_duration_ms >= 0),
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    span_id TEXT NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000'),
    parent_span_id TEXT NOT NULL CHECK (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000'),
    traceparent TEXT NOT NULL CHECK (traceparent = '00-' || trace_id || '-' || span_id || '-01'),
    restore_runtime_checkpoint_id UUID,
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
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
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
    run_lease_id UUID,
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
    FOREIGN KEY (org_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, run_id, id)
        ON DELETE SET NULL (run_lease_id)
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
    run_lease_id UUID,
    snapshot_version BIGINT NOT NULL CHECK (snapshot_version > 0),
    decision run_retry_decision_kind NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    error_class TEXT NOT NULL DEFAULT '',
    retry_after TIMESTAMPTZ,
    next_attempt_number INTEGER CHECK (next_attempt_number IS NULL OR next_attempt_number > 0),
    policy_snapshot JSONB NOT NULL DEFAULT '{"enabled": false}'::jsonb,
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_id, attempt_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, attempt_id)
        REFERENCES run_attempts(org_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, run_id, id)
        ON DELETE SET NULL (run_lease_id),
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
    run_lease_id UUID NOT NULL,
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    concurrency_key TEXT,
    slot_ordinal INTEGER NOT NULL CHECK (slot_ordinal > 0),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, run_lease_id),
    FOREIGN KEY (org_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, run_id, id)
        ON DELETE CASCADE
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE run_log_chunks
    ADD CONSTRAINT run_log_chunks_run_lease_id_fkey
    FOREIGN KEY (org_id, run_id, run_lease_id)
    REFERENCES run_leases(org_id, run_id, id)
    ON DELETE CASCADE;

ALTER TABLE events
    ADD CONSTRAINT events_run_lease_id_fkey
    FOREIGN KEY (org_id, run_id, run_lease_id)
    REFERENCES run_leases(org_id, run_id, id)
    ON DELETE SET NULL (run_lease_id);

ALTER TABLE runs
    ADD CONSTRAINT runs_current_run_lease_id_fkey
    FOREIGN KEY (org_id, id, current_run_lease_id)
    REFERENCES run_leases(org_id, run_id, id)
    ON DELETE SET NULL (current_run_lease_id);

CREATE TABLE runtime_checkpoints (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    source_workspace_lease_id UUID NOT NULL,
    materialization_id UUID NOT NULL,
    base_workspace_version_id UUID NOT NULL,
    state runtime_checkpoint_state NOT NULL DEFAULT 'creating',
    runtime_backend TEXT NOT NULL CHECK (btrim(runtime_backend) <> ''),
    runtime_id TEXT NOT NULL CHECK (btrim(runtime_id) <> ''),
    runtime_arch TEXT NOT NULL CHECK (btrim(runtime_arch) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    initramfs_digest TEXT NOT NULL CHECK (btrim(initramfs_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    runtime_config_digest TEXT NOT NULL CHECK (btrim(runtime_config_digest) <> ''),
    runtime_vcpus INTEGER CHECK (runtime_vcpus IS NULL OR runtime_vcpus > 0),
    runtime_memory_mib INTEGER CHECK (runtime_memory_mib IS NULL OR runtime_memory_mib > 0),
    runtime_scratch_disk_mib INTEGER CHECK (runtime_scratch_disk_mib IS NULL OR runtime_scratch_disk_mib > 0),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    image_key TEXT,
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    restoring_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, project_id, environment_id, run_id, id),
    FOREIGN KEY (runtime_id)
        REFERENCES runtime_releases(runtime_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, source_workspace_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialization_id)
        REFERENCES workspace_materializations(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, base_workspace_version_id)
        REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE RESTRICT
);

CREATE TYPE runtime_checkpoint_artifact_role AS ENUM (
    'runtime_config',
    'vm_state',
    'memory',
    'scratch_disk'
);

CREATE TABLE runtime_checkpoint_artifacts (
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    runtime_checkpoint_id UUID NOT NULL,
    role runtime_checkpoint_artifact_role NOT NULL,
    ordinal INTEGER NOT NULL DEFAULT 0 CHECK (ordinal >= 0),
    artifact_id UUID NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL CHECK (btrim(media_type) <> ''),
    digest TEXT NOT NULL CHECK (btrim(digest) <> ''),
    encrypt_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (encrypt_duration_ms >= 0),
    store_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (store_duration_ms >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, runtime_checkpoint_id, role, ordinal),
    FOREIGN KEY (org_id, project_id, environment_id, run_id, runtime_checkpoint_id)
        REFERENCES runtime_checkpoints(org_id, project_id, environment_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, project_id, environment_id, artifact_id, digest)
        REFERENCES artifacts(org_id, project_id, environment_id, id, digest)
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE runs
    ADD CONSTRAINT runs_latest_runtime_checkpoint_id_fkey
    FOREIGN KEY (org_id, id, latest_runtime_checkpoint_id)
    REFERENCES runtime_checkpoints(org_id, run_id, id)
    ON DELETE SET NULL (latest_runtime_checkpoint_id);

ALTER TABLE run_leases
    ADD CONSTRAINT run_leases_restore_runtime_checkpoint_id_fkey
    FOREIGN KEY (org_id, run_id, restore_runtime_checkpoint_id)
    REFERENCES runtime_checkpoints(org_id, run_id, id)
    ON DELETE SET NULL (restore_runtime_checkpoint_id);

CREATE TYPE run_usage_event_kind AS ENUM (
    'active_time',
    'log_bytes',
    'output_bytes',
    'checkpoint_bytes'
);

CREATE TYPE run_usage_event_unit AS ENUM (
    'ms',
    'bytes'
);

CREATE TABLE run_usage_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_id UUID,
    run_lease_id UUID,
    runtime_checkpoint_id UUID,
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    snapshot_version BIGINT NOT NULL CHECK (snapshot_version > 0),
    kind run_usage_event_kind NOT NULL,
    quantity BIGINT NOT NULL CHECK (quantity >= 0),
    unit run_usage_event_unit NOT NULL,
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
    FOREIGN KEY (org_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, run_id, id)
        ON DELETE SET NULL (run_lease_id),
    FOREIGN KEY (org_id, run_id, runtime_checkpoint_id)
        REFERENCES runtime_checkpoints(org_id, run_id, id)
        ON DELETE SET NULL (runtime_checkpoint_id),
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

CREATE TABLE run_waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    kind run_wait_kind NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    state run_wait_state NOT NULL DEFAULT 'parking',
    timeout_at TIMESTAMPTZ,
    runtime_checkpoint_id UUID,
    workspace_version_id UUID,
    active_elapsed_ms_at_park BIGINT CHECK (active_elapsed_ms_at_park IS NULL OR active_elapsed_ms_at_park >= 0),
    parked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    resumed_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, run_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, run_id, runtime_checkpoint_id)
        REFERENCES runtime_checkpoints(org_id, project_id, environment_id, run_id, id)
        ON DELETE SET NULL (runtime_checkpoint_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_version_id)
        REFERENCES workspace_versions(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (workspace_version_id),
    CHECK (
        state <> 'waiting'
        OR (runtime_checkpoint_id IS NOT NULL AND workspace_version_id IS NOT NULL AND active_elapsed_ms_at_park IS NOT NULL)
    )
);

CREATE TABLE stream_waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_wait_id UUID NOT NULL,
    stream_id UUID NOT NULL,
    after_sequence BIGINT NOT NULL DEFAULT 0 CHECK (after_sequence >= 0),
    correlation_id TEXT NOT NULL DEFAULT '',
    matched_record_id UUID,
    cursor_advanced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_wait_id),
    UNIQUE (org_id, project_id, environment_id, run_wait_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_wait_id)
        REFERENCES run_waits(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, stream_id)
        REFERENCES streams(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, stream_id, matched_record_id)
        REFERENCES stream_records(org_id, stream_id, id)
        ON DELETE SET NULL (matched_record_id)
);

CREATE TABLE token_waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_wait_id UUID NOT NULL,
    token_id UUID NOT NULL,
    matched_completion_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_wait_id),
    UNIQUE (org_id, project_id, environment_id, run_wait_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_wait_id)
        REFERENCES run_waits(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, token_id)
        REFERENCES tokens(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE timer_waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_wait_id UUID NOT NULL,
    fire_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_wait_id),
    UNIQUE (org_id, project_id, environment_id, run_wait_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_wait_id)
        REFERENCES run_waits(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
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
CREATE INDEX runs_schedule_idx
    ON runs (org_id, project_id, environment_id, schedule_id, created_at DESC)
    WHERE schedule_id IS NOT NULL;
CREATE INDEX runs_schedule_id_idx
    ON runs (schedule_id)
    WHERE schedule_id IS NOT NULL;
CREATE INDEX runs_schedule_instance_id_idx
    ON runs (org_id, project_id, environment_id, schedule_instance_id)
    WHERE schedule_instance_id IS NOT NULL;
CREATE INDEX runs_queued_expiry_idx
    ON runs(org_id, queued_expires_at)
    WHERE status = 'queued' AND queued_expires_at IS NOT NULL;
CREATE INDEX runs_queued_queue_scope_idx
    ON runs(org_id, project_id, environment_id, queue_name, priority DESC, queue_timestamp, id)
    WHERE status = 'queued' AND current_run_lease_id IS NULL;
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
CREATE INDEX auth_sessions_user_active_idx ON auth_sessions(user_id) WHERE revoked_at IS NULL;
CREATE INDEX auth_sessions_expiry_active_idx ON auth_sessions(expires_at) WHERE revoked_at IS NULL;
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
CREATE UNIQUE INDEX api_key_grants_unique_idx ON api_key_grants(org_id, api_key_id, permission);
CREATE INDEX device_codes_pending_expiry_idx ON device_codes(expires_at) WHERE status = 'pending';
CREATE INDEX worker_bootstrap_tokens_active_idx ON worker_bootstrap_tokens(created_at)
    WHERE revoked_at IS NULL;
CREATE INDEX worker_instances_status_seen_idx ON worker_instances(status, last_seen_at DESC);
CREATE INDEX worker_instances_worker_group_status_seen_idx
    ON worker_instances(worker_group_id, status, last_seen_at DESC);
CREATE INDEX worker_instances_capacity_idx ON worker_instances(available_milli_cpu, available_memory_mib, available_execution_slots)
    WHERE status = 'active';
CREATE UNIQUE INDEX runtime_release_selections_singleton_idx ON runtime_release_selections((true));
CREATE UNIQUE INDEX worker_instance_credentials_worker_instance_one_active_idx ON worker_instance_credentials(worker_instance_id)
    WHERE revoked_at IS NULL;
CREATE INDEX secrets_key_id_updated_idx ON secrets(key_id, updated_at ASC, id ASC);
CREATE INDEX environments_current_deployment_idx
    ON environments(org_id, project_id, current_deployment_id)
    WHERE current_deployment_id IS NOT NULL;
CREATE INDEX deployment_promotions_deployment_idx
    ON deployment_promotions(org_id, project_id, environment_id, deployment_id);
CREATE INDEX deployment_promotions_environment_created_idx
    ON deployment_promotions(org_id, project_id, environment_id, created_at DESC);
CREATE UNIQUE INDEX deployments_reusable_build_key_idx
    ON deployments(org_id, project_id, environment_id, worker_group_id, content_hash)
    WHERE status IN ('queued', 'building');
CREATE INDEX deployments_worker_group_status_idx
    ON deployments(worker_group_id, status, created_at)
    WHERE status IN ('queued', 'building');
CREATE INDEX artifacts_scope_kind_created_idx
    ON artifacts(org_id, project_id, environment_id, kind, created_at DESC);
CREATE INDEX artifacts_digest_idx
    ON artifacts(digest);
CREATE INDEX deployment_tasks_lookup_idx
    ON deployment_tasks(org_id, project_id, environment_id, task_id);
CREATE INDEX deployment_sandboxes_lookup_idx
    ON deployment_sandboxes(org_id, project_id, environment_id, deployment_id, sandbox_id);
CREATE UNIQUE INDEX run_log_chunks_observed_idx ON run_log_chunks(org_id, run_id, run_lease_id, stream, observed_seq);
CREATE INDEX events_run_id_idx ON events(run_id)
    WHERE run_id IS NOT NULL;
CREATE INDEX events_deployment_id_idx ON events(deployment_id)
    WHERE deployment_id IS NOT NULL;
CREATE INDEX events_run_lease_idx ON events(org_id, run_id, run_lease_id, seq)
    WHERE run_lease_id IS NOT NULL;
CREATE INDEX events_run_attempt_idx ON events(org_id, run_id, attempt_number, seq)
    WHERE attempt_number IS NOT NULL;
CREATE INDEX run_attempts_run_status_idx ON run_attempts(org_id, run_id, status, attempt_number);
CREATE UNIQUE INDEX run_leases_one_active_per_run_idx ON run_leases(run_id)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_leases_attempt_idx ON run_leases(org_id, run_id, attempt_id, leased_at DESC);
CREATE INDEX run_leases_active_lease_idx ON run_leases(org_id, status, lease_expires_at)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_leases_worker_instance_status_idx ON run_leases(org_id, worker_instance_id, status);
CREATE INDEX run_leases_worker_group_idx ON run_leases(worker_group_id);
CREATE INDEX run_runtime_requirements_worker_group_idx
    ON run_runtime_requirements(worker_group_id);
CREATE INDEX run_runtime_requirements_worker_scope_idx
    ON run_runtime_requirements(worker_group_id, org_id, run_id);
CREATE INDEX run_snapshots_run_created_idx ON run_snapshots(org_id, run_id, created_at DESC);
CREATE INDEX runtime_checkpoints_run_state_idx ON runtime_checkpoints(run_id, state, created_at DESC);
CREATE INDEX runtime_checkpoint_artifacts_role_idx ON runtime_checkpoint_artifacts(org_id, run_id, runtime_checkpoint_id, role, ordinal);
CREATE INDEX tokens_scope_state_idx ON tokens(org_id, project_id, environment_id, state, created_at DESC);
CREATE UNIQUE INDEX tokens_idempotency_idx ON tokens(org_id, project_id, environment_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX tokens_timeout_pending_idx ON tokens(org_id, timeout_at)
    WHERE state = 'pending';
CREATE INDEX tokens_callback_fingerprint_pending_idx ON tokens(callback_key_id, callback_secret_fingerprint)
    WHERE state = 'pending' AND callback_key_id <> '' AND callback_secret_fingerprint <> '';
CREATE INDEX run_waits_run_state_idx ON run_waits(org_id, run_id, state, parked_at DESC);
CREATE INDEX run_waits_timeout_idx ON run_waits(org_id, timeout_at)
    WHERE state = 'waiting' AND timeout_at IS NOT NULL;
CREATE INDEX stream_waits_open_idx ON stream_waits(org_id, stream_id, after_sequence, run_wait_id)
    WHERE matched_record_id IS NULL;
CREATE INDEX stream_waits_matched_record_idx ON stream_waits(org_id, stream_id, matched_record_id)
    WHERE matched_record_id IS NOT NULL;
CREATE INDEX token_waits_token_idx ON token_waits(org_id, token_id, run_wait_id);
CREATE INDEX timer_waits_fire_idx ON timer_waits(org_id, fire_at, run_wait_id);
CREATE INDEX tasks_scope_updated_idx ON tasks(org_id, project_id, environment_id, updated_at DESC);
CREATE UNIQUE INDEX task_schedules_internal_dedup_active_idx
    ON task_schedules (org_id, project_id, schedule_type, dedup_key);
CREATE UNIQUE INDEX task_schedules_user_dedup_active_idx
    ON task_schedules (org_id, project_id, user_dedup_key)
    WHERE user_dedup_key IS NOT NULL;
CREATE INDEX task_schedules_scope_created_idx
    ON task_schedules (org_id, project_id, created_at DESC, id DESC);
CREATE INDEX task_schedule_instances_environment_idx
    ON task_schedule_instances (org_id, project_id, environment_id, active);
CREATE INDEX task_schedule_instances_index_due_idx
    ON task_schedule_instances (coalesce(retry_after, next_fire_at), id)
    WHERE active AND next_fire_at IS NOT NULL;
CREATE UNIQUE INDEX sessions_external_id_idx ON sessions(org_id, project_id, environment_id, external_id)
    WHERE external_id <> '';
CREATE INDEX sessions_scope_status_updated_idx ON sessions(org_id, project_id, environment_id, status, updated_at DESC);
CREATE INDEX sessions_tags_idx ON sessions USING GIN (tags);
CREATE INDEX session_start_idempotencies_expiry_idx ON session_start_idempotencies(org_id, project_id, environment_id, expires_at);
CREATE INDEX session_runs_timeline_idx ON session_runs(org_id, session_id, turn_index, created_at);
CREATE INDEX session_run_requests_pending_idx ON session_run_requests(next_attempt_at, created_at)
    WHERE status IN ('accepted', 'claimed');
CREATE INDEX workspaces_state_idx ON workspaces(org_id, project_id, environment_id, state, updated_at DESC);
CREATE INDEX workspaces_tags_idx ON workspaces USING GIN (tags);
CREATE UNIQUE INDEX workspaces_external_id_idx ON workspaces(org_id, project_id, environment_id, external_id)
    WHERE external_id <> '';
CREATE INDEX workspace_versions_workspace_created_idx ON workspace_versions(org_id, workspace_id, created_at DESC);
CREATE UNIQUE INDEX workspace_materializations_one_active_idx ON workspace_materializations(workspace_id)
    WHERE state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping');
CREATE INDEX workspace_materializations_claim_idx
    ON workspace_materializations(state, priority DESC, requested_at ASC, claim_attempt ASC)
    WHERE state IN ('requested', 'materializing');
CREATE INDEX workspace_materializations_heartbeat_idx
    ON workspace_materializations(org_id, state, last_heartbeat_at)
    WHERE state IN ('materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping');
CREATE UNIQUE INDEX workspace_leases_one_active_writer_workspace_idx ON workspace_leases(workspace_id)
    WHERE lease_kind = 'write' AND state IN ('active', 'releasing');
CREATE UNIQUE INDEX workspace_leases_one_active_writer_materialization_idx ON workspace_leases(materialization_id)
    WHERE lease_kind = 'write' AND state IN ('active', 'releasing');
CREATE INDEX workspace_leases_expiry_idx ON workspace_leases(org_id, expires_at)
    WHERE state IN ('active', 'releasing');
ALTER TABLE workspace_exec_stream_chunks
    ADD CONSTRAINT workspace_exec_stream_chunks_no_overlap
    EXCLUDE USING gist (
        exec_id WITH =,
        stream WITH =,
        int8range(offset_start, offset_end, '[)') WITH &&
    );
ALTER TABLE workspace_exec_stream_chunk_receipts
    ADD CONSTRAINT workspace_exec_stream_chunk_receipts_no_overlap
    EXCLUDE USING gist (
        exec_id WITH =,
        stream WITH =,
        int8range(offset_start, offset_end, '[)') WITH &&
    );
ALTER TABLE workspace_pty_stream_chunks
    ADD CONSTRAINT workspace_pty_stream_chunks_no_overlap
    EXCLUDE USING gist (
        pty_session_id WITH =,
        stream WITH =,
        int8range(offset_start, offset_end, '[)') WITH &&
    );
ALTER TABLE workspace_pty_stream_chunk_receipts
    ADD CONSTRAINT workspace_pty_stream_chunk_receipts_no_overlap
    EXCLUDE USING gist (
        pty_session_id WITH =,
        stream WITH =,
        int8range(offset_start, offset_end, '[)') WITH &&
    );
CREATE UNIQUE INDEX workspace_ports_active_idx ON workspace_ports(materialization_id, port, protocol)
    WHERE state IN ('exposing', 'open');
CREATE UNIQUE INDEX workspace_operation_idempotencies_workspace_idx
    ON workspace_operation_idempotencies(org_id, project_id, environment_id, operation_kind, workspace_id, idempotency_key)
    WHERE workspace_id IS NOT NULL;
CREATE UNIQUE INDEX workspace_operation_idempotencies_environment_idx
    ON workspace_operation_idempotencies(org_id, project_id, environment_id, operation_kind, idempotency_key)
    WHERE workspace_id IS NULL;
CREATE INDEX workspace_materialization_operations_claim_idx
    ON workspace_materialization_operations(materialization_id, state, operation_expires_at, claim_expires_at, priority DESC, requested_at ASC)
    WHERE state IN ('queued', 'claimed');
CREATE INDEX workspace_materialization_operations_worker_claim_idx
    ON workspace_materialization_operations(claimed_by_worker_instance_id, state, claim_expires_at)
    WHERE state = 'claimed';
CREATE UNIQUE INDEX workspace_materialization_operations_active_resource_idx
    ON workspace_materialization_operations(org_id, project_id, environment_id, materialization_id, operation_kind, resource_kind, resource_id)
    WHERE state IN ('queued', 'claimed', 'running') AND resource_id IS NOT NULL;
CREATE INDEX deployment_streams_lookup_idx ON deployment_streams(org_id, project_id, environment_id, deployment_id, name, direction);
CREATE UNIQUE INDEX streams_session_name_idx ON streams(org_id, session_id, name, direction);
CREATE INDEX stream_records_sequence_idx ON stream_records(org_id, stream_id, sequence, id);
CREATE INDEX stream_records_correlation_sequence_idx ON stream_records(org_id, stream_id, correlation_id, sequence, id)
    WHERE correlation_id <> '';
CREATE UNIQUE INDEX stream_records_idempotency_idx ON stream_records(org_id, stream_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX public_access_tokens_scope_expiry_idx ON public_access_tokens(org_id, project_id, environment_id, expires_at)
    WHERE state = 'active';
CREATE INDEX public_access_token_scopes_token_idx ON public_access_token_scopes(org_id, token_id, scope_type)
    WHERE token_id IS NOT NULL;
CREATE INDEX public_access_token_scopes_stream_idx ON public_access_token_scopes(org_id, stream_id, scope_type)
    WHERE stream_id IS NOT NULL;

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

CREATE TRIGGER tokens_set_updated_at
    BEFORE UPDATE ON tokens
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER public_access_tokens_set_updated_at
    BEFORE UPDATE ON public_access_tokens
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER run_waits_set_updated_at
    BEFORE UPDATE ON run_waits
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
