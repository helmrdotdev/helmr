CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE organizations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^org_[a-z2-7]{26}$'),
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

CREATE TYPE region_state AS ENUM (
    'available',
    'draining',
    'disabled'
);

CREATE TYPE region_visibility AS ENUM (
    'public',
    'allowlisted',
    'hidden'
);

CREATE TYPE worker_group_health_state AS ENUM (
    'healthy',
    'degraded',
    'unavailable'
);

CREATE TYPE worker_trust_tier AS ENUM (
    'helmr_managed',
    'customer_managed'
);

CREATE TYPE worker_group_state AS ENUM (
    'active',
    'draining',
    'disabled'
);

CREATE TYPE telemetry_stream_kind AS ENUM (
    'run_log',
    'event',
    'terminal_output',
    'meter_event'
);

CREATE TYPE telemetry_outbox_state AS ENUM (
    'pending',
    'claimed',
    'written',
    'failed',
    'dead_lettered'
);

CREATE TABLE regions (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    provider TEXT NOT NULL CHECK (btrim(provider) <> ''),
    provider_region TEXT NOT NULL CHECK (btrim(provider_region) <> ''),
    display_name TEXT NOT NULL CHECK (btrim(display_name) <> ''),
    state region_state NOT NULL DEFAULT 'available',
    visibility region_visibility NOT NULL DEFAULT 'public',
    location TEXT NOT NULL DEFAULT '',
    static_ips TEXT[] NOT NULL DEFAULT '{}'::text[],
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_region)
);

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^usr_[a-z2-7]{26}$'),
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^prj_[a-z2-7]{26}$'),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    default_region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id)
);

CREATE TABLE environments (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^env_[a-z2-7]{26}$'),
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^inv_[a-z2-7]{26}$'),
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^apk_[a-z2-7]{26}$'),
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
    org_id UUID NOT NULL,
    digest TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, digest)
);

CREATE TYPE artifact_kind AS ENUM (
    'deployment_source',
    'build_manifest',
    'deployment_manifest',
    'sandbox_image',
    'task_bundle',
    'runtime_substrate',
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
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    owner_org_id UUID REFERENCES organizations(id) ON DELETE RESTRICT,
    region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT 'aws' CHECK (btrim(provider) <> ''),
    state worker_group_state NOT NULL DEFAULT 'active',
    health_state worker_group_health_state NOT NULL DEFAULT 'healthy',
    health_checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    routing_fresh_until TIMESTAMPTZ NOT NULL DEFAULT now() - interval '1 second',
    health_details JSONB NOT NULL DEFAULT '{}'::jsonb,
    trust_tier worker_trust_tier NOT NULL DEFAULT 'helmr_managed',
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (region_id, id),
    UNIQUE (region_id, name),
    UNIQUE (owner_org_id, region_id, name)
);

CREATE TRIGGER worker_groups_set_updated_at
    BEFORE UPDATE ON worker_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

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
    org_id UUID,
    resource_id TEXT NOT NULL CHECK (btrim(resource_id) <> ''),
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    status worker_instance_status NOT NULL DEFAULT 'active',
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
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
    org_id UUID,
    token_hash BYTEA NOT NULL UNIQUE,
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    last_used_by_worker_instance_id UUID REFERENCES worker_instances(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE worker_instance_credentials (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID,
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    worker_instance_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE CASCADE,
    key_prefix TEXT NOT NULL CHECK (btrim(key_prefix) <> ''),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    expires_at TIMESTAMPTZ,
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
    digest TEXT NOT NULL,
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
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, digest)
        REFERENCES cas_objects(org_id, digest)
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

CREATE TYPE wait_kind AS ENUM (
    'stream',
    'token',
    'timer'
);

CREATE TYPE wait_state AS ENUM (
    'pending',
    'completed',
    'failed',
    'expired',
    'cancelled'
);

CREATE TYPE worker_command_kind AS ENUM (
	'runtime_prepare',
	'runtime_resume_wait',
	'runtime_checkpoint_wait',
	'runtime_stop',
	'runtime_substrate_prepare'
);

CREATE TYPE run_wait_state AS ENUM (
    'hot_waiting',
    'checkpointing',
    'checkpointed_waiting',
    'resuming',
    'released',
    'cancelled',
    'failed'
);

CREATE TYPE runtime_checkpoint_state AS ENUM (
    'creating',
    'ready',
    'invalid',
    'deleted'
);

CREATE TYPE runtime_checkpoint_restore_status AS ENUM (
    'restoring',
    'restored',
    'failed',
    'abandoned'
);

CREATE TYPE runtime_instance_state AS ENUM (
    'preparing',
    'ready',
    'binding',
    'running',
    'waiting_hot',
    'checkpointing',
    'stopping',
    'closed',
    'lost',
    'failed'
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

CREATE TYPE run_operation_kind AS ENUM (
    'cancel'
);

CREATE TYPE run_operation_status AS ENUM (
    'requested',
    'applied',
    'rejected'
);

CREATE TYPE session_status AS ENUM (
    'open',
    'closed',
    'cancelled',
    'expired'
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

CREATE TYPE workspace_mount_state AS ENUM (
    'mounting',
    'mounted',
    'unmounting',
    'unmounted',
    'lost',
    'failed'
);

CREATE TYPE workspace_operation_state AS ENUM (
    'queued',
    'claimed',
    'running',
    'completed',
    'failed',
    'cancelled',
    'lost',
    'expired'
);

CREATE TYPE workspace_operation_kind AS ENUM (
    'start_process',
    'resize_process',
    'close_process'
);

CREATE TYPE workspace_stream_notification_kind AS ENUM (
    'chunk',
    'terminal'
);

CREATE TYPE workspace_operation_idempotency_kind AS ENUM (
    'workspace_create',
    'workspace_stop',
    'workspace_command_create',
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

CREATE TYPE workspace_process_state AS ENUM (
    'queued',
    'starting',
    'running',
    'closing',
    'exited',
    'lost',
    'failed'
);

CREATE TYPE deployment_status AS ENUM (
    'queued',
    'building',
    'deployed',
    'failed'
);

CREATE TABLE deployments (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^dep_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    build_worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
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
    FOREIGN KEY (build_worker_instance_id, build_worker_group_id)
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^task_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, task_id),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE deployment_sandboxes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^sbx_[a-z2-7]{26}$'),
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
    UNIQUE (org_id, project_id, environment_id, deployment_id, id),
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

CREATE TABLE runtime_substrate_artifacts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_sandbox_id UUID NOT NULL,
    artifact_id UUID NOT NULL,
    substrate_digest TEXT NOT NULL CHECK (btrim(substrate_digest) <> ''),
    substrate_format TEXT NOT NULL CHECK (btrim(substrate_format) <> ''),
    builder_abi TEXT NOT NULL CHECK (btrim(builder_abi) <> ''),
    layout_abi TEXT NOT NULL CHECK (btrim(layout_abi) <> ''),
    substrate_size_bytes BIGINT NOT NULL CHECK (substrate_size_bytes >= 0),
    source JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by_worker_instance_id UUID REFERENCES worker_instances(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at TIMESTAMPTZ,
    last_referenced_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (org_id, worker_group_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, worker_group_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, deployment_sandbox_id, id),
    UNIQUE (org_id, worker_group_id, project_id, environment_id, deployment_sandbox_id, id),
    UNIQUE (org_id, worker_group_id, project_id, environment_id, deployment_sandbox_id, substrate_digest, substrate_format, builder_abi, layout_abi),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE TRIGGER runtime_substrate_artifacts_set_updated_at
    BEFORE UPDATE ON runtime_substrate_artifacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE deployment_tasks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^dtask_[a-z2-7]{26}$'),
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
    requested_execution_slots INTEGER NOT NULL DEFAULT 1 CHECK (requested_execution_slots > 0),
    secret_declarations JSONB NOT NULL DEFAULT '[]'::jsonb,
    resource_requirements JSONB NOT NULL DEFAULT '{}'::jsonb,
    network_policy JSONB NOT NULL DEFAULT '{"internet": true}'::jsonb,
    placement JSONB NOT NULL DEFAULT '{}'::jsonb,
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
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, deployment_id, id)
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^sch_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    schedule_type task_schedule_type NOT NULL DEFAULT 'imperative',
    task_id TEXT NOT NULL CHECK (btrim(task_id) <> ''),
    dedup_key TEXT NOT NULL CHECK (btrim(dedup_key) <> ''),
    user_dedup_key TEXT CHECK (user_dedup_key IS NULL OR btrim(user_dedup_key) <> ''),
    external_id TEXT,
    cron TEXT NOT NULL CHECK (btrim(cron) <> ''),
    timezone TEXT NOT NULL DEFAULT 'UTC' CHECK (btrim(timezone) <> ''),
    enabled BOOLEAN NOT NULL DEFAULT true,
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
    enabled BOOLEAN NOT NULL DEFAULT true,
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

CREATE TABLE workspaces (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^wsp_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
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
    last_workspace_mount_id UUID,
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^ses_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
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
    expired_at TIMESTAMPTZ,
    terminal_reason JSONB NOT NULL DEFAULT '{}'::jsonb,
    result JSONB,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id, task_id),
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^run_[a-z2-7]{26}$'),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    deployment_task_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    workspace_mount_id UUID,
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
    queue_class TEXT NOT NULL DEFAULT 'default' CHECK (btrim(queue_class) <> ''),
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    queue_concurrency_limit INTEGER,
    concurrency_key TEXT,
    priority INTEGER NOT NULL DEFAULT 0,
    queue_timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl TEXT NOT NULL DEFAULT '',
    queued_expires_at TIMESTAMPTZ,
    dispatch_generation BIGINT NOT NULL DEFAULT 1 CHECK (dispatch_generation > 0),
    dispatch_attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (dispatch_attempt_count >= 0),
    last_enqueue_error TEXT NOT NULL DEFAULT '',
    last_enqueued_at TIMESTAMPTZ,
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
    max_active_duration_ms BIGINT NOT NULL CHECK (max_active_duration_ms > 0),
    active_elapsed_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_elapsed_ms >= 0),
    active_started_at TIMESTAMPTZ,
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    root_span_id TEXT NOT NULL CHECK (root_span_id ~ '^[0-9a-f]{16}$' AND root_span_id <> '0000000000000000'),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    current_attempt_number INTEGER NOT NULL DEFAULT 1 CHECK (current_attempt_number > 0),
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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
    FOREIGN KEY (runtime_id)
        REFERENCES runtime_releases(runtime_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, session_id, task_id)
        REFERENCES sessions(org_id, project_id, environment_id, id, task_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, schedule_id)
        REFERENCES task_schedules(org_id, project_id, id)
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^rop_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE UNIQUE INDEX run_operations_idempotency_idx
    ON run_operations (org_id, project_id, environment_id, run_id, kind, idempotency_key)
    WHERE idempotency_key <> '';

CREATE TABLE session_runs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^srun_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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

CREATE TABLE workspace_mounts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    deployment_sandbox_id UUID NOT NULL,
    sandbox_fingerprint TEXT NOT NULL CHECK (btrim(sandbox_fingerprint) <> ''),
    base_version_id UUID,
    runtime_instance_id UUID,
    claim_attempt INTEGER NOT NULL DEFAULT 0 CHECK (claim_attempt >= 0),
    priority INTEGER NOT NULL DEFAULT 0,
    guestd_channel_token_hash TEXT NOT NULL DEFAULT '',
    guestd_channel_token_expires_at TIMESTAMPTZ,
    state workspace_mount_state NOT NULL DEFAULT 'mounting',
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
    mounted_at TIMESTAMPTZ,
    unmounted_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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
        ON DELETE RESTRICT
);

ALTER TABLE runs
    ADD CONSTRAINT runs_workspace_mount_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, workspace_mount_id)
    REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (workspace_mount_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_last_workspace_mount_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, id, last_workspace_mount_id)
    REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE SET NULL (last_workspace_mount_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE workspace_leases (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    workspace_mount_id UUID NOT NULL,
    lease_kind workspace_lease_kind NOT NULL,
    state workspace_lease_state NOT NULL DEFAULT 'active',
    owner_run_id UUID,
    owner_process_id UUID,
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
    CHECK (num_nonnulls(owner_run_id, owner_process_id) = 1),
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, owner_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_processes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    workspace_mount_id UUID,
    instance_lease_id UUID,
    write_lease_id UUID,
    kind TEXT NOT NULL CHECK (btrim(kind) <> ''),
    command JSONB NOT NULL DEFAULT '[]'::jsonb,
    cwd TEXT NOT NULL DEFAULT '',
    env_shape JSONB NOT NULL DEFAULT '{}'::jsonb,
    filesystem_mode workspace_filesystem_mode NOT NULL DEFAULT 'write',
    state workspace_process_state NOT NULL DEFAULT 'queued',
    detached BOOLEAN NOT NULL DEFAULT false,
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_fingerprint TEXT NOT NULL DEFAULT '',
    runtime_process_id TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    signal TEXT NOT NULL DEFAULT '',
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    pty_cols INTEGER CHECK (pty_cols IS NULL OR pty_cols > 0),
    pty_rows INTEGER CHECK (pty_rows IS NULL OR pty_rows > 0),
    pending_pty_cols INTEGER CHECK (pending_pty_cols IS NULL OR pending_pty_cols > 0),
    pending_pty_rows INTEGER CHECK (pending_pty_rows IS NULL OR pending_pty_rows > 0),
    stdout_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stdout_cursor >= 0),
    stderr_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stderr_cursor >= 0),
    stdin_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stdin_cursor >= 0),
    stdin_delivered_cursor BIGINT NOT NULL DEFAULT 0 CHECK (stdin_delivered_cursor >= 0 AND stdin_delivered_cursor <= stdin_cursor),
    stdin_closed_at TIMESTAMPTZ,
    input_cursor BIGINT NOT NULL DEFAULT 0 CHECK (input_cursor >= 0),
    input_delivered_cursor BIGINT NOT NULL DEFAULT 0 CHECK (input_delivered_cursor >= 0 AND input_delivered_cursor <= input_cursor),
    output_cursor BIGINT NOT NULL DEFAULT 0 CHECK (output_cursor >= 0),
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    exited_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (pending_pty_cols IS NULL AND pending_pty_rows IS NULL)
        OR (pending_pty_cols IS NOT NULL AND pending_pty_rows IS NOT NULL)
    ),
    CHECK (
        kind <> 'pty'
        OR (pty_cols IS NOT NULL AND pty_rows IS NOT NULL)
    ),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (workspace_mount_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, instance_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (instance_lease_id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, write_lease_id)
        REFERENCES workspace_leases(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (write_lease_id)
);

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_owner_process_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, owner_process_id)
    REFERENCES workspace_processes(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE CASCADE;

CREATE TABLE workspace_versions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^wsv_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    parent_version_id UUID,
    source_workspace_mount_id UUID,
    source_write_lease_id UUID,
    produced_by_run_id UUID,
    produced_by_process_id UUID,
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, source_workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (source_workspace_mount_id),
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, produced_by_process_id)
        REFERENCES workspace_processes(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE SET NULL (produced_by_process_id),
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

ALTER TABLE workspace_mounts
    ADD CONSTRAINT workspace_mounts_base_version_id_fkey
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

CREATE TABLE workspace_process_stream_chunks (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    process_id UUID NOT NULL,
    stream_name TEXT NOT NULL CHECK (btrim(stream_name) <> ''),
    direction TEXT NOT NULL CHECK (direction IN ('input', 'output')),
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '7 days',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, process_id, stream_name, offset_start),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, process_id)
        REFERENCES workspace_processes(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

CREATE TABLE workspace_process_stream_receipts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    process_id UUID NOT NULL,
    stream_name TEXT NOT NULL CHECK (btrim(stream_name) <> ''),
    direction TEXT NOT NULL CHECK (direction IN ('input', 'output')),
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data_sha256 BYTEA NOT NULL CHECK (length(data_sha256) = 32),
    data_size INTEGER NOT NULL CHECK (data_size >= 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, process_id, stream_name, offset_start),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, process_id)
        REFERENCES workspace_processes(org_id, project_id, environment_id, workspace_id, id)
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

CREATE TABLE workspace_process_operations (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    workspace_mount_id UUID NOT NULL,
    operation_kind workspace_operation_kind NOT NULL,
    process_id UUID NOT NULL,
    request_fingerprint TEXT NOT NULL CHECK (btrim(request_fingerprint) <> ''),
    operation_expires_at TIMESTAMPTZ NOT NULL,
    state workspace_operation_state NOT NULL DEFAULT 'queued',
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
    UNIQUE (org_id, workspace_mount_id, id),
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, process_id)
        REFERENCES workspace_processes(org_id, project_id, environment_id, workspace_id, id)
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^str_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, session_id)
        REFERENCES sessions(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_stream_id, name, direction)
        REFERENCES deployment_streams(org_id, project_id, environment_id, id, name, direction)
        ON DELETE CASCADE
);

CREATE TABLE tokens (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^tok_[a-z2-7]{26}$'),
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^pat_[a-z2-7]{26}$'),
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
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^srec_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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
    worker_group_id TEXT NOT NULL,
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
    FOREIGN KEY (worker_group_id)
        REFERENCES worker_groups(id)
        ON DELETE RESTRICT,
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

CREATE TYPE event_subject_type AS ENUM (
    'run',
    'deployment'
);

CREATE TABLE telemetry_outbox (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    stream_kind telemetry_stream_kind NOT NULL,
    source_kind TEXT NOT NULL CHECK (btrim(source_kind) <> ''),
    source_id UUID NOT NULL,
    stream_name TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT CHECK (idempotency_key IS NULL OR btrim(idempotency_key) <> ''),
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID,
    deployment_id UUID,
    workspace_id UUID,
    resource_kind TEXT NOT NULL DEFAULT '',
    resource_id UUID,
    run_lease_id UUID,
    attempt_number INTEGER CHECK (attempt_number IS NULL OR attempt_number > 0),
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    parent_span_id TEXT CHECK (parent_span_id IS NULL OR (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000')),
    traceparent TEXT CHECK (
        traceparent IS NULL
        OR (
            trace_id IS NOT NULL
            AND span_id IS NOT NULL
            AND traceparent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'
            AND substring(traceparent from 4 for 32) = trace_id
            AND substring(traceparent from 37 for 16) = span_id
        )
    ),
    category TEXT NOT NULL DEFAULT 'system',
    severity TEXT NOT NULL DEFAULT 'info',
    source TEXT NOT NULL DEFAULT 'control',
    kind TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    content BYTEA,
    size_bytes BIGINT CHECK (size_bytes IS NULL OR size_bytes >= 0),
    observed_seq BIGINT CHECK (observed_seq IS NULL OR observed_seq >= 0),
    offset_start BIGINT CHECK (offset_start IS NULL OR offset_start >= 0),
    offset_end BIGINT CHECK (offset_end IS NULL OR offset_end >= 0),
    redaction_class TEXT NOT NULL DEFAULT 'internal',
    retention_class TEXT NOT NULL DEFAULT 'standard',
    snapshot_version BIGINT CHECK (snapshot_version IS NULL OR snapshot_version > 0),
    state telemetry_outbox_state NOT NULL DEFAULT 'pending',
    retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    next_retry_at TIMESTAMPTZ,
    written_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    publish_attempts INTEGER NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    publish_locked_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        stream_kind <> 'event'
        OR (
            source_kind IN ('run', 'deployment')
            AND source_id = COALESCE(run_id, deployment_id)
            AND btrim(kind) <> ''
            AND (
                (run_id IS NOT NULL AND deployment_id IS NULL)
                OR (deployment_id IS NOT NULL AND run_id IS NULL)
            )
        )
    ),
    CHECK (
        stream_kind <> 'run_log'
        OR (
            source_kind = 'run'
            AND run_id = source_id
            AND stream_name IN ('stdout', 'stderr')
            AND content IS NOT NULL
            AND size_bytes IS NOT NULL
            AND observed_seq IS NOT NULL
            AND offset_start IS NULL
            AND offset_end IS NULL
        )
    ),
    CHECK (
        stream_kind <> 'terminal_output'
        OR (
            source_kind = 'workspace_process'
            AND resource_kind = source_kind
            AND resource_id = source_id
            AND workspace_id IS NOT NULL
            AND stream_name <> ''
            AND content IS NOT NULL
            AND size_bytes IS NOT NULL
            AND offset_start IS NOT NULL
            AND offset_end IS NOT NULL
            AND offset_end >= offset_start
        )
    ),
    CHECK (
        stream_kind <> 'meter_event'
        OR (
            source_kind IN ('run_log', 'run_lease')
            AND run_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND btrim(kind) <> ''
            AND payload IS NOT NULL
            AND content IS NULL
            AND observed_seq IS NULL
            AND offset_start IS NULL
            AND offset_end IS NULL
        )
    )
);

CREATE UNIQUE INDEX telemetry_outbox_idempotency_idx
    ON telemetry_outbox (org_id, stream_kind, source_kind, source_id, stream_name, idempotency_key);
CREATE INDEX telemetry_outbox_publish_ready_idx
    ON telemetry_outbox (created_at, id)
    WHERE stream_kind = 'event' AND published_at IS NULL;
CREATE INDEX telemetry_outbox_ingest_ready_idx
    ON telemetry_outbox (stream_kind, source_kind, source_id, stream_name, id)
    WHERE written_at IS NULL;
CREATE INDEX telemetry_outbox_ingest_claim_idx
    ON telemetry_outbox (stream_kind, id)
    WHERE written_at IS NULL AND state IN ('pending', 'claimed', 'failed');
CREATE INDEX telemetry_outbox_written_gc_idx
    ON telemetry_outbox (id)
    WHERE written_at IS NOT NULL;

CREATE TABLE workspace_process_stream_wakeups (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    process_id UUID NOT NULL,
    stream_name TEXT NOT NULL CHECK (btrim(stream_name) <> ''),
    cursor_offset BIGINT NOT NULL CHECK (cursor_offset >= 0),
    notification_kind workspace_stream_notification_kind NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, process_id)
        REFERENCES workspace_processes(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE CASCADE
);

CREATE INDEX workspace_process_stream_wakeups_ready_idx
    ON workspace_process_stream_wakeups (locked_until, id)
    WHERE attempts < 25;

CREATE TYPE run_log_stream AS ENUM (
    'stdout',
    'stderr'
);

CREATE TABLE run_leases (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    queue_class TEXT NOT NULL DEFAULT 'default' CHECK (btrim(queue_class) <> ''),
    run_id UUID NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    dispatch_message_id TEXT NOT NULL CHECK (btrim(dispatch_message_id) <> ''),
    dispatch_generation BIGINT NOT NULL CHECK (dispatch_generation >= 0),
    dispatch_lease_id TEXT NOT NULL CHECK (btrim(dispatch_lease_id) <> ''),
    dispatch_attempt INTEGER NOT NULL CHECK (dispatch_attempt > 0),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    concurrency_key TEXT,
    status run_lease_status NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    runtime_id TEXT NOT NULL CHECK (btrim(runtime_id) <> ''),
    worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v1' CHECK (btrim(worker_protocol_version) <> ''),
    active_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_duration_ms >= 0),
    trace_id TEXT NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
    span_id TEXT NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000'),
    parent_span_id TEXT NOT NULL CHECK (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000'),
    traceparent TEXT NOT NULL CHECK (
        traceparent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'
        AND substring(traceparent from 4 for 32) = trace_id
        AND substring(traceparent from 37 for 16) = span_id
    ),
    restore_runtime_checkpoint_id UUID,
    leased_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, worker_group_id, run_id, id),
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
        ON DELETE CASCADE
);

CREATE TABLE run_state_snapshots (
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    run_id UUID NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    status run_status NOT NULL,
    execution_status run_execution_status NOT NULL DEFAULT 'queued',
    terminal_outcome run_terminal_outcome,
    attempt_number INTEGER CHECK (attempt_number IS NULL OR attempt_number > 0),
    run_lease_id UUID,
    worker_instance_id UUID,
    runtime_instance_id UUID,
    runtime_checkpoint_id UUID,
    operation_id UUID,
    previous_version BIGINT CHECK (previous_version IS NULL OR previous_version > 0),
    transition TEXT NOT NULL CHECK (btrim(transition) <> ''),
    reason JSONB NOT NULL DEFAULT '{}'::jsonb,
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, version),
    UNIQUE (org_id, run_id, version),
    UNIQUE (org_id, worker_group_id, run_id, version),
    FOREIGN KEY (org_id, run_id)
        REFERENCES runs(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_group_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, worker_group_id, run_id, id)
        ON DELETE SET NULL (run_lease_id)
);

ALTER TABLE run_state_snapshots
    ADD CONSTRAINT run_state_snapshots_operation_id_fkey
    FOREIGN KEY (org_id, run_id, operation_id)
    REFERENCES run_operations(org_id, run_id, id)
    ON DELETE SET NULL (operation_id);

ALTER TABLE telemetry_outbox
    ADD CONSTRAINT telemetry_outbox_run_lease_id_fkey
    FOREIGN KEY (org_id, worker_group_id, run_id, run_lease_id)
    REFERENCES run_leases(org_id, worker_group_id, run_id, id)
    ON DELETE SET NULL (run_lease_id);

ALTER TABLE runs
    ADD CONSTRAINT runs_current_run_lease_id_fkey
    FOREIGN KEY (org_id, worker_group_id, id, current_run_lease_id)
    REFERENCES run_leases(org_id, worker_group_id, run_id, id)
    ON DELETE SET NULL (current_run_lease_id);

CREATE TABLE runtime_checkpoints (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    source_workspace_lease_id UUID NOT NULL,
    workspace_mount_id UUID NOT NULL,
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
    owner_runtime_instance_id UUID,
    owner_runtime_epoch BIGINT CHECK (owner_runtime_epoch IS NULL OR owner_runtime_epoch > 0),
    owner_run_id UUID,
    owner_run_wait_id UUID,
    owner_run_lease_id UUID,
    owner_worker_instance_id UUID,
    source_worker_instance_id UUID,
    substrate_digest TEXT CHECK (substrate_digest IS NULL OR btrim(substrate_digest) <> ''),
    runtime_substrate_artifact_id UUID,
    runtime_vcpus INTEGER CHECK (runtime_vcpus IS NULL OR runtime_vcpus > 0),
    runtime_memory_mib INTEGER CHECK (runtime_memory_mib IS NULL OR runtime_memory_mib > 0),
    runtime_scratch_disk_mib INTEGER CHECK (runtime_scratch_disk_mib IS NULL OR runtime_scratch_disk_mib > 0),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    image_key TEXT,
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT,
    expires_at TIMESTAMPTZ,
    creation_started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    creation_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, worker_group_id, run_id, id),
    UNIQUE (org_id, project_id, environment_id, run_id, id),
    UNIQUE (org_id, worker_group_id, project_id, environment_id, run_id, id),
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, base_workspace_version_id)
        REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (source_worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE SET NULL (source_worker_instance_id),
    FOREIGN KEY (owner_worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE SET NULL (owner_worker_instance_id),
    FOREIGN KEY (org_id, worker_group_id, project_id, environment_id, runtime_substrate_artifact_id)
        REFERENCES runtime_substrate_artifacts(org_id, worker_group_id, project_id, environment_id, id)
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
    worker_group_id TEXT NOT NULL,
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
    PRIMARY KEY (org_id, worker_group_id, run_id, runtime_checkpoint_id, role, ordinal),
    FOREIGN KEY (org_id, worker_group_id, project_id, environment_id, run_id, runtime_checkpoint_id)
        REFERENCES runtime_checkpoints(org_id, worker_group_id, project_id, environment_id, run_id, id)
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
    FOREIGN KEY (org_id, worker_group_id, id, latest_runtime_checkpoint_id)
    REFERENCES runtime_checkpoints(org_id, worker_group_id, run_id, id)
    ON DELETE SET NULL (latest_runtime_checkpoint_id);

ALTER TABLE run_leases
    ADD CONSTRAINT run_leases_restore_runtime_checkpoint_id_fkey
    FOREIGN KEY (org_id, worker_group_id, run_id, restore_runtime_checkpoint_id)
    REFERENCES runtime_checkpoints(org_id, worker_group_id, run_id, id)
    ON DELETE SET NULL (restore_runtime_checkpoint_id);

CREATE TABLE meter_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY,
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    source_type TEXT NOT NULL,
    source_id UUID NOT NULL,
    run_id UUID NOT NULL,
    attempt_number INTEGER CHECK (attempt_number IS NULL OR attempt_number > 0),
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    meter TEXT NOT NULL CHECK (btrim(meter) <> ''),
    quantity NUMERIC NOT NULL CHECK (quantity >= 0),
    unit TEXT NOT NULL CHECK (btrim(unit) <> ''),
    measured_to TIMESTAMPTZ,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL CHECK (btrim(idempotency_key) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX meter_events_idempotency_idx
    ON meter_events (org_id, source_type, source_id, meter, idempotency_key);

CREATE INDEX meter_events_scope_meter_time_idx
    ON meter_events (org_id, project_id, environment_id, meter, occurred_at DESC, id DESC);

CREATE INDEX meter_events_trace_idx
    ON meter_events (trace_id, created_at)
    WHERE trace_id IS NOT NULL;

CREATE INDEX meter_events_run_meter_idx
    ON meter_events (org_id, run_id, meter)
    INCLUDE (quantity);

CREATE TABLE waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^wait_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    kind wait_kind NOT NULL,
    state wait_state NOT NULL DEFAULT 'pending',
    idempotency_key TEXT NOT NULL DEFAULT '',
    correlation_key TEXT NOT NULL DEFAULT '',
    completed_by_run_id UUID,
    completed_after TIMESTAMPTZ,
    stream_id UUID,
    stream_sequence BIGINT CHECK (stream_sequence IS NULL OR stream_sequence >= 0),
    stream_record_id UUID,
    token_id UUID,
    result JSONB,
    error JSONB,
    expires_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    CHECK (
        (
            kind = 'stream'
            AND stream_id IS NOT NULL
            AND token_id IS NULL
            AND completed_after IS NULL
        )
        OR (
            kind = 'token'
            AND token_id IS NOT NULL
            AND stream_id IS NULL
            AND completed_after IS NULL
        )
        OR (
            kind = 'timer'
            AND completed_after IS NOT NULL
            AND stream_id IS NULL
            AND token_id IS NULL
        )
    ),
    FOREIGN KEY (org_id, project_id, environment_id, completed_by_run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (completed_by_run_id),
    FOREIGN KEY (org_id, project_id, environment_id, stream_id)
        REFERENCES streams(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, stream_id, stream_record_id)
        REFERENCES stream_records(org_id, stream_id, id)
        ON DELETE SET NULL (stream_record_id),
    FOREIGN KEY (org_id, project_id, environment_id, token_id)
        REFERENCES tokens(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE run_waits (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    wait_id UUID NOT NULL,
    state run_wait_state NOT NULL DEFAULT 'hot_waiting',
    runtime_checkpoint_due_at TIMESTAMPTZ,
    runtime_checkpoint_started_at TIMESTAMPTZ,
    hot_wait_started_at TIMESTAMPTZ,
    owner_runtime_instance_id UUID,
    owner_runtime_epoch BIGINT CHECK (owner_runtime_epoch IS NULL OR owner_runtime_epoch > 0),
    owner_run_id UUID,
    owner_run_lease_id UUID,
    owner_run_state_version BIGINT CHECK (owner_run_state_version IS NULL OR owner_run_state_version >= 0),
    owner_worker_instance_id UUID,
    runtime_checkpoint_id UUID,
    workspace_version_id UUID,
    active_elapsed_ms_at_park BIGINT CHECK (active_elapsed_ms_at_park IS NULL OR active_elapsed_ms_at_park >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resuming_at TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, worker_group_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, worker_group_id, project_id, environment_id, id),
    UNIQUE (org_id, run_id, id),
    UNIQUE (org_id, worker_group_id, run_id, id),
    UNIQUE (org_id, run_id, wait_id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, wait_id)
        REFERENCES waits(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_group_id, project_id, environment_id, run_id, runtime_checkpoint_id)
        REFERENCES runtime_checkpoints(org_id, worker_group_id, project_id, environment_id, run_id, id)
        ON DELETE SET NULL (runtime_checkpoint_id),
    FOREIGN KEY (org_id, worker_group_id, owner_run_id, owner_run_lease_id)
        REFERENCES run_leases(org_id, worker_group_id, run_id, id)
        ON DELETE SET NULL (owner_run_lease_id),
    FOREIGN KEY (owner_worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE SET NULL,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_version_id)
        REFERENCES workspace_versions(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (workspace_version_id),
    CHECK (
        state <> 'hot_waiting'
        OR (
            hot_wait_started_at IS NOT NULL
            AND owner_runtime_instance_id IS NOT NULL
            AND owner_runtime_epoch IS NOT NULL
            AND owner_run_id IS NOT NULL
            AND owner_run_lease_id IS NOT NULL
            AND owner_run_state_version IS NOT NULL
            AND owner_worker_instance_id IS NOT NULL
        )
    ),
    CHECK (
        state <> 'checkpointed_waiting'
        OR (runtime_checkpoint_id IS NOT NULL AND workspace_version_id IS NOT NULL AND active_elapsed_ms_at_park IS NOT NULL)
    )
);

CREATE TABLE worker_commands (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID,
    run_wait_id UUID,
    run_lease_id UUID,
    worker_instance_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE RESTRICT,
    deployment_sandbox_id UUID,
    runtime_instance_id UUID,
    runtime_epoch BIGINT CHECK (runtime_epoch IS NULL OR runtime_epoch > 0),
    run_state_version BIGINT CHECK (run_state_version IS NULL OR run_state_version >= 0),
    kind worker_command_kind NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    delivered_at TIMESTAMPTZ,
    accepted_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
    delivery_locked_until TIMESTAMPTZ,
    last_delivery_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT worker_commands_target_shape_chk CHECK (
        (
            kind IN ('runtime_resume_wait', 'runtime_checkpoint_wait')
            AND run_id IS NOT NULL
            AND run_wait_id IS NOT NULL
            AND run_lease_id IS NOT NULL
            AND deployment_sandbox_id IS NULL
            AND runtime_instance_id IS NOT NULL
            AND runtime_epoch IS NOT NULL
        )
        OR (
            kind IN ('runtime_prepare', 'runtime_stop')
            AND run_id IS NULL
            AND run_wait_id IS NULL
            AND run_lease_id IS NULL
            AND deployment_sandbox_id IS NULL
            AND run_state_version IS NULL
            AND runtime_instance_id IS NOT NULL
            AND runtime_epoch IS NOT NULL
        )
        OR (
            kind = 'runtime_substrate_prepare'
            AND run_id IS NULL
            AND run_wait_id IS NULL
            AND run_lease_id IS NULL
            AND deployment_sandbox_id IS NOT NULL
            AND runtime_instance_id IS NULL
            AND runtime_epoch IS NULL
            AND run_state_version IS NULL
        )
    ),
    FOREIGN KEY (org_id, project_id, environment_id, run_id)
        REFERENCES runs(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_group_id, run_id, run_wait_id)
        REFERENCES run_waits(org_id, worker_group_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_group_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, worker_group_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, id)
        ON DELETE CASCADE
);

CREATE TABLE runtime_checkpoint_restores (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    runtime_checkpoint_id UUID NOT NULL,
    run_wait_id UUID NOT NULL,
    run_lease_id UUID NOT NULL,
    worker_instance_id UUID NOT NULL,
    status runtime_checkpoint_restore_status NOT NULL DEFAULT 'restoring',
    phases JSONB NOT NULL DEFAULT '[]'::jsonb,
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_id, run_lease_id, runtime_checkpoint_id),
    UNIQUE (org_id, worker_group_id, run_id, run_lease_id, runtime_checkpoint_id),
    FOREIGN KEY (org_id, worker_group_id, project_id, environment_id, run_id, runtime_checkpoint_id)
        REFERENCES runtime_checkpoints(org_id, worker_group_id, project_id, environment_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_group_id, run_id, run_wait_id)
        REFERENCES run_waits(org_id, worker_group_id, run_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, worker_group_id, run_id, run_lease_id)
        REFERENCES run_leases(org_id, worker_group_id, run_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id)
        REFERENCES worker_instances(id)
        ON DELETE RESTRICT,
    CHECK (acknowledged_at IS NULL OR acknowledged_at >= started_at),
    CHECK (finished_at IS NULL OR finished_at >= started_at),
    CHECK (
        (status = 'restoring' AND finished_at IS NULL)
        OR (status <> 'restoring' AND finished_at IS NOT NULL)
    ),
    CHECK (jsonb_typeof(phases) = 'array')
);

CREATE INDEX runtime_checkpoint_restores_run_idx
    ON runtime_checkpoint_restores (org_id, run_id, started_at, id);

CREATE TABLE runtime_instances (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    worker_instance_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE CASCADE,
    runtime_release_id TEXT NOT NULL REFERENCES runtime_releases(runtime_id) ON DELETE RESTRICT,
    deployment_sandbox_id UUID NOT NULL,
    runtime_substrate_artifact_id UUID,
    runtime_epoch BIGINT NOT NULL DEFAULT 1 CHECK (runtime_epoch > 0),
    runtime_key_hash TEXT NOT NULL CHECK (btrim(runtime_key_hash) <> ''),
    runtime_key JSONB NOT NULL DEFAULT '{}'::jsonb,
    sandbox_fingerprint TEXT NOT NULL CHECK (btrim(sandbox_fingerprint) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    image_digest TEXT NOT NULL CHECK (btrim(image_digest) <> ''),
    image_format TEXT NOT NULL CHECK (btrim(image_format) <> ''),
    sandbox_image_artifact_id UUID NOT NULL,
    sandbox_image_artifact_digest TEXT NOT NULL CHECK (btrim(sandbox_image_artifact_digest) <> ''),
    sandbox_image_artifact_format TEXT NOT NULL CHECK (btrim(sandbox_image_artifact_format) <> ''),
    workspace_mount_path TEXT NOT NULL CHECK (btrim(workspace_mount_path) <> ''),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    guestd_abi TEXT NOT NULL CHECK (btrim(guestd_abi) <> ''),
    adapter_abi TEXT NOT NULL CHECK (btrim(adapter_abi) <> ''),
    network_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    reserved_cpu_millis INTEGER NOT NULL CHECK (reserved_cpu_millis > 0),
    reserved_memory_mib INTEGER NOT NULL CHECK (reserved_memory_mib > 0),
    reserved_disk_mib BIGINT NOT NULL CHECK (reserved_disk_mib >= 0),
    reserved_execution_slots INTEGER NOT NULL DEFAULT 1 CHECK (reserved_execution_slots > 0),
    adopting_workspace_mount_id UUID,
    adopted_at TIMESTAMPTZ,
    adoption_expires_at TIMESTAMPTZ,
    workspace_mount_id UUID,
    owner_run_id UUID,
    owner_run_lease_id UUID,
    owner_run_wait_id UUID,
    owner_workspace_id UUID,
    owner_workspace_version_id UUID,
    owner_run_state_version BIGINT CHECK (owner_run_state_version IS NULL OR owner_run_state_version >= 0),
    state runtime_instance_state NOT NULL DEFAULT 'preparing',
    instance_token TEXT NOT NULL DEFAULT '',
    last_heartbeat_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    prepared_at TIMESTAMPTZ,
    bound_at TIMESTAMPTZ,
    running_at TIMESTAMPTZ,
    waiting_at TIMESTAMPTZ,
    checkpointing_at TIMESTAMPTZ,
    stopping_requested_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    last_reclaim_reason TEXT NOT NULL DEFAULT '',
    error JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, worker_group_id, id),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_sandbox_id)
        REFERENCES deployment_sandboxes(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, sandbox_image_artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, sandbox_image_artifact_id, sandbox_image_artifact_digest)
        REFERENCES artifacts(org_id, project_id, environment_id, id, digest)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, worker_group_id, project_id, environment_id, deployment_sandbox_id, runtime_substrate_artifact_id)
        REFERENCES runtime_substrate_artifacts(org_id, worker_group_id, project_id, environment_id, deployment_sandbox_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT runtime_instances_workspace_mount_id_fkey FOREIGN KEY (org_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, id)
        ON DELETE SET NULL (workspace_mount_id),
    CONSTRAINT runtime_instances_adopting_workspace_mount_id_fkey FOREIGN KEY (org_id, adopting_workspace_mount_id)
        REFERENCES workspace_mounts(org_id, id)
        ON DELETE SET NULL (adopting_workspace_mount_id),
    CONSTRAINT runtime_instances_owner_run_id_fkey FOREIGN KEY (org_id, owner_run_id)
        REFERENCES runs(org_id, id)
        ON DELETE SET NULL (owner_run_id),
    CONSTRAINT runtime_instances_owner_run_lease_id_fkey FOREIGN KEY (org_id, worker_group_id, owner_run_id, owner_run_lease_id)
        REFERENCES run_leases(org_id, worker_group_id, run_id, id)
        ON DELETE SET NULL (owner_run_lease_id),
    CONSTRAINT runtime_instances_owner_run_wait_id_fkey FOREIGN KEY (org_id, worker_group_id, owner_run_wait_id)
        REFERENCES run_waits(org_id, worker_group_id, id)
        ON DELETE SET NULL (owner_run_wait_id),
    CONSTRAINT runtime_instances_owner_workspace_id_fkey FOREIGN KEY (org_id, project_id, environment_id, owner_workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (owner_workspace_id),
    CONSTRAINT runtime_instances_owner_workspace_version_id_fkey FOREIGN KEY (org_id, project_id, environment_id, owner_workspace_version_id)
        REFERENCES workspace_versions(org_id, project_id, environment_id, id)
        ON DELETE SET NULL (owner_workspace_version_id)
);

ALTER TABLE workspace_mounts
    ADD CONSTRAINT workspace_mounts_runtime_instance_id_fkey
    FOREIGN KEY (org_id, worker_group_id, runtime_instance_id)
    REFERENCES runtime_instances(org_id, worker_group_id, id)
    ON DELETE SET NULL;

ALTER TABLE run_waits
    ADD CONSTRAINT run_waits_owner_runtime_instance_id_fkey
    FOREIGN KEY (org_id, worker_group_id, owner_runtime_instance_id)
    REFERENCES runtime_instances(org_id, worker_group_id, id)
    ON DELETE SET NULL;

ALTER TABLE worker_commands
    ADD CONSTRAINT worker_commands_runtime_instance_id_fkey
    FOREIGN KEY (org_id, worker_group_id, runtime_instance_id)
    REFERENCES runtime_instances(org_id, worker_group_id, id)
    ON DELETE CASCADE;

ALTER TABLE runtime_checkpoints
    ADD CONSTRAINT runtime_checkpoints_owner_runtime_instance_id_fkey
    FOREIGN KEY (org_id, worker_group_id, owner_runtime_instance_id)
    REFERENCES runtime_instances(org_id, worker_group_id, id)
    ON DELETE SET NULL (owner_runtime_instance_id);

CREATE INDEX runtime_instances_ready_claim_idx
    ON runtime_instances (worker_instance_id, runtime_release_id, deployment_sandbox_id, prepared_at, id)
    WHERE state = 'ready';

CREATE INDEX runtime_instances_coverage_idx
    ON runtime_instances (deployment_sandbox_id, runtime_release_id, state)
    WHERE state IN ('preparing', 'ready');

CREATE INDEX runtime_instances_worker_active_idx
    ON runtime_instances (worker_instance_id, state, expires_at)
    WHERE state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping');

CREATE INDEX runtime_instances_lost_sweep_idx
    ON runtime_instances (state, expires_at)
    WHERE expires_at IS NOT NULL
      AND state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping');

CREATE UNIQUE INDEX runtime_instances_workspace_active_uidx
    ON runtime_instances (workspace_mount_id)
    WHERE workspace_mount_id IS NOT NULL
      AND state IN ('binding', 'running', 'waiting_hot', 'checkpointing', 'stopping');

CREATE UNIQUE INDEX runtime_instances_adopting_workspace_uidx
    ON runtime_instances (adopting_workspace_mount_id)
    WHERE adopting_workspace_mount_id IS NOT NULL
      AND state IN ('preparing', 'ready');

CREATE INDEX runtime_instances_adoption_expiry_idx
    ON runtime_instances (adoption_expires_at)
    WHERE adopting_workspace_mount_id IS NOT NULL
      AND adoption_expires_at IS NOT NULL
      AND state IN ('preparing', 'ready');

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
    ON runs(org_id, worker_group_id, project_id, environment_id, queue_class, queue_name, priority DESC, queue_timestamp, id)
    WHERE status = 'queued' AND current_run_lease_id IS NULL;
CREATE INDEX runs_dispatch_repair_idx
    ON runs(worker_group_id, org_id, status, last_enqueued_at, dispatch_generation)
    WHERE status = 'queued' AND current_run_lease_id IS NULL;
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
CREATE INDEX worker_commands_ready_idx
    ON worker_commands (delivery_locked_until, id)
    WHERE delivered_at IS NULL AND acknowledged_at IS NULL;
CREATE INDEX worker_commands_worker_replay_idx
    ON worker_commands (worker_instance_id, id)
    WHERE acknowledged_at IS NULL;
CREATE UNIQUE INDEX worker_commands_runtime_resume_wait_once_idx
    ON worker_commands (org_id, run_wait_id, kind, run_lease_id, runtime_instance_id, runtime_epoch, run_state_version)
    WHERE kind = 'runtime_resume_wait';
CREATE UNIQUE INDEX worker_commands_runtime_checkpoint_wait_once_idx
    ON worker_commands (org_id, run_wait_id, kind, run_lease_id, runtime_instance_id, runtime_epoch, run_state_version)
    WHERE kind = 'runtime_checkpoint_wait' AND acknowledged_at IS NULL;
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
    ON deployments(org_id, build_worker_group_id, project_id, environment_id, content_hash)
    WHERE status IN ('queued', 'building');
CREATE INDEX deployments_worker_group_status_idx
    ON deployments(build_worker_group_id, status, created_at)
    WHERE status IN ('queued', 'building');
CREATE INDEX artifacts_scope_kind_created_idx
    ON artifacts(org_id, project_id, environment_id, kind, created_at DESC);
CREATE INDEX artifacts_digest_idx
    ON artifacts(digest);
CREATE UNIQUE INDEX artifacts_runtime_substrate_digest_uidx
    ON artifacts(org_id, project_id, environment_id, digest, kind)
    WHERE kind = 'runtime_substrate';
CREATE INDEX deployment_tasks_lookup_idx
    ON deployment_tasks(org_id, project_id, environment_id, task_id);
CREATE INDEX deployment_sandboxes_lookup_idx
    ON deployment_sandboxes(org_id, project_id, environment_id, deployment_id, sandbox_id);
CREATE UNIQUE INDEX telemetry_outbox_run_log_observed_idx
    ON telemetry_outbox(org_id, run_id, run_lease_id, stream_name, observed_seq)
    WHERE stream_kind = 'run_log';
CREATE INDEX telemetry_outbox_run_id_idx ON telemetry_outbox(run_id)
    WHERE run_id IS NOT NULL;
CREATE INDEX telemetry_outbox_deployment_id_idx ON telemetry_outbox(deployment_id)
    WHERE deployment_id IS NOT NULL;
CREATE INDEX telemetry_outbox_run_lease_idx ON telemetry_outbox(org_id, run_id, run_lease_id, id)
    WHERE run_lease_id IS NOT NULL;
CREATE INDEX telemetry_outbox_run_attempt_number_idx ON telemetry_outbox(org_id, run_id, attempt_number, id)
    WHERE attempt_number IS NOT NULL;
CREATE UNIQUE INDEX run_leases_one_active_per_run_idx ON run_leases(run_id)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_leases_attempt_number_idx ON run_leases(org_id, worker_group_id, run_id, attempt_number, leased_at DESC);
CREATE INDEX run_leases_active_lease_idx ON run_leases(org_id, worker_group_id, status, lease_expires_at)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_leases_active_concurrency_idx
    ON run_leases(org_id, worker_group_id, project_id, environment_id, queue_class, queue_name, concurrency_key, lease_expires_at)
    WHERE status IN ('leased', 'running');
CREATE INDEX run_leases_worker_instance_status_idx ON run_leases(org_id, worker_group_id, worker_instance_id, status);
CREATE INDEX run_leases_worker_group_idx ON run_leases(worker_group_id);
CREATE INDEX run_state_snapshots_run_created_idx ON run_state_snapshots(org_id, run_id, created_at DESC);
CREATE INDEX runtime_checkpoints_run_state_idx ON runtime_checkpoints(run_id, state, created_at DESC);
CREATE INDEX runtime_checkpoint_artifacts_role_idx ON runtime_checkpoint_artifacts(org_id, run_id, runtime_checkpoint_id, role, ordinal);
CREATE INDEX tokens_scope_state_idx ON tokens(org_id, project_id, environment_id, state, created_at DESC);
CREATE UNIQUE INDEX tokens_idempotency_idx ON tokens(org_id, project_id, environment_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX tokens_timeout_pending_idx ON tokens(org_id, timeout_at)
    WHERE state = 'pending';
CREATE INDEX tokens_callback_fingerprint_pending_idx ON tokens(callback_key_id, callback_secret_fingerprint)
    WHERE state = 'pending' AND callback_key_id <> '' AND callback_secret_fingerprint <> '';
CREATE INDEX waits_scope_state_idx ON waits(org_id, project_id, environment_id, state, created_at DESC);
CREATE INDEX waits_stream_pending_idx ON waits(org_id, stream_id, stream_sequence, id)
    WHERE kind = 'stream' AND state = 'pending';
CREATE INDEX waits_stream_record_idx ON waits(org_id, stream_id, stream_record_id)
    WHERE stream_record_id IS NOT NULL;
CREATE INDEX waits_token_idx ON waits(org_id, token_id, id)
    WHERE kind = 'token';
CREATE INDEX waits_timer_due_idx ON waits(org_id, completed_after, id)
    WHERE kind = 'timer' AND state = 'pending';
CREATE INDEX waits_expiry_idx ON waits(org_id, expires_at, id)
    WHERE expires_at IS NOT NULL AND state = 'pending';
CREATE INDEX run_waits_run_state_idx ON run_waits(org_id, run_id, state, created_at DESC);
CREATE INDEX tasks_scope_updated_idx ON tasks(org_id, project_id, environment_id, updated_at DESC);
CREATE UNIQUE INDEX task_schedules_internal_dedup_active_idx
    ON task_schedules (org_id, project_id, schedule_type, dedup_key);
CREATE UNIQUE INDEX task_schedules_user_dedup_active_idx
    ON task_schedules (org_id, project_id, user_dedup_key)
    WHERE user_dedup_key IS NOT NULL;
CREATE INDEX task_schedules_scope_created_idx
    ON task_schedules (org_id, project_id, created_at DESC, id DESC);
CREATE INDEX task_schedule_instances_environment_idx
    ON task_schedule_instances (org_id, project_id, environment_id, enabled);
CREATE INDEX task_schedule_instances_index_due_idx
    ON task_schedule_instances (coalesce(retry_after, next_fire_at), id)
    WHERE enabled AND next_fire_at IS NOT NULL;
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
CREATE UNIQUE INDEX workspace_mounts_one_active_idx ON workspace_mounts(workspace_id)
    WHERE state IN ('mounting', 'mounted', 'unmounting');
CREATE INDEX workspace_mounts_heartbeat_idx
    ON workspace_mounts(org_id, state, last_heartbeat_at)
    WHERE state IN ('mounting', 'mounted', 'unmounting');
CREATE UNIQUE INDEX workspace_leases_one_active_writer_workspace_idx ON workspace_leases(workspace_id)
    WHERE lease_kind = 'write' AND state IN ('active', 'releasing');
CREATE UNIQUE INDEX workspace_leases_one_active_writer_workspace_mount_idx ON workspace_leases(workspace_mount_id)
    WHERE lease_kind = 'write' AND state IN ('active', 'releasing');
CREATE INDEX workspace_leases_expiry_idx ON workspace_leases(org_id, expires_at)
    WHERE state IN ('active', 'releasing');
ALTER TABLE workspace_process_stream_chunks
    ADD CONSTRAINT workspace_process_stream_chunks_no_overlap
    EXCLUDE USING gist (
        process_id WITH =,
        stream_name WITH =,
        int8range(offset_start, offset_end, '[)') WITH &&
    );
ALTER TABLE workspace_process_stream_receipts
    ADD CONSTRAINT workspace_process_stream_receipts_no_overlap
    EXCLUDE USING gist (
        process_id WITH =,
        stream_name WITH =,
        int8range(offset_start, offset_end, '[)') WITH &&
    );
CREATE UNIQUE INDEX workspace_operation_idempotencies_workspace_idx
    ON workspace_operation_idempotencies(org_id, project_id, environment_id, operation_kind, workspace_id, idempotency_key)
    WHERE workspace_id IS NOT NULL;
CREATE UNIQUE INDEX workspace_operation_idempotencies_environment_idx
    ON workspace_operation_idempotencies(org_id, project_id, environment_id, operation_kind, idempotency_key)
    WHERE workspace_id IS NULL;
CREATE INDEX workspace_process_operations_claim_idx
    ON workspace_process_operations(workspace_mount_id, state, operation_expires_at, claim_expires_at, priority DESC, requested_at ASC)
    WHERE state IN ('queued', 'claimed');
CREATE INDEX workspace_process_operations_worker_claim_idx
    ON workspace_process_operations(claimed_by_worker_instance_id, state, claim_expires_at)
    WHERE state = 'claimed';
CREATE UNIQUE INDEX workspace_process_operations_active_process_idx
    ON workspace_process_operations(org_id, project_id, environment_id, workspace_mount_id, operation_kind, process_id)
    WHERE state IN ('queued', 'claimed', 'running');
CREATE INDEX deployment_streams_lookup_idx ON deployment_streams(org_id, project_id, environment_id, deployment_id, name, direction);
CREATE UNIQUE INDEX streams_session_name_idx ON streams(org_id, session_id, name, direction);
CREATE INDEX stream_records_sequence_idx ON stream_records(org_id, stream_id, sequence, id);
CREATE INDEX stream_records_correlation_sequence_idx ON stream_records(org_id, stream_id, correlation_id, sequence, id)
    WHERE correlation_id <> '';
CREATE UNIQUE INDEX stream_records_idempotency_idx ON stream_records(org_id, stream_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX public_access_tokens_scope_expiry_idx ON public_access_tokens(org_id, project_id, environment_id, expires_at)
    WHERE state = 'active';
CREATE INDEX public_access_token_scopes_token_idx ON public_access_token_scopes(org_id, project_id, environment_id, token_id, scope_type)
    WHERE token_id IS NOT NULL;
CREATE INDEX public_access_token_scopes_stream_idx ON public_access_token_scopes(org_id, project_id, environment_id, stream_id, scope_type)
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
