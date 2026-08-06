CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE organizations (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    slug TEXT NOT NULL UNIQUE CHECK (btrim(slug) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE region_visibility AS ENUM (
    'public',
    'allowlisted',
    'hidden'
);

CREATE TYPE telemetry_stream_kind AS ENUM (
    'run_log',
    'event',
    'terminal_output',
    'meter_event'
);

CREATE TABLE regions (
    id TEXT PRIMARY KEY CHECK (
        id = btrim(id)
        AND octet_length(id) BETWEEN 1 AND 255
        AND id !~ '[[:cntrl:]]'
        AND id !~ '(^[[:space:]])|([[:space:]]$)'
    ),
    provider TEXT NOT NULL CHECK (btrim(provider) <> ''),
    provider_region TEXT NOT NULL CHECK (btrim(provider_region) <> ''),
    display_name TEXT NOT NULL CHECK (btrim(display_name) <> ''),
    state TEXT NOT NULL DEFAULT 'available'
        CHECK (state IN ('available', 'draining', 'disabled')),
    visibility region_visibility NOT NULL DEFAULT 'public',
    location TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_region)
);

CREATE TABLE users (
    id UUID PRIMARY KEY,
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
    id UUID PRIMARY KEY,
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

CREATE TYPE deletion_job_target_type AS ENUM (
    'project',
    'environment'
);

CREATE TABLE deletion_jobs (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    target_type deletion_job_target_type NOT NULL,
    target_id UUID NOT NULL,
    target_project_id UUID,
    target_slug TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',
    requested_by_principal TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'completed', 'failed')),
    failure TEXT NOT NULL DEFAULT '',
    deleted_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id UUID PRIMARY KEY,
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
    id UUID PRIMARY KEY,
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
    id UUID PRIMARY KEY,
    org_id UUID REFERENCES organizations(id) ON DELETE SET NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE invitations (
    id UUID PRIMARY KEY,
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
    id UUID PRIMARY KEY,
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
    id UUID PRIMARY KEY,
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
    id UUID PRIMARY KEY,
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

CREATE TABLE device_codes (
    id UUID PRIMARY KEY,
    org_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    user_code_hash BYTEA NOT NULL UNIQUE,
    device_code_hash BYTEA NOT NULL UNIQUE,
    decided_by_user_id UUID,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'denied', 'consumed')),
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
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'revoked', 'deleted')),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    current_version_id UUID,
    revocation_generation BIGINT NOT NULL DEFAULT 0 CHECK (revocation_generation >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, name),
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE RESTRICT,
    CHECK (
        (state = 'active' AND current_version_id IS NOT NULL AND revoked_at IS NULL AND deleted_at IS NULL)
        OR
        (state = 'revoked' AND current_version_id IS NULL AND revoked_at IS NOT NULL AND deleted_at IS NULL)
        OR
        (state = 'deleted' AND current_version_id IS NULL AND revoked_at IS NOT NULL AND deleted_at IS NOT NULL)
    )
);

CREATE TABLE secret_versions (
    id UUID PRIMARY KEY,
    secret_id UUID NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) >= 16),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (secret_id, id),
    UNIQUE (secret_id, version),
    FOREIGN KEY (secret_id) REFERENCES secrets(id) ON DELETE RESTRICT
);

ALTER TABLE secrets
    ADD CONSTRAINT secrets_current_version_fk
    FOREIGN KEY (id, current_version_id)
    REFERENCES secret_versions(secret_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

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
    'deployment_program',
    'workspace_image',
    'run_checkpoint_config',
    'run_checkpoint_vm_state',
    'run_checkpoint_memory',
    'run_checkpoint_scratch_disk',
    'workspace_version'
);

CREATE TABLE worker_groups (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'paused', 'draining', 'disabled')),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    allows_run BOOLEAN NOT NULL DEFAULT true,
    allows_build BOOLEAN NOT NULL DEFAULT true,
    required_cpu_millis BIGINT NOT NULL DEFAULT 1 CHECK (required_cpu_millis > 0),
    required_memory_bytes BIGINT NOT NULL DEFAULT 1 CHECK (required_memory_bytes > 0),
    required_guest_ephemeral_disk_bytes BIGINT NOT NULL DEFAULT 1 CHECK (required_guest_ephemeral_disk_bytes > 0),
    required_build_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (required_build_cache_bytes >= 0),
    required_artifact_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (required_artifact_cache_bytes >= 0),
    required_vm_slots INTEGER NOT NULL DEFAULT 1 CHECK (required_vm_slots >= 0),
    required_build_executors INTEGER NOT NULL DEFAULT 1 CHECK (required_build_executors >= 0),
    observation_ttl_seconds INTEGER NOT NULL CHECK (observation_ttl_seconds > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, region_id),
    UNIQUE (region_id, name),
    CHECK (allows_run OR allows_build),
    CHECK (NOT allows_run OR required_vm_slots > 0),
    CHECK (NOT allows_build OR required_build_executors > 0)
);

CREATE INDEX worker_groups_active_placement_idx
    ON worker_groups (region_id, id)
    WHERE state = 'active';

CREATE UNIQUE INDEX worker_groups_one_active_run_per_region_idx
    ON worker_groups (region_id)
    WHERE state IN ('active', 'paused') AND allows_run;

CREATE UNIQUE INDEX worker_groups_one_active_build_per_region_idx
    ON worker_groups (region_id)
    WHERE state IN ('active', 'paused') AND allows_build;

CREATE TABLE runtime_identities (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    runtime_arch TEXT NOT NULL CHECK (runtime_arch = 'x86_64'),
    vm_runtime_contract TEXT NOT NULL CHECK (btrim(vm_runtime_contract) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    initramfs_digest TEXT NOT NULL CHECK (btrim(initramfs_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE worker_instances (
    id UUID PRIMARY KEY,
    resource_id TEXT NOT NULL CHECK (btrim(resource_id) <> ''),
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'registering'
        CHECK (state IN ('registering', 'active', 'draining', 'termination_ready', 'lost')),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    current_epoch BIGINT CHECK (current_epoch IS NULL OR current_epoch > 0),
    current_service_id UUID,
    supervisor_version TEXT NOT NULL DEFAULT '',
    supports_run BOOLEAN NOT NULL DEFAULT false,
    supports_build BOOLEAN NOT NULL DEFAULT false,
    runtime_identity_id TEXT REFERENCES runtime_identities(id) ON DELETE RESTRICT,
    substrate_format TEXT NOT NULL DEFAULT '',
    substrate_contract TEXT NOT NULL DEFAULT '',
    epoch_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (epoch_cpu_millis >= 0),
    epoch_memory_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_memory_bytes >= 0),
    epoch_guest_ephemeral_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_guest_ephemeral_disk_bytes >= 0),
    epoch_build_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_build_cache_bytes >= 0),
    epoch_artifact_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_artifact_cache_bytes >= 0),
    epoch_hugepages_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_hugepages_bytes >= 0),
    epoch_checkpoint_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_checkpoint_bytes >= 0),
    per_vm_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_cpu_millis >= 0),
    per_vm_memory_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_memory_bytes >= 0),
    per_vm_guest_ephemeral_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_guest_ephemeral_disk_bytes >= 0),
    max_vm_slots INTEGER NOT NULL DEFAULT 0 CHECK (max_vm_slots >= 0),
    max_run_consumers INTEGER NOT NULL DEFAULT 0 CHECK (max_run_consumers >= 0),
    max_build_executors INTEGER NOT NULL DEFAULT 0 CHECK (max_build_executors >= 0),
    max_runtime_starts INTEGER NOT NULL DEFAULT 0 CHECK (max_runtime_starts >= 0),
    epoch_started_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    draining_at TIMESTAMPTZ,
    termination_ready_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, worker_group_id),
    CHECK (octet_length(resource_id) <= 512),
    CHECK (
        (current_epoch IS NULL AND current_service_id IS NULL AND epoch_started_at IS NULL)
        OR (current_epoch IS NOT NULL AND current_service_id IS NOT NULL AND epoch_started_at IS NOT NULL)
    ),
    CHECK (state NOT IN ('active', 'draining', 'termination_ready') OR current_epoch IS NOT NULL),
    CONSTRAINT worker_instances_epoch_shape_check CHECK (
        state <> 'active'
        OR (
            btrim(supervisor_version) <> ''
            AND activated_at IS NOT NULL
            AND epoch_cpu_millis > 0
            AND epoch_memory_bytes > 0
            AND per_vm_cpu_millis > 0
            AND per_vm_memory_bytes > 0
            AND per_vm_guest_ephemeral_disk_bytes > 0
            AND (supports_run OR supports_build)
        )
    ),
    CHECK (
        state <> 'active'
        OR NOT supports_run
        OR (
            runtime_identity_id IS NOT NULL
            AND max_vm_slots > 0
            AND max_run_consumers > 0
            AND max_runtime_starts > 0
        )
    ),
    CHECK (state <> 'active' OR NOT supports_build OR max_build_executors > 0),
    CHECK (state <> 'active' OR runtime_identity_id IS NOT NULL),
    CHECK (
        (supports_run
         AND btrim(substrate_format) <> ''
         AND btrim(substrate_contract) <> '')
        OR
        (NOT supports_run
         AND substrate_format = ''
         AND substrate_contract = '')
    ),
    CHECK (state <> 'active' OR activated_at IS NOT NULL),
    CHECK (state NOT IN ('draining', 'termination_ready') OR draining_at IS NOT NULL),
    CHECK ((state = 'termination_ready') = (termination_ready_at IS NOT NULL)),
    CHECK ((state = 'lost') = (lost_at IS NOT NULL))
);

CREATE UNIQUE INDEX worker_instances_one_live_locator_idx
    ON worker_instances (worker_group_id, resource_id)
    WHERE state IN ('registering', 'active', 'draining');

CREATE INDEX worker_instances_active_placement_idx
    ON worker_instances (worker_group_id, id)
    WHERE state = 'active';

CREATE INDEX worker_instances_current_epoch_idx
    ON worker_instances (id, current_epoch)
    WHERE current_epoch IS NOT NULL;

CREATE TABLE worker_enrollment_nonces (
    id UUID PRIMARY KEY,
    nonce_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(nonce_hash) > 0),
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_by_worker_instance_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (
        (consumed_at IS NULL AND consumed_by_worker_instance_id IS NULL)
        OR (consumed_at IS NOT NULL AND consumed_by_worker_instance_id IS NOT NULL)
    ),
    FOREIGN KEY (consumed_by_worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT
);

CREATE INDEX worker_enrollment_nonces_active_idx
    ON worker_enrollment_nonces (expires_at, id)
    WHERE consumed_at IS NULL;

CREATE TABLE worker_instance_credentials (
    id UUID PRIMARY KEY,
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    worker_instance_id UUID NOT NULL,
    key_prefix TEXT NOT NULL UNIQUE CHECK (btrim(key_prefix) <> ''),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    allows_run BOOLEAN NOT NULL,
    allows_build BOOLEAN NOT NULL,
    expires_at TIMESTAMPTZ,
    secret_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(secret_hash) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    UNIQUE (worker_instance_id, id),
    CHECK (allows_run OR allows_build),
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX worker_instance_credentials_one_active_idx
    ON worker_instance_credentials (worker_instance_id)
    WHERE revoked_at IS NULL;

CREATE TABLE worker_observations (
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    cpu_pressure_bps INTEGER NOT NULL CHECK (cpu_pressure_bps BETWEEN 0 AND 10000),
    memory_pressure_bps INTEGER NOT NULL CHECK (memory_pressure_bps BETWEEN 0 AND 10000),
    guest_ephemeral_disk_pressure_bps INTEGER NOT NULL CHECK (guest_ephemeral_disk_pressure_bps BETWEEN 0 AND 10000),
    build_cache_pressure_bps INTEGER NOT NULL CHECK (build_cache_pressure_bps BETWEEN 0 AND 10000),
    artifact_cache_pressure_bps INTEGER NOT NULL CHECK (artifact_cache_pressure_bps BETWEEN 0 AND 10000),
    checkpoint_pressure_bps INTEGER NOT NULL CHECK (checkpoint_pressure_bps BETWEEN 0 AND 10000),
    quarantined_resource_count INTEGER NOT NULL CHECK (quarantined_resource_count >= 0),
    run_queue_depth INTEGER NOT NULL CHECK (run_queue_depth >= 0),
    build_queue_depth INTEGER NOT NULL CHECK (build_queue_depth >= 0),
    runtime_start_queue_depth INTEGER NOT NULL CHECK (runtime_start_queue_depth >= 0),
    run_paused_reason TEXT,
    build_paused_reason TEXT,
    runtime_paused_reason TEXT,
    health_details JSONB NOT NULL DEFAULT '{}'::jsonb,
    observed_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (worker_instance_id, worker_epoch),
    FOREIGN KEY (worker_instance_id) REFERENCES worker_instances(id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(health_details) = 'object')
);

CREATE INDEX worker_observations_freshness_idx
    ON worker_observations (observed_at, worker_instance_id, worker_epoch);

CREATE TABLE artifacts (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    digest TEXT NOT NULL,
    kind artifact_kind NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL CHECK (btrim(media_type) <> ''),
    created_by_worker_instance_id UUID REFERENCES worker_instances(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    CONSTRAINT artifacts_environment_id_id_key UNIQUE (environment_id, id),
    UNIQUE (environment_id, id, kind),
    UNIQUE (environment_id, id, kind, digest, size_bytes),
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

CREATE TYPE wait_kind AS ENUM (
    'token',
    'timer',
    'child',
    'actor_input'
);

CREATE TYPE run_checkpoint_kind AS ENUM (
    'suspend',
    'handoff_resume'
);

CREATE TYPE workspace_version_kind AS ENUM (
    'user',
    'system'
);

CREATE TABLE deployments (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    build_region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    build_node_version TEXT NOT NULL CHECK (
        build_node_version = btrim(build_node_version)
        AND octet_length(build_node_version) BETWEEN 1 AND 64
    ),
    build_runtime_digest BYTEA CHECK (
        build_runtime_digest IS NULL OR octet_length(build_runtime_digest) = 32
    ),
    build_toolchain_digest BYTEA CHECK (
        build_toolchain_digest IS NULL
        OR octet_length(build_toolchain_digest) = 32
    ),
    build_manager_name TEXT NOT NULL CHECK (build_manager_name IN ('npm', 'pnpm', 'bun')),
    build_manager_version TEXT NOT NULL CHECK (
        build_manager_version = btrim(build_manager_version)
        AND octet_length(build_manager_version) BETWEEN 1 AND 64
    ),
    build_manager_integrity TEXT,
    build_manager_digest BYTEA CHECK (
        build_manager_digest IS NULL OR octet_length(build_manager_digest) = 32
    ),
    build_contract TEXT NOT NULL CHECK (build_contract = 'helmr.program-build.v0'),
    image_cache_mode TEXT NOT NULL CHECK (image_cache_mode IN ('prefer', 'bypass')),
    version TEXT NOT NULL CHECK (btrim(version) <> ''),
    content_hash TEXT NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
    deployment_source_artifact_id UUID NOT NULL,
    program_artifact_id UUID,
    program_artifact_kind artifact_kind NOT NULL DEFAULT 'deployment_program'
        CHECK (program_artifact_kind = 'deployment_program'),
    program_index_digest BYTEA CHECK (
        program_index_digest IS NULL OR octet_length(program_index_digest) = 32
    ),
    queue_config JSONB,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'building', 'deployed', 'failed')),
    failure JSONB,
    current_build_lease_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    building_at TIMESTAMPTZ,
    built_at TIMESTAMPTZ,
    deployed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    CONSTRAINT deployments_environment_id_id_key UNIQUE (environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id, build_region_id),
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
    CONSTRAINT deployments_program_artifact_fk
        FOREIGN KEY (environment_id, program_artifact_id, program_artifact_kind)
        REFERENCES artifacts(environment_id, id, kind)
        ON DELETE RESTRICT,
    CONSTRAINT deployments_program_tuple_check CHECK (
        (program_artifact_id IS NULL
         AND program_index_digest IS NULL)
        OR
        (program_artifact_id IS NOT NULL
         AND program_index_digest IS NOT NULL)
    ),
    CONSTRAINT deployments_platform_pins_check CHECK (
        (
            build_runtime_digest IS NULL
            AND build_toolchain_digest IS NULL
            AND build_manager_digest IS NULL
        )
        OR
        (
            build_runtime_digest IS NOT NULL
            AND build_toolchain_digest IS NOT NULL
            AND build_manager_digest IS NOT NULL
        )
    ),
    CONSTRAINT deployments_platform_pin_state_check CHECK (
        (
            status = 'queued'
            AND current_build_lease_id IS NULL
        )
        OR
        (
            status IN ('building', 'deployed')
            AND build_runtime_digest IS NOT NULL
            AND build_toolchain_digest IS NOT NULL
            AND build_manager_digest IS NOT NULL
        )
        OR
        status = 'failed'
    ),
    CONSTRAINT deployments_build_lease_pin_check CHECK (
        current_build_lease_id IS NULL
        OR (
            build_runtime_digest IS NOT NULL
            AND build_toolchain_digest IS NOT NULL
            AND build_manager_digest IS NOT NULL
        )
    ),
    CONSTRAINT deployments_queue_config_check CHECK (
        (status = 'deployed' AND jsonb_typeof(queue_config) = 'object')
        OR
        (status <> 'deployed' AND queue_config IS NULL)
    ),
    CONSTRAINT deployments_failure_check CHECK (
        (status = 'failed'
         AND jsonb_typeof(failure) = 'object'
         AND failure ?& ARRAY['code', 'message', 'details']
         AND failure - ARRAY['code', 'message', 'details'] = '{}'::jsonb
         AND failure->>'code' ~ '^[a-z][a-z0-9_]{0,127}$'
         AND failure->>'code' = btrim(failure->>'code')
         AND failure->>'message' = btrim(failure->>'message')
         AND octet_length(failure->>'message') BETWEEN 1 AND 1024
         AND jsonb_typeof(failure->'details') = 'object')
        OR
        (status <> 'failed'
         AND failure IS NULL)
    )
);

CREATE INDEX deployments_program_artifact_idx
    ON deployments (environment_id, program_artifact_id);

CREATE TABLE deployment_definitions (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('task', 'actor', 'sandbox')),
    declared_id TEXT NOT NULL CHECK (
        declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(declared_id) BETWEEN 1 AND 128
    ),
    manifest_version INTEGER NOT NULL CHECK (manifest_version = 0),
    manifest JSONB NOT NULL CHECK (jsonb_typeof(manifest) = 'object'),
    manifest_digest BYTEA NOT NULL CHECK (octet_length(manifest_digest) = 32),
    artifact_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT deployment_definitions_environment_id_id_key
        UNIQUE (environment_id, id),
    CONSTRAINT deployment_definitions_membership_key
        UNIQUE (deployment_id, kind, declared_id),
    CONSTRAINT deployment_definitions_runtime_pin_key
        UNIQUE (environment_id, id, kind, declared_id),
    CONSTRAINT deployment_definitions_owned_runtime_pin_key
        UNIQUE (environment_id, deployment_id, id, kind, declared_id),
    CONSTRAINT deployment_definitions_deployment_fk
        FOREIGN KEY (environment_id, deployment_id)
        REFERENCES deployments(environment_id, id)
        ON DELETE CASCADE,
    CONSTRAINT deployment_definitions_artifact_fk
        FOREIGN KEY (environment_id, artifact_id)
        REFERENCES artifacts(environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT deployment_definitions_projection_check CHECK (
        (
			kind = 'sandbox'
            AND artifact_id IS NOT NULL
        )
        OR
        (
            kind IN ('task', 'actor')
            AND artifact_id IS NULL
        )
    )
);

CREATE INDEX deployment_definitions_artifact_idx
    ON deployment_definitions (environment_id, artifact_id);

CREATE TABLE deployment_build_leases (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    build_region_id TEXT NOT NULL,
    lease_sequence BIGINT NOT NULL CHECK (lease_sequence BETWEEN 1 AND 3),
    worker_group_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    requested_cpu_millis BIGINT NOT NULL CHECK (requested_cpu_millis = 3000),
    requested_memory_bytes BIGINT NOT NULL CHECK (requested_memory_bytes = 4294967296),
    requested_guest_ephemeral_disk_bytes BIGINT NOT NULL CHECK (requested_guest_ephemeral_disk_bytes = 34359738368),
    requested_build_executors INTEGER NOT NULL DEFAULT 1 CHECK (requested_build_executors = 1),
    build_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    trace_id TEXT,
    span_id TEXT,
    parent_span_id TEXT,
    traceparent TEXT,
    state TEXT NOT NULL DEFAULT 'assigned'
        CHECK (state IN (
            'assigned',
            'starting',
            'running',
            'succeeded',
            'failed',
            'cancelled',
            'lost',
            'rejected',
            'expired'
        )),
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    start_deadline_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    renewed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    terminal_error JSONB,
    terminal_request_fingerprint TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT deployment_build_leases_deployment_id_id_key
        UNIQUE (deployment_id, id),
    UNIQUE (org_id, deployment_id, id),
    UNIQUE (deployment_id, lease_sequence),
    UNIQUE (org_id, project_id, environment_id, deployment_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id, build_region_id)
        REFERENCES deployments(org_id, project_id, environment_id, id, build_region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_group_id, build_region_id)
        REFERENCES worker_groups(id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT,
    CHECK (jsonb_typeof(build_snapshot) = 'object'),
    CHECK (expires_at > assigned_at),
    CHECK (start_deadline_at <= expires_at),
    CHECK (claimed_at IS NULL OR claimed_at >= assigned_at),
    CHECK (started_at IS NULL OR (claimed_at IS NOT NULL AND started_at >= claimed_at)),
    CHECK (renewed_at IS NULL OR (
        renewed_at >= COALESCE(started_at, claimed_at, assigned_at)
        AND (terminal_at IS NULL OR renewed_at <= terminal_at)
    )),
    CHECK (
        (state = 'assigned' AND claimed_at IS NULL AND started_at IS NULL)
        OR (state = 'starting' AND claimed_at IS NOT NULL AND started_at IS NULL)
        OR (state IN ('running', 'succeeded', 'failed') AND claimed_at IS NOT NULL AND started_at IS NOT NULL)
        OR (state IN ('cancelled', 'lost', 'expired'))
        OR (state = 'rejected' AND started_at IS NULL)
    ),
    CHECK (
        (state IN ('assigned', 'starting', 'running') AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (
            state IN ('succeeded', 'failed', 'cancelled', 'lost', 'rejected', 'expired')
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NOT NULL
            AND btrim(terminal_reason_code) <> ''
            AND octet_length(terminal_reason_code) <= 128
        )
    ),
    CHECK (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object'),
    CHECK (terminal_request_fingerprint IS NULL OR (
        btrim(terminal_request_fingerprint) <> '' AND octet_length(terminal_request_fingerprint) <= 128
    )),
    CHECK (
        (state IN ('succeeded', 'failed', 'rejected') AND terminal_request_fingerprint IS NOT NULL)
        OR
        (state IN ('assigned', 'starting', 'running', 'cancelled', 'lost', 'expired')
         AND terminal_request_fingerprint IS NULL)
    ),
    CHECK (
        state <> 'succeeded' OR terminal_error IS NULL
    )
);

CREATE UNIQUE INDEX deployment_build_leases_deployment_active_uidx
    ON deployment_build_leases (deployment_id)
    WHERE state IN ('assigned', 'starting', 'running');

CREATE INDEX deployment_build_leases_worker_replay_idx
    ON deployment_build_leases (worker_instance_id, worker_epoch, state, assigned_at, id)
    WHERE state IN ('assigned', 'starting', 'running');

CREATE INDEX deployment_build_leases_expiry_idx
    ON deployment_build_leases (expires_at, id)
    WHERE state IN ('assigned', 'starting', 'running');

CREATE INDEX deployment_build_leases_capacity_idx
    ON deployment_build_leases (worker_instance_id, worker_epoch, state, requested_build_executors)
    WHERE state IN ('assigned', 'starting', 'running');

CREATE INDEX deployment_build_leases_history_idx
    ON deployment_build_leases (deployment_id, lease_sequence DESC);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_current_build_lease_id_fkey
    FOREIGN KEY (org_id, id, current_build_lease_id)
    REFERENCES deployment_build_leases(org_id, deployment_id, id)
    ON DELETE RESTRICT;

ALTER TABLE environments
    ADD COLUMN current_deployment_id UUID;

CREATE TABLE deployment_promotions (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    previous_deployment_id UUID,
    promoted_by_principal TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (environment_id)
        REFERENCES environments(id)
        ON DELETE CASCADE,
    FOREIGN KEY (environment_id, deployment_id)
        REFERENCES deployments(environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (environment_id, previous_deployment_id)
        REFERENCES deployments(environment_id, id)
        ON DELETE RESTRICT
);

ALTER TABLE environments
    ADD CONSTRAINT environments_current_deployment_fk
    FOREIGN KEY (id, current_deployment_id)
    REFERENCES deployments(environment_id, id)
    ON DELETE RESTRICT;

CREATE TABLE runtime_substrates (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_definition_id UUID NOT NULL,
    substrate_digest TEXT NOT NULL CHECK (btrim(substrate_digest) <> ''),
    substrate_format TEXT NOT NULL CHECK (btrim(substrate_format) <> ''),
    substrate_contract TEXT NOT NULL CHECK (btrim(substrate_contract) <> ''),
    substrate_size_bytes BIGINT NOT NULL CHECK (substrate_size_bytes >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, deployment_definition_id, id),
    CONSTRAINT runtime_substrates_input_key
        UNIQUE (org_id, project_id, environment_id, deployment_definition_id, substrate_format, substrate_contract),
    FOREIGN KEY (environment_id, deployment_definition_id)
        REFERENCES deployment_definitions(environment_id, id)
        ON DELETE RESTRICT
);

CREATE INDEX runtime_substrates_deployment_definition_idx
    ON runtime_substrates (environment_id, deployment_definition_id);

CREATE TABLE idempotency_claims (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    operation TEXT NOT NULL CHECK (btrim(operation) <> '' AND octet_length(operation) <= 128),
    slot_hash BYTEA NOT NULL CHECK (octet_length(slot_hash) = 32),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed', 'failed')),
    receipt JSONB,
    accepted_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE CASCADE,
    CHECK (
        (state = 'pending' AND receipt IS NULL AND completed_at IS NULL)
        OR
        (state IN ('completed', 'failed') AND receipt IS NOT NULL AND completed_at IS NOT NULL)
    ),
    CHECK (receipt IS NULL OR jsonb_typeof(receipt) = 'object'),
    CHECK (
        (operation = 'task.child.invoke' AND expires_at IS NULL)
        OR
        (operation <> 'task.child.invoke' AND expires_at = accepted_at + interval '30 days')
    ),
    CHECK (retired_at IS NULL OR retired_at >= accepted_at)
);

CREATE UNIQUE INDEX idempotency_claims_live_slot_uidx
    ON idempotency_claims (environment_id, operation, slot_hash)
    WHERE retired_at IS NULL;

CREATE INDEX idempotency_claims_live_expiry_idx
    ON idempotency_claims (expires_at, id)
    WHERE retired_at IS NULL AND expires_at IS NOT NULL;

CREATE INDEX idempotency_claims_retired_idx
    ON idempotency_claims (retired_at, id)
    WHERE retired_at IS NOT NULL;

CREATE TABLE registry_credential_resolutions (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    build_lease_id UUID NOT NULL,
    image_operation_id UUID NOT NULL,
    plan_digest BYTEA NOT NULL CHECK (octet_length(plan_digest) = 32),
    registry_authority TEXT NOT NULL CHECK (
        registry_authority = btrim(registry_authority)
        AND octet_length(registry_authority) BETWEEN 1 AND 512
    ),
    username TEXT NOT NULL CHECK (
        username = btrim(username)
        AND octet_length(username) BETWEEN 1 AND 256
    ),
    secret_id UUID NOT NULL,
    secret_version_id UUID NOT NULL,
    revocation_generation BIGINT NOT NULL CHECK (revocation_generation >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT registry_credential_resolutions_binding_key
        UNIQUE (build_lease_id, image_operation_id, registry_authority),
    CONSTRAINT registry_credential_resolutions_build_lease_fk
        FOREIGN KEY (deployment_id, build_lease_id)
        REFERENCES deployment_build_leases(deployment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT registry_credential_resolutions_image_operation_fk
        FOREIGN KEY (environment_id, image_operation_id)
        REFERENCES idempotency_claims(environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT registry_credential_resolutions_secret_fk
        FOREIGN KEY (environment_id, secret_id)
        REFERENCES secrets(environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT registry_credential_resolutions_secret_version_fk
        FOREIGN KEY (secret_id, secret_version_id)
        REFERENCES secret_versions(secret_id, id)
        ON DELETE RESTRICT
);

CREATE INDEX registry_credential_resolutions_image_operation_idx
    ON registry_credential_resolutions (environment_id, image_operation_id);

CREATE INDEX registry_credential_resolutions_secret_idx
    ON registry_credential_resolutions (environment_id, secret_id);

CREATE INDEX registry_credential_resolutions_secret_version_idx
    ON registry_credential_resolutions (secret_id, secret_version_id);

CREATE TABLE schedules (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    target_kind TEXT NOT NULL DEFAULT 'task' CHECK (target_kind = 'task'),
    task_declared_id TEXT NOT NULL CHECK (
        task_declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(task_declared_id) BETWEEN 1 AND 128
    ),
    deployment_definition_id UUID,
    deployment_id UUID,
    cron_pattern TEXT NOT NULL CHECK (octet_length(cron_pattern) BETWEEN 1 AND 1024),
    timezone TEXT NOT NULL CHECK (octet_length(timezone) BETWEEN 1 AND 255),
    cron_semantics_version TEXT NOT NULL DEFAULT 'robfig-cron-v3.0.1/standard-5-field'
        CHECK (cron_semantics_version = 'robfig-cron-v3.0.1/standard-5-field'),
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'errored', 'archived')),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    effective_from TIMESTAMPTZ NOT NULL,
    next_fire_at TIMESTAMPTZ,
    last_fire_at TIMESTAMPTZ,
    claimed_by TEXT CHECK (claimed_by IS NULL OR btrim(claimed_by) <> ''),
    claim_expires_at TIMESTAMPTZ,
    retry_step SMALLINT CHECK (retry_step BETWEEN 1 AND 10),
    retry_after TIMESTAMPTZ,
    last_failure JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, task_declared_id),
    UNIQUE (environment_id, id, generation),
    FOREIGN KEY (environment_id)
        REFERENCES environments(id)
        ON DELETE CASCADE,
    CONSTRAINT schedules_definition_fk
        FOREIGN KEY (
            environment_id,
            deployment_id,
            deployment_definition_id,
            target_kind,
            task_declared_id
        )
        REFERENCES deployment_definitions(
            environment_id,
            deployment_id,
            id,
            kind,
            declared_id
        )
        ON DELETE RESTRICT,
    CHECK (
        (state = 'archived'
         AND deployment_definition_id IS NULL
         AND deployment_id IS NULL
         AND next_fire_at IS NULL
         AND claimed_by IS NULL
         AND claim_expires_at IS NULL
         AND retry_step IS NULL
         AND retry_after IS NULL
         )
        OR
        (state IN ('active', 'errored')
         AND deployment_definition_id IS NOT NULL
         AND deployment_id IS NOT NULL
         AND next_fire_at IS NOT NULL)
    ),
    CHECK ((claimed_by IS NULL) = (claim_expires_at IS NULL)),
    CHECK ((retry_step IS NULL) = (retry_after IS NULL)),
    CHECK (retry_step IS NULL OR state = 'active'),
    CHECK (claimed_by IS NULL OR (state = 'active' AND next_fire_at IS NOT NULL)),
    CHECK (
        (state = 'errored'
         AND last_failure->>'code' IN (
             'task_authority_invalid',
             'sandbox_authority_invalid',
             'architecture_incompatible',
             'generation_invalid',
             'input_invalid'
         )
         AND jsonb_typeof(last_failure) = 'object'
         AND last_failure ?& ARRAY['code', 'message', 'details']
         AND last_failure - ARRAY['code', 'message', 'details'] = '{}'::jsonb
         AND last_failure->>'message' = btrim(last_failure->>'message')
         AND octet_length(last_failure->>'message') BETWEEN 1 AND 1024
         AND jsonb_typeof(last_failure->'details') = 'object')
        OR
        (state <> 'errored'
         AND (
             last_failure IS NULL
             OR
             (last_failure->>'code' IN (
                  'task_authority_invalid',
                  'sandbox_authority_invalid',
                  'architecture_incompatible',
                  'generation_invalid',
                  'input_invalid'
              )
              AND jsonb_typeof(last_failure) = 'object'
              AND last_failure ?& ARRAY['code', 'message', 'details']
              AND last_failure - ARRAY['code', 'message', 'details'] = '{}'::jsonb
              AND last_failure->>'message' = btrim(last_failure->>'message')
              AND octet_length(last_failure->>'message') BETWEEN 1 AND 1024
              AND jsonb_typeof(last_failure->'details') = 'object')
         ))
    )
);

CREATE INDEX schedules_due_idx
    ON schedules (next_fire_at, id)
    WHERE state = 'active' AND next_fire_at IS NOT NULL;

CREATE INDEX schedules_definition_idx
    ON schedules (
        environment_id,
        deployment_id,
        deployment_definition_id,
        target_kind,
        task_declared_id
    )
    WHERE state <> 'archived';

CREATE TABLE schedule_secrets (
    schedule_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    placement_kind TEXT NOT NULL CHECK (placement_kind IN ('env', 'file')),
    placement_target TEXT NOT NULL CHECK (
        btrim(placement_target) <> ''
        AND octet_length(placement_target) <= 4096
    ),
    secret_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schedule_id, placement_kind, placement_target),
    UNIQUE (schedule_id, placement_kind, placement_target, secret_id),
    FOREIGN KEY (environment_id, schedule_id)
        REFERENCES schedules(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, secret_id)
        REFERENCES secrets(environment_id, id)
        ON DELETE RESTRICT
);

CREATE INDEX schedule_secrets_secret_idx
    ON schedule_secrets (secret_id, schedule_id);

CREATE TABLE workspaces (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    sandbox_declared_id TEXT CHECK (
        sandbox_declared_id IS NULL
        OR (
        sandbox_declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(sandbox_declared_id) BETWEEN 1 AND 128
        )
    ),
    deployment_definition_id UUID,
    key TEXT CHECK (
        key IS NULL
        OR (
            octet_length(key) BETWEEN 1 AND 512
            AND key !~ '^[[:space:]]'
            AND key !~ '[[:space:]]$'
        )
    ),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    owner_session_id UUID,
    owner_run_id UUID,
    ownership_generation BIGINT NOT NULL DEFAULT 0 CHECK (ownership_generation >= 0),
    writer_generation BIGINT NOT NULL DEFAULT 0 CHECK (writer_generation >= 0),
    head_version_id UUID,
    state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'deleting', 'recovery_required', 'deleted')),
    desired_state TEXT NOT NULL DEFAULT 'active'
        CHECK (desired_state IN ('active', 'stopped', 'deleted')),
    dirty_state TEXT NOT NULL DEFAULT 'clean'
        CHECK (dirty_state IN ('clean', 'dirty', 'capturing', 'capture_failed', 'dirty_state_lost')),
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, id, deployment_definition_id),
    UNIQUE (environment_id, id, region_id),
    FOREIGN KEY (environment_id)
        REFERENCES environments(id)
        ON DELETE CASCADE,
    CONSTRAINT workspaces_deployment_definition_fk
        FOREIGN KEY (environment_id, deployment_definition_id)
        REFERENCES deployment_definitions(environment_id, id)
        ON DELETE RESTRICT,
    CHECK (num_nonnulls(owner_session_id, owner_run_id) <= 1),
    CHECK (
        (state <> 'deleted'
         AND sandbox_declared_id IS NOT NULL
         AND deployment_definition_id IS NOT NULL
         AND head_version_id IS NOT NULL
         AND deleted_at IS NULL)
        OR
        (state = 'deleted'
         AND sandbox_declared_id IS NULL
         AND deployment_definition_id IS NULL
         AND head_version_id IS NULL
         AND owner_session_id IS NULL
         AND owner_run_id IS NULL
         AND dirty_state = 'clean'
         AND desired_state = 'deleted'
         AND deleted_at IS NOT NULL)
    ),
    CHECK (state <> 'deleting' OR desired_state = 'deleted'),
    CHECK (
        (state = 'recovery_required' AND dirty_state = 'dirty_state_lost' AND desired_state = 'stopped')
        OR
        (state <> 'recovery_required' AND dirty_state <> 'dirty_state_lost')
    )
);

CREATE INDEX workspaces_deployment_definition_idx
    ON workspaces (
        environment_id,
        deployment_definition_id,
        sandbox_declared_id
    );

CREATE TABLE workspace_secrets (
    workspace_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    placement_kind TEXT NOT NULL CHECK (placement_kind IN ('env', 'file')),
    placement_target TEXT NOT NULL CHECK (
        btrim(placement_target) <> ''
        AND octet_length(placement_target) <= 4096
    ),
    secret_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, placement_kind, placement_target),
    UNIQUE (workspace_id, placement_kind, placement_target, secret_id),
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, secret_id)
        REFERENCES secrets(environment_id, id)
        ON DELETE RESTRICT
);

CREATE INDEX workspace_secrets_secret_idx
    ON workspace_secrets (secret_id, workspace_id);

CREATE TABLE sessions (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    actor_declared_id TEXT NOT NULL CHECK (
        actor_declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(actor_declared_id) BETWEEN 1 AND 128
    ),
    deployment_definition_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    key TEXT,
    current_run_id UUID,
    run_generation BIGINT NOT NULL DEFAULT 1 CHECK (run_generation > 0),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    manual_run_cancelled BOOLEAN NOT NULL DEFAULT false,
    failure JSONB,
    failure_run_id UUID,
    next_input_sequence BIGINT NOT NULL DEFAULT 1 CHECK (next_input_sequence BETWEEN 1 AND 9007199254740992),
    committed_input_sequence BIGINT NOT NULL DEFAULT 0 CHECK (committed_input_sequence BETWEEN 0 AND 9007199254740991),
    next_output_sequence BIGINT NOT NULL DEFAULT 1 CHECK (next_output_sequence BETWEEN 1 AND 9007199254740992),
    input_retention_floor BIGINT NOT NULL DEFAULT 1 CHECK (input_retention_floor BETWEEN 1 AND 9007199254740992),
    output_retention_floor BIGINT NOT NULL DEFAULT 1 CHECK (output_retention_floor BETWEEN 1 AND 9007199254740992),
    run_queue_name TEXT NOT NULL CHECK (btrim(run_queue_name) <> '' AND octet_length(run_queue_name) <= 256),
    run_concurrency_key TEXT CHECK (
        run_concurrency_key IS NULL
        OR (
            octet_length(run_concurrency_key) BETWEEN 1 AND 512
            AND ascii(left(run_concurrency_key, 1)) NOT BETWEEN 9 AND 13
            AND ascii(left(run_concurrency_key, 1)) <> 32
            AND ascii(right(run_concurrency_key, 1)) NOT BETWEEN 9 AND 13
            AND ascii(right(run_concurrency_key, 1)) <> 32
        )
    ),
    run_queue_concurrency_limit BIGINT CHECK (
        run_queue_concurrency_limit BETWEEN 1 AND 9007199254740991
    ),
    run_priority INTEGER NOT NULL DEFAULT 0,
    run_queue_ttl_ms BIGINT CHECK (run_queue_ttl_ms BETWEEN 1 AND 9007199254740991),
    run_max_active_duration_ms BIGINT NOT NULL CHECK (run_max_active_duration_ms BETWEEN 1 AND 9007199254740991),
    run_retry_policy JSONB NOT NULL DEFAULT '{"enabled":false}'::jsonb,
    run_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    state TEXT NOT NULL DEFAULT 'open' CHECK (
        state IN ('open', 'closing', 'closed', 'cancelled', 'failed')
    ),
    close_sequence BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (id, workspace_id),
    UNIQUE (id, actor_declared_id, deployment_definition_id),
    UNIQUE (id, actor_declared_id, deployment_definition_id, workspace_id),
    FOREIGN KEY (environment_id)
        REFERENCES environments(id)
        ON DELETE CASCADE,
    CONSTRAINT sessions_deployment_definition_fk
        FOREIGN KEY (environment_id, deployment_definition_id)
        REFERENCES deployment_definitions(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE RESTRICT,
    CHECK (key IS NULL OR (
        octet_length(key) BETWEEN 1 AND 512
        AND key !~ '^[[:space:]]'
        AND key !~ '[[:space:]]$'
    )),
    CHECK (committed_input_sequence < next_input_sequence),
    CHECK (input_retention_floor <= committed_input_sequence + 1),
    CHECK (output_retention_floor <= next_output_sequence),
    CONSTRAINT sessions_run_retry_policy_object
        CHECK (jsonb_typeof(run_retry_policy) = 'object'),
    CONSTRAINT sessions_run_metadata_object
        CHECK (jsonb_typeof(run_metadata) = 'object'),
    CHECK (
        (state = 'failed'
         AND failure->>'code' IN ('no_progress', 'run_failed', 'run_expired', 'platform_failure')
         AND jsonb_typeof(failure) = 'object'
         AND failure ?& ARRAY['code', 'message', 'details']
         AND failure - ARRAY['code', 'message', 'details'] = '{}'::jsonb
         AND failure->>'message' = btrim(failure->>'message')
         AND octet_length(failure->>'message') BETWEEN 1 AND 1024
         AND jsonb_typeof(failure->'details') = 'object')
        OR
        (state = 'cancelled'
         AND failure->>'code' = 'cancelled'
         AND jsonb_typeof(failure) = 'object'
         AND failure ?& ARRAY['code', 'message', 'details']
         AND failure - ARRAY['code', 'message', 'details'] = '{}'::jsonb
         AND failure->>'message' = btrim(failure->>'message')
         AND octet_length(failure->>'message') BETWEEN 1 AND 1024
         AND jsonb_typeof(failure->'details') = 'object')
        OR
        (state NOT IN ('failed', 'cancelled')
         AND failure IS NULL
         AND failure_run_id IS NULL)
    )
);

CREATE UNIQUE INDEX sessions_environment_declared_id_key_uidx
    ON sessions (environment_id, actor_declared_id, key)
    WHERE key IS NOT NULL;

CREATE INDEX sessions_deployment_definition_idx
    ON sessions (
        environment_id,
        deployment_definition_id,
        actor_declared_id
    );

CREATE INDEX sessions_environment_created_id_idx
    ON sessions (environment_id, created_at DESC, id DESC);

CREATE TABLE runs (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    deployment_definition_id UUID NOT NULL,
    entrypoint_kind TEXT NOT NULL CHECK (entrypoint_kind IN ('task', 'actor')),
    entrypoint_declared_id TEXT NOT NULL CHECK (
        entrypoint_declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(entrypoint_declared_id) BETWEEN 1 AND 128
    ),
    session_id UUID,
    cause_kind TEXT NOT NULL CHECK (
        cause_kind IN ('api', 'manual', 'child', 'schedule', 'actor_start', 'continuation')
    ),
    schedule_id UUID,
    schedule_generation BIGINT,
    scheduled_at TIMESTAMPTZ,
    previous_scheduled_at TIMESTAMPTZ,
    schedule_timezone TEXT,
    parent_run_id UUID,
    parent_owns_lifecycle BOOLEAN,
    workspace_id UUID NOT NULL,
    base_workspace_version_id UUID NOT NULL,
    session_input_start_sequence BIGINT,
    session_input_high_watermark BIGINT,
    payload JSONB,
    output JSONB,
    failure JSONB,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN (
            'queued',
            'running',
            'waiting',
            'retry_delayed',
            'cancel_requested',
            'succeeded',
            'failed',
            'cancelled',
            'expired',
            'system_failed'
        )),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    current_attempt_number INTEGER NOT NULL DEFAULT 1 CHECK (current_attempt_number > 0),
    current_run_lease_id UUID,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    queue_name TEXT NOT NULL CHECK (btrim(queue_name) <> ''),
    concurrency_key TEXT CHECK (
        concurrency_key IS NULL
        OR (
            octet_length(concurrency_key) BETWEEN 1 AND 512
            AND ascii(left(concurrency_key, 1)) NOT BETWEEN 9 AND 13
            AND ascii(left(concurrency_key, 1)) <> 32
            AND ascii(right(concurrency_key, 1)) NOT BETWEEN 9 AND 13
            AND ascii(right(concurrency_key, 1)) <> 32
        )
    ),
    queue_concurrency_limit BIGINT CHECK (queue_concurrency_limit BETWEEN 1 AND 9007199254740991),
    priority INTEGER NOT NULL DEFAULT 0,
    queue_origin_at TIMESTAMPTZ NOT NULL,
    queue_score_at TIMESTAMPTZ NOT NULL,
    queued_expires_at TIMESTAMPTZ,
    max_active_duration_ms BIGINT NOT NULL CHECK (max_active_duration_ms BETWEEN 5000 AND 86400000),
    retry_policy JSONB NOT NULL CHECK (jsonb_typeof(retry_policy) = 'object'),
    active_elapsed_ms BIGINT NOT NULL DEFAULT 0 CHECK (active_elapsed_ms >= 0),
    active_started_at TIMESTAMPTZ,
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    root_span_id TEXT NOT NULL CHECK (root_span_id ~ '^[0-9a-f]{16}$' AND root_span_id <> '0000000000000000'),
    claim_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_lease_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    retry_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, id, deployment_id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id, deployment_id),
    UNIQUE (org_id, project_id, environment_id, id, workspace_id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    UNIQUE (session_id, id),
    UNIQUE (session_id, workspace_id, id),
    UNIQUE (id, workspace_id),
    UNIQUE (id, entrypoint_kind, workspace_id),
    UNIQUE (parent_run_id, id, parent_owns_lifecycle),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id)
        REFERENCES deployments(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT runs_deployment_definition_fk
        FOREIGN KEY (
            environment_id,
            deployment_id,
            deployment_definition_id,
            entrypoint_kind,
            entrypoint_declared_id
        )
        REFERENCES deployment_definitions(
            environment_id,
            deployment_id,
            id,
            kind,
            declared_id
        )
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT runs_actor_definition_workspace_fk
        FOREIGN KEY (session_id, entrypoint_declared_id, deployment_definition_id, workspace_id)
        REFERENCES sessions(id, actor_declared_id, deployment_definition_id, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, schedule_id)
        REFERENCES schedules(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, parent_run_id)
        REFERENCES runs(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, claim_id)
        REFERENCES idempotency_claims(environment_id, id)
        ON DELETE RESTRICT,
    CHECK (
        (entrypoint_kind = 'task'
         AND session_id IS NULL
         AND session_input_start_sequence IS NULL
         AND session_input_high_watermark IS NULL)
        OR
        (entrypoint_kind = 'actor'
         AND session_id IS NOT NULL
         AND session_input_start_sequence IS NOT NULL
         AND session_input_high_watermark IS NOT NULL
         AND session_input_high_watermark >= session_input_start_sequence
         AND payload IS NULL)
    ),
    CHECK (
        (cause_kind = 'child'
         AND entrypoint_kind = 'task'
         AND parent_run_id IS NOT NULL
         AND parent_owns_lifecycle IS NOT NULL
         AND (NOT parent_owns_lifecycle OR claim_id IS NOT NULL)
         AND schedule_id IS NULL
         AND schedule_generation IS NULL
         AND scheduled_at IS NULL
         AND previous_scheduled_at IS NULL
         AND schedule_timezone IS NULL)
        OR
        (cause_kind = 'schedule'
         AND entrypoint_kind = 'task'
         AND parent_run_id IS NULL
         AND parent_owns_lifecycle IS NULL
         AND schedule_id IS NOT NULL
         AND schedule_generation IS NOT NULL
         AND scheduled_at IS NOT NULL
         AND schedule_timezone IS NOT NULL)
        OR
        (cause_kind IN ('api', 'manual')
         AND entrypoint_kind = 'task'
         AND parent_run_id IS NULL
         AND parent_owns_lifecycle IS NULL
         AND schedule_id IS NULL
         AND schedule_generation IS NULL
         AND scheduled_at IS NULL
         AND previous_scheduled_at IS NULL
         AND schedule_timezone IS NULL)
        OR
        (cause_kind IN ('actor_start', 'continuation')
         AND entrypoint_kind = 'actor'
         AND parent_run_id IS NULL
         AND parent_owns_lifecycle IS NULL
         AND schedule_id IS NULL
         AND schedule_generation IS NULL
         AND scheduled_at IS NULL
         AND previous_scheduled_at IS NULL
         AND schedule_timezone IS NULL)
    ),
    CHECK (
        (status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')
         AND terminal_at IS NULL
         AND failure IS NULL
         AND output IS NULL
         )
        OR
        (status = 'succeeded'
         AND terminal_at IS NOT NULL
         AND failure IS NULL)
        OR
        (status IN ('failed', 'cancelled', 'expired', 'system_failed')
         AND terminal_at IS NOT NULL
         AND jsonb_typeof(failure) = 'object'
         AND failure ?& ARRAY['code', 'message', 'details']
         AND failure - ARRAY['code', 'message', 'details'] = '{}'::jsonb
         AND failure->>'code' ~ '^[a-z][a-z0-9_]{0,127}$'
         AND failure->>'code' = btrim(failure->>'code')
         AND failure->>'message' = btrim(failure->>'message')
         AND octet_length(failure->>'message') BETWEEN 1 AND 1024
         AND jsonb_typeof(failure->'details') = 'object'
         AND output IS NULL)
    ),
    CHECK ((status = 'retry_delayed') = (retry_at IS NOT NULL))
);

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_owner_actor_fk
    FOREIGN KEY (owner_session_id, id)
    REFERENCES sessions(id, workspace_id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_owner_run_fk
    FOREIGN KEY (owner_run_id, id)
    REFERENCES runs(id, workspace_id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE run_attempts (
    run_id UUID NOT NULL,
    number INTEGER NOT NULL CHECK (number > 0),
    entrypoint_kind TEXT NOT NULL CHECK (entrypoint_kind IN ('task', 'actor')),
    workspace_id UUID NOT NULL,
    entrypoint_entered_at TIMESTAMPTZ,
    session_input_start_sequence BIGINT CHECK (session_input_start_sequence IS NULL OR session_input_start_sequence >= 0),
    base_workspace_version_id UUID NOT NULL,
    terminal_session_input_sequence BIGINT CHECK (terminal_session_input_sequence IS NULL OR terminal_session_input_sequence >= 0),
    terminal_outcome TEXT CHECK (terminal_outcome IN ('succeeded', 'failed', 'cancelled')),
    terminal_reason_code TEXT,
    terminal_error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at TIMESTAMPTZ,
    PRIMARY KEY (run_id, number),
    UNIQUE (run_id, number, workspace_id),
    FOREIGN KEY (run_id, entrypoint_kind, workspace_id)
        REFERENCES runs(id, entrypoint_kind, workspace_id)
        ON DELETE RESTRICT,
    CHECK (
        (entrypoint_kind = 'task'
         AND session_input_start_sequence IS NULL
         AND terminal_session_input_sequence IS NULL)
        OR
        (entrypoint_kind = 'actor'
         AND session_input_start_sequence IS NOT NULL)
    ),
    CHECK (
        (terminal_outcome IS NULL
         AND terminal_at IS NULL
         AND terminal_reason_code IS NULL
         AND terminal_error IS NULL
         AND terminal_session_input_sequence IS NULL)
        OR
        (terminal_outcome IS NOT NULL
         AND terminal_at IS NOT NULL
         AND terminal_reason_code IS NOT NULL
         AND btrim(terminal_reason_code) <> ''
         AND octet_length(terminal_reason_code) <= 128
         AND (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object')
         AND (
             (entrypoint_kind = 'task' AND terminal_session_input_sequence IS NULL)
             OR
             (entrypoint_kind = 'actor'
              AND (terminal_outcome <> 'succeeded'
                   OR terminal_session_input_sequence IS NOT NULL))
         ))
    )
);

CREATE TABLE session_records (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('input', 'output')),
    sequence BIGINT NOT NULL CHECK (sequence BETWEEN 1 AND 9007199254740991),
    data JSONB NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/json' CHECK (
        btrim(content_type) <> '' AND octet_length(content_type) <= 255
    ),
    source_kind TEXT,
    source_run_id UUID,
    producer_run_id UUID,
    producer_attempt_number INTEGER,
    claim_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, direction, sequence),
    UNIQUE (session_id, id, direction),
    FOREIGN KEY (environment_id, session_id)
        REFERENCES sessions(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, source_run_id)
        REFERENCES runs(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (producer_run_id, producer_attempt_number)
        REFERENCES run_attempts(run_id, number)
        ON DELETE RESTRICT,
    FOREIGN KEY (session_id, producer_run_id)
        REFERENCES runs(session_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, claim_id)
        REFERENCES idempotency_claims(environment_id, id)
        ON DELETE RESTRICT,
    CHECK (
        (direction = 'input'
         AND source_kind IN ('external', 'run')
         AND producer_run_id IS NULL
         AND producer_attempt_number IS NULL
         AND content_type = 'application/json'
         AND (
             (source_kind = 'external' AND source_run_id IS NULL)
             OR
             (source_kind = 'run' AND source_run_id IS NOT NULL)
         ))
        OR
        (direction = 'output'
         AND source_kind IS NULL
         AND source_run_id IS NULL
         AND producer_run_id IS NOT NULL
         AND producer_attempt_number IS NOT NULL)
    )
);

CREATE UNIQUE INDEX session_records_claim_uidx
    ON session_records (session_id, direction, claim_id)
    WHERE claim_id IS NOT NULL;

CREATE INDEX session_records_claim_idx
    ON session_records (claim_id)
    WHERE claim_id IS NOT NULL;

CREATE INDEX session_records_input_sequence_idx
    ON session_records (session_id, sequence, id)
    WHERE direction = 'input';

CREATE INDEX session_records_output_sequence_idx
    ON session_records (session_id, sequence, id)
    WHERE direction = 'output';

ALTER TABLE runs
    ADD CONSTRAINT runs_current_attempt_fk
    FOREIGN KEY (id, current_attempt_number)
    REFERENCES run_attempts(run_id, number)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE sessions
    ADD CONSTRAINT sessions_current_run_fk
    FOREIGN KEY (id, workspace_id, current_run_id)
    REFERENCES runs(session_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE sessions
    ADD CONSTRAINT sessions_failure_run_fk
    FOREIGN KEY (id, failure_run_id)
    REFERENCES runs(session_id, id)
    ON DELETE RESTRICT;

CREATE INDEX runs_deployment_definition_idx
    ON runs (
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id
    );

CREATE INDEX runs_claim_idx
    ON runs (claim_id)
    WHERE claim_id IS NOT NULL;

CREATE UNIQUE INDEX runs_actor_live_uidx
    ON runs (session_id)
    WHERE session_id IS NOT NULL
      AND status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested');

CREATE UNIQUE INDEX runs_schedule_instant_uidx
    ON runs (schedule_id, scheduled_at)
    WHERE cause_kind = 'schedule';

CREATE INDEX runs_queue_candidate_idx
    ON runs (environment_id, queue_name, concurrency_key, queue_score_at, id)
    WHERE status = 'queued' AND current_run_lease_id IS NULL;

CREATE INDEX runs_initial_expiry_idx
    ON runs (queued_expires_at, id)
    WHERE status = 'queued'
      AND first_lease_at IS NULL
      AND queued_expires_at IS NOT NULL;

CREATE INDEX runs_retry_ready_idx
    ON runs (retry_at, id)
    WHERE status = 'retry_delayed';

CREATE TABLE workspace_mounts (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    region_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    workspace_id UUID NOT NULL,
    materialized_version_id UUID NOT NULL,
    runtime_instance_id UUID NOT NULL,
    claim_attempt INTEGER NOT NULL DEFAULT 0 CHECK (claim_attempt >= 0),
    guest_channel_token_hash TEXT NOT NULL DEFAULT '',
    guest_channel_token_expires_at TIMESTAMPTZ,
    state TEXT NOT NULL DEFAULT 'mounting'
        CHECK (state IN ('mounting', 'mounted', 'unmounting', 'unmounted', 'lost', 'failed')),
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    dirty_generation BIGINT NOT NULL DEFAULT 0 CHECK (dirty_generation >= 0),
    fencing_generation BIGINT NOT NULL DEFAULT 1 CHECK (fencing_generation > 0),
    finalization_kind TEXT CHECK (finalization_kind IN ('capture', 'discard')),
    finalization_reason_code TEXT,
    finalization_error JSONB,
    staged_version_id UUID,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    mounted_at TIMESTAMPTZ,
    unmounted_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    terminal_error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    UNIQUE (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, id),
    UNIQUE (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, id, fencing_generation),
    FOREIGN KEY (worker_group_id, region_id)
        REFERENCES worker_groups(id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE RESTRICT,
    CHECK (jsonb_typeof(request) = 'object'),
    CHECK (
        (state IN ('mounting', 'mounted', 'unmounting') AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (
            state IN ('unmounted', 'lost', 'failed')
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NOT NULL
            AND btrim(terminal_reason_code) <> ''
            AND octet_length(terminal_reason_code) <= 128
        )
    ),
    CHECK (state <> 'mounted' OR mounted_at IS NOT NULL),
    CHECK (state <> 'unmounted' OR unmounted_at IS NOT NULL),
    CHECK (state <> 'lost' OR lost_at IS NOT NULL),
    CHECK (state <> 'failed' OR failed_at IS NOT NULL),
    CHECK (
        (finalization_kind IS NULL
         AND finalization_reason_code IS NULL
         AND finalization_error IS NULL
         AND staged_version_id IS NULL)
        OR
        (finalization_kind = 'capture'
         AND finalization_reason_code = 'workspace_exec_completed'
         AND finalization_error IS NULL
         AND state IN ('unmounting', 'unmounted', 'failed', 'lost'))
        OR
        (finalization_kind = 'discard'
         AND finalization_reason_code IS NOT NULL
         AND btrim(finalization_reason_code) <> ''
         AND octet_length(finalization_reason_code) <= 128
         AND state IN ('unmounting', 'unmounted', 'failed', 'lost'))
    ),
    CHECK (staged_version_id IS NULL OR finalization_kind = 'capture'),
    CHECK (finalization_error IS NULL OR jsonb_typeof(finalization_error) = 'object'),
    CHECK (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object')
);

CREATE UNIQUE INDEX workspace_mounts_workspace_active_uidx
    ON workspace_mounts (workspace_id)
    WHERE state IN ('mounting', 'mounted', 'unmounting');

CREATE UNIQUE INDEX workspace_mounts_runtime_active_uidx
    ON workspace_mounts (runtime_instance_id)
    WHERE state IN ('mounting', 'mounted', 'unmounting');

CREATE INDEX workspace_mounts_worker_replay_idx
    ON workspace_mounts (worker_instance_id, worker_epoch, state, requested_at, id)
    WHERE state IN ('mounting', 'mounted', 'unmounting');

CREATE INDEX workspace_mounts_sweep_idx
    ON workspace_mounts (state, updated_at, id)
    WHERE state IN ('mounting', 'unmounting');

CREATE TABLE workspace_leases (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    region_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    runtime_instance_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    workspace_mount_id UUID NOT NULL,
    state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'releasing', 'released', 'expired', 'fenced', 'lost')),
    owner_run_lease_id UUID,
    owner_process_id UUID,
    base_version_id UUID NOT NULL,
    ownership_generation BIGINT NOT NULL CHECK (ownership_generation > 0),
    writer_generation BIGINT NOT NULL CHECK (writer_generation > 0),
    mount_fencing_generation BIGINT NOT NULL CHECK (mount_fencing_generation > 0),
    fencing_token_hash TEXT NOT NULL CHECK (btrim(fencing_token_hash) <> ''),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    terminal_error JSONB,
    UNIQUE (org_id, id),
    UNIQUE (workspace_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    UNIQUE (workspace_id, id, ownership_generation, writer_generation),
    UNIQUE (workspace_id, writer_generation),
    UNIQUE (workspace_id, owner_run_lease_id, id),
    UNIQUE (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, id),
    CHECK (num_nonnulls(owner_run_lease_id, owner_process_id) = 1),
    FOREIGN KEY (worker_group_id, region_id)
        REFERENCES worker_groups(id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, id)
        ON DELETE RESTRICT,
    CHECK (
        (state IN ('active', 'releasing') AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (
            state = 'released'
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NULL
            AND terminal_error IS NULL
        )
        OR (
            state IN ('expired', 'fenced', 'lost')
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NOT NULL
            AND btrim(terminal_reason_code) <> ''
            AND octet_length(terminal_reason_code) <= 128
        )
    ),
    CHECK (state <> 'released' OR released_at IS NOT NULL),
    CHECK (state <> 'lost' OR lost_at IS NOT NULL),
    CHECK (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object')
);

CREATE TABLE workspace_processes (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    base_version_id UUID NOT NULL,
    restore_desired_state TEXT NOT NULL
        CHECK (restore_desired_state IN ('active', 'stopped', 'deleted')),
    region_id TEXT,
    worker_group_id TEXT,
    worker_instance_id UUID,
    worker_epoch BIGINT CHECK (worker_epoch IS NULL OR worker_epoch > 0),
    runtime_instance_id UUID,
    workspace_mount_id UUID,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'starting', 'running', 'exit_requested', 'exited', 'failed')),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    request JSONB NOT NULL,
    stdin BYTEA NOT NULL DEFAULT ''::bytea,
    stdout BYTEA,
    stderr BYTEA,
    claim_id UUID NOT NULL,
    exit_code INTEGER,
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    exited_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    error JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (jsonb_typeof(request) = 'object'),
    CHECK (octet_length(stdin) <= 1048576),
    CHECK (stdout IS NULL OR octet_length(stdout) <= 4194304),
    CHECK (stderr IS NULL OR octet_length(stderr) <= 4194304),
    CHECK (
        num_nonnulls(
            region_id,
            worker_group_id,
            worker_instance_id,
            worker_epoch,
            runtime_instance_id,
            workspace_mount_id
        ) IN (0, 6)
    ),
    CHECK (
        (state = 'pending' AND region_id IS NULL)
        OR state = 'failed'
        OR
        (state IN ('starting', 'running', 'exit_requested', 'exited')
         AND region_id IS NOT NULL)
    ),
    CHECK (
        (state IN ('pending', 'starting', 'running', 'exit_requested')
         AND terminal_at IS NULL
         AND terminal_reason_code IS NULL
         AND error IS NULL)
        OR (
            state IN ('exited', 'failed')
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NOT NULL
            AND btrim(terminal_reason_code) <> ''
            AND octet_length(terminal_reason_code) <= 128
        )
    ),
    CHECK (state NOT IN ('exit_requested', 'exited') OR (stdout IS NOT NULL AND stderr IS NOT NULL)),
    CHECK (state <> 'exited' OR exited_at IS NOT NULL),
    CHECK (error IS NULL OR jsonb_typeof(error) = 'object'),
    UNIQUE (org_id, id),
    UNIQUE (workspace_id, id),
    UNIQUE (id, workspace_id, runtime_instance_id),
    UNIQUE (environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, workspace_mount_id)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, claim_id)
        REFERENCES idempotency_claims(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_group_id, region_id)
        REFERENCES worker_groups(id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX workspace_processes_workspace_active_uidx
    ON workspace_processes (workspace_id)
    WHERE state IN ('pending', 'starting', 'running', 'exit_requested');

CREATE UNIQUE INDEX workspace_processes_claim_uidx
    ON workspace_processes (claim_id);

CREATE INDEX workspace_processes_worker_replay_idx
    ON workspace_processes (worker_instance_id, worker_epoch, state, created_at, id)
    WHERE state IN ('starting', 'running', 'exit_requested');

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_owner_process_id_fkey
    FOREIGN KEY (owner_process_id, workspace_id, runtime_instance_id)
    REFERENCES workspace_processes(id, workspace_id, runtime_instance_id)
    ON DELETE RESTRICT;

CREATE UNIQUE INDEX workspace_leases_mount_active_uidx
    ON workspace_leases (workspace_mount_id)
    WHERE state IN ('active', 'releasing');

CREATE UNIQUE INDEX workspace_leases_workspace_active_uidx
    ON workspace_leases (workspace_id)
    WHERE state IN ('active', 'releasing');

CREATE UNIQUE INDEX workspace_leases_owner_process_uidx
    ON workspace_leases (owner_process_id)
    WHERE owner_process_id IS NOT NULL;

CREATE INDEX workspace_leases_expiry_idx
    ON workspace_leases (expires_at, id)
    WHERE state = 'active';

CREATE INDEX workspace_leases_worker_replay_idx
    ON workspace_leases (worker_instance_id, worker_epoch, state, id)
    WHERE state IN ('active', 'releasing');

CREATE TABLE workspace_versions (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    parent_version_id UUID,
    artifact_id UUID,
    artifact_kind artifact_kind,
    kind workspace_version_kind NOT NULL DEFAULT 'user',
    content_digest TEXT NOT NULL CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    entry_count INTEGER NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    state TEXT NOT NULL DEFAULT 'private'
        CHECK (state IN ('private', 'committed', 'discarded')),
    source_workspace_lease_id UUID,
    ownership_generation BIGINT NOT NULL CHECK (ownership_generation >= 0),
    writer_generation BIGINT NOT NULL CHECK (writer_generation >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    discarded_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (workspace_id, id),
    UNIQUE (environment_id, workspace_id, id),
    FOREIGN KEY (environment_id, workspace_id)
        REFERENCES workspaces(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, parent_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (
        workspace_id,
        source_workspace_lease_id,
        ownership_generation,
        writer_generation
    )
        REFERENCES workspace_leases(
            workspace_id,
            id,
            ownership_generation,
            writer_generation
        )
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, artifact_id, artifact_kind)
        REFERENCES artifacts(environment_id, id, kind)
        ON DELETE RESTRICT,
    CHECK (
        (
            parent_version_id IS NULL
            AND artifact_id IS NULL
            AND artifact_kind IS NULL
            AND kind = 'system'
            AND content_digest = 'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96'
            AND size_bytes = 0
            AND entry_count = 0
            AND state = 'committed'
            AND source_workspace_lease_id IS NULL
            AND ownership_generation = 0
            AND writer_generation = 0
            AND published_at IS NOT NULL
            AND discarded_at IS NULL
        )
        OR (
            parent_version_id IS NOT NULL
            AND artifact_id IS NOT NULL
            AND artifact_kind = 'workspace_version'
            AND source_workspace_lease_id IS NOT NULL
        )
    ),
    CHECK (
        (state = 'private' AND published_at IS NULL AND discarded_at IS NULL)
        OR (state = 'committed' AND published_at IS NOT NULL AND discarded_at IS NULL)
        OR (state = 'discarded' AND published_at IS NULL AND discarded_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX workspace_versions_root_uidx
    ON workspace_versions (workspace_id)
    WHERE parent_version_id IS NULL;

ALTER TABLE workspace_mounts
    ADD CONSTRAINT workspace_mounts_materialized_version_id_fkey
    FOREIGN KEY (environment_id, workspace_id, materialized_version_id)
    REFERENCES workspace_versions(environment_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspace_mounts
    ADD CONSTRAINT workspace_mounts_staged_version_id_fkey
    FOREIGN KEY (workspace_id, staged_version_id)
    REFERENCES workspace_versions(workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_base_version_id_fkey
    FOREIGN KEY (environment_id, workspace_id, base_version_id)
    REFERENCES workspace_versions(environment_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_head_version_id_fkey
    FOREIGN KEY (environment_id, id, head_version_id)
    REFERENCES workspace_versions(environment_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspace_processes
    ADD CONSTRAINT workspace_processes_base_version_id_fkey
    FOREIGN KEY (workspace_id, base_version_id)
    REFERENCES workspace_versions(workspace_id, id)
    ON DELETE RESTRICT;

ALTER TABLE runs
    ADD CONSTRAINT runs_base_workspace_version_fk
    FOREIGN KEY (environment_id, workspace_id, base_workspace_version_id)
    REFERENCES workspace_versions(environment_id, workspace_id, id)
    ON DELETE RESTRICT;

ALTER TABLE run_attempts
    ADD CONSTRAINT run_attempts_base_workspace_version_fk
    FOREIGN KEY (workspace_id, base_workspace_version_id)
    REFERENCES workspace_versions(workspace_id, id)
    ON DELETE RESTRICT;

CREATE TABLE secret_resolutions (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    run_id UUID,
    attempt_number INTEGER,
    process_id UUID,
    placement_kind TEXT NOT NULL CHECK (placement_kind IN ('env', 'file')),
    placement_target TEXT NOT NULL CHECK (
        btrim(placement_target) <> ''
        AND octet_length(placement_target) <= 4096
    ),
    secret_id UUID NOT NULL,
    secret_version_id UUID NOT NULL,
    revocation_generation BIGINT NOT NULL CHECK (revocation_generation >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (run_id, attempt_number, workspace_id)
        REFERENCES run_attempts(run_id, number, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, process_id)
        REFERENCES workspace_processes(workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, placement_kind, placement_target, secret_id)
        REFERENCES workspace_secrets(workspace_id, placement_kind, placement_target, secret_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (secret_id, secret_version_id)
        REFERENCES secret_versions(secret_id, id)
        ON DELETE RESTRICT,
    CHECK (
        (run_id IS NOT NULL AND attempt_number IS NOT NULL AND process_id IS NULL)
        OR
        (run_id IS NULL AND attempt_number IS NULL AND process_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX secret_resolutions_attempt_target_uidx
    ON secret_resolutions (run_id, attempt_number, placement_kind, placement_target)
    WHERE run_id IS NOT NULL;

CREATE UNIQUE INDEX secret_resolutions_process_target_uidx
    ON secret_resolutions (process_id, placement_kind, placement_target)
    WHERE process_id IS NOT NULL;

CREATE TABLE tokens (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'completed', 'expired', 'cancelled')),
    expires_at TIMESTAMPTZ NOT NULL,
    callback_secret_fingerprint BYTEA NOT NULL
        CHECK (octet_length(callback_secret_fingerprint) = 32),
    completion_fingerprint BYTEA
        CHECK (completion_fingerprint IS NULL OR octet_length(completion_fingerprint) = 32),
    result JSONB,
    error JSONB,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    expired_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    CHECK (expires_at > created_at),
    CHECK (jsonb_typeof(metadata) = 'object'),
    CHECK (cardinality(tags) <= 10),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE
);

CREATE TABLE public_access_tokens (
    id UUID PRIMARY KEY,
    token_id UUID NOT NULL UNIQUE,
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'revoked', 'expired')),
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
    CHECK (max_uses IS NULL OR used_count <= max_uses),
    CHECK (expires_at > created_at),
    FOREIGN KEY (token_id)
        REFERENCES tokens(id)
        ON DELETE CASCADE
);

CREATE TABLE outbox_messages (
    id UUID PRIMARY KEY,
    lane TEXT NOT NULL CHECK (btrim(lane) <> '' AND octet_length(lane) <= 128),
    topic TEXT NOT NULL CHECK (btrim(topic) <> '' AND octet_length(topic) <= 128),
    partition_key TEXT NOT NULL CHECK (btrim(partition_key) <> '' AND octet_length(partition_key) <= 512),
    payload JSONB NOT NULL CHECK (
        jsonb_typeof(payload) = 'object'
    ),
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'claimed', 'delivered', 'dead_lettered')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_by TEXT CHECK (claimed_by IS NULL OR btrim(claimed_by) <> ''),
    claim_expires_at TIMESTAMPTZ,
    last_error TEXT CHECK (
        last_error IS NULL
        OR (btrim(last_error) <> '' AND octet_length(last_error) <= 2048)
    ),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    CHECK ((claimed_by IS NULL) = (claim_expires_at IS NULL)),
    CHECK (
        (state = 'pending' AND claimed_by IS NULL AND delivered_at IS NULL)
        OR
        (state = 'claimed' AND claimed_by IS NOT NULL AND delivered_at IS NULL)
        OR
        (state = 'delivered' AND claimed_by IS NULL AND delivered_at IS NOT NULL AND last_error IS NULL)
        OR
        (state = 'dead_lettered' AND claimed_by IS NULL AND delivered_at IS NULL AND last_error IS NOT NULL)
    )
);

CREATE INDEX outbox_messages_delivery_idx
    ON outbox_messages (lane, topic, available_at, id)
    WHERE state IN ('pending', 'claimed');

CREATE TABLE telemetry_outbox (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id UUID NOT NULL,
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
    meter_event_id BIGINT,
    attempt_number INTEGER CHECK (attempt_number IS NULL OR attempt_number > 0),
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    parent_span_id TEXT CHECK (parent_span_id IS NULL OR (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000')),
    traceparent TEXT,
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
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'claimed', 'written', 'failed', 'dead_lettered')),
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
            btrim(kind) <> ''
            AND (
                (
                    run_id IS NOT NULL
                    AND deployment_id IS NULL
                    AND source_kind = 'run'
                    AND source_id = run_id
                )
                OR (
                    deployment_id IS NOT NULL
                    AND run_id IS NULL
                    AND source_kind = 'deployment'
                    AND source_id = deployment_id
                )
            )
        )
    ),
    CHECK (
        stream_kind <> 'run_log'
        OR (
            source_kind = 'run'
            AND run_id = source_id
            AND stream_name IN ('stdout', 'stderr', 'structured')
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
            meter_event_id IS NOT NULL
            AND (
                (run_id IS NOT NULL AND deployment_id IS NULL
                 AND source_kind = 'run_lease' AND attempt_number IS NOT NULL)
                OR
                (deployment_id IS NOT NULL AND run_id IS NULL
                 AND source_kind = 'deployment_build_lease' AND attempt_number IS NULL)
            )
            AND idempotency_key IS NOT NULL
            AND btrim(kind) <> ''
            AND payload IS NOT NULL
            AND content IS NULL
            AND observed_seq IS NULL
            AND offset_start IS NULL
            AND offset_end IS NULL
        )
    ),
    CHECK ((stream_kind = 'meter_event') = (meter_event_id IS NOT NULL))
);

CREATE UNIQUE INDEX telemetry_outbox_idempotency_idx
    ON telemetry_outbox (org_id, stream_kind, source_kind, source_id, stream_name, idempotency_key);
CREATE INDEX telemetry_outbox_publish_ready_idx
    ON telemetry_outbox (stream_kind, org_id, source_kind, source_id, stream_name, id)
    WHERE stream_kind IN ('event', 'run_log', 'terminal_output')
      AND published_at IS NULL
      AND state <> 'dead_lettered';
CREATE INDEX telemetry_outbox_ingest_ready_idx
    ON telemetry_outbox (stream_kind, source_kind, source_id, stream_name, id)
    WHERE written_at IS NULL;
CREATE INDEX telemetry_outbox_ingest_claim_idx
    ON telemetry_outbox (stream_kind, id)
    WHERE written_at IS NULL AND state IN ('pending', 'claimed', 'failed');
CREATE INDEX telemetry_outbox_written_gc_idx
    ON telemetry_outbox (id)
    WHERE written_at IS NOT NULL;

CREATE TABLE run_leases (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    region_id TEXT NOT NULL,
    lease_sequence BIGINT NOT NULL CHECK (lease_sequence > 0),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    worker_group_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    runtime_instance_id UUID NOT NULL,
    runtime_identity_id TEXT NOT NULL CHECK (btrim(runtime_identity_id) <> ''),
    requested_cpu_millis BIGINT NOT NULL CHECK (requested_cpu_millis > 0),
    requested_memory_bytes BIGINT NOT NULL CHECK (requested_memory_bytes > 0),
    requested_guest_ephemeral_disk_bytes BIGINT NOT NULL CHECK (requested_guest_ephemeral_disk_bytes >= 0),
    requested_execution_slots INTEGER NOT NULL DEFAULT 1 CHECK (requested_execution_slots > 0),
    trace_id TEXT,
    span_id TEXT,
    parent_span_id TEXT,
    traceparent TEXT,
    state TEXT NOT NULL DEFAULT 'assigned'
        CHECK (state IN (
            'assigned',
            'starting',
            'running',
            'checkpointing',
            'finalizing',
            'checkpointed',
            'completed',
            'failed',
            'cancelled',
            'lost',
            'rejected',
            'expired'
        )),
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    start_deadline_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    renewed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    previous_expires_at TIMESTAMPTZ,
    finalization_operation_id UUID,
    finalization_kind TEXT,
    finalization_started_at TIMESTAMPTZ,
    finalization_request_fingerprint TEXT,
    checkpointed_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    terminal_error JSONB,
    terminal_request_fingerprint TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, run_id, id),
    UNIQUE (run_id, lease_sequence),
    UNIQUE (workspace_id, id),
    UNIQUE (run_id, workspace_id, id),
    UNIQUE (run_id, attempt_number, workspace_id, id),
    UNIQUE (workspace_id, runtime_instance_id, id),
    UNIQUE (org_id, run_id, id, worker_instance_id, worker_epoch, runtime_instance_id),
    UNIQUE (org_id, project_id, environment_id, run_id, id, attempt_number),
    FOREIGN KEY (runtime_identity_id)
        REFERENCES runtime_identities(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, run_id, workspace_id)
        REFERENCES runs(org_id, project_id, environment_id, id, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, attempt_number)
        REFERENCES run_attempts(run_id, number)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, workspace_id, region_id)
        REFERENCES workspaces(environment_id, id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_group_id, region_id)
        REFERENCES worker_groups(id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT,
    CHECK (expires_at > assigned_at),
    CHECK (start_deadline_at <= expires_at),
    CHECK (claimed_at IS NULL OR claimed_at >= assigned_at),
    CHECK (started_at IS NULL OR (claimed_at IS NOT NULL AND started_at >= claimed_at)),
    CHECK (renewed_at IS NULL OR (
        renewed_at >= COALESCE(started_at, claimed_at, assigned_at)
        AND (terminal_at IS NULL OR renewed_at <= terminal_at)
    )),
    CHECK ((previous_expires_at IS NULL) = (renewed_at IS NULL)),
    CHECK (previous_expires_at IS NULL OR (
        start_deadline_at <= previous_expires_at
        AND renewed_at < previous_expires_at
        AND previous_expires_at < expires_at
    )),
    CHECK (
        (state = 'assigned' AND claimed_at IS NULL AND started_at IS NULL)
        OR (state = 'starting' AND claimed_at IS NOT NULL AND started_at IS NULL)
        OR (state IN ('running', 'checkpointing', 'finalizing', 'checkpointed', 'completed', 'failed') AND claimed_at IS NOT NULL AND started_at IS NOT NULL)
        OR (state IN ('cancelled', 'lost', 'expired'))
        OR (state = 'rejected' AND started_at IS NULL)
    ),
    CHECK (num_nonnulls(
        finalization_operation_id,
        finalization_kind,
        finalization_started_at,
        finalization_request_fingerprint
    ) IN (0, 4)),
    CHECK (
        (state IN ('assigned', 'starting', 'running', 'checkpointing', 'checkpointed', 'rejected')
         AND finalization_operation_id IS NULL)
        OR (state = 'finalizing' AND finalization_operation_id IS NOT NULL)
        OR state IN ('completed', 'failed', 'cancelled', 'lost', 'expired')
    ),
    CHECK (finalization_kind IS NULL OR finalization_kind IN ('capture', 'reset')),
    CHECK (finalization_request_fingerprint IS NULL OR (
        btrim(finalization_request_fingerprint) <> ''
        AND octet_length(finalization_request_fingerprint) <= 128
    )),
    CHECK (finalization_started_at IS NULL OR (
        started_at IS NOT NULL
        AND started_at <= finalization_started_at
        AND finalization_started_at < expires_at
        AND (terminal_at IS NULL OR finalization_started_at <= terminal_at)
    )),
    CHECK ((state = 'checkpointed') = (checkpointed_at IS NOT NULL)),
    CHECK (
        (state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing') AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (
            state IN ('checkpointed', 'completed', 'failed', 'cancelled', 'lost', 'rejected', 'expired')
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NOT NULL
            AND btrim(terminal_reason_code) <> ''
            AND octet_length(terminal_reason_code) <= 128
        )
    ),
    CHECK (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object'),
    CHECK (terminal_request_fingerprint IS NULL OR (
        btrim(terminal_request_fingerprint) <> '' AND octet_length(terminal_request_fingerprint) <= 128
    ))
);

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_owner_run_lease_fk
    FOREIGN KEY (
        workspace_id,
        runtime_instance_id,
        owner_run_lease_id
    )
    REFERENCES run_leases(
        workspace_id,
        runtime_instance_id,
        id
    )
    ON DELETE RESTRICT;

CREATE UNIQUE INDEX run_leases_run_active_uidx
    ON run_leases (run_id)
    WHERE state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing');

CREATE UNIQUE INDEX run_leases_runtime_active_uidx
    ON run_leases (runtime_instance_id)
    WHERE runtime_instance_id IS NOT NULL
      AND state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing');

CREATE INDEX run_leases_worker_replay_idx
    ON run_leases (worker_instance_id, worker_epoch, state, expires_at, id)
    WHERE state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing');

CREATE INDEX run_leases_expiry_idx
    ON run_leases (expires_at, id)
    WHERE state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing');

CREATE INDEX run_leases_history_idx
    ON run_leases (run_id, attempt_number, lease_sequence DESC);

ALTER TABLE telemetry_outbox
    ADD CONSTRAINT telemetry_outbox_run_lease_id_fkey
    FOREIGN KEY (org_id, run_id, run_lease_id)
    REFERENCES run_leases(org_id, run_id, id)
    ON DELETE RESTRICT;

ALTER TABLE runs
    ADD CONSTRAINT runs_current_run_lease_id_fkey
    FOREIGN KEY (org_id, id, current_run_lease_id)
    REFERENCES run_leases(org_id, run_id, id)
    ON DELETE RESTRICT;

CREATE TABLE run_checkpoints (
    id UUID PRIMARY KEY,
    kind run_checkpoint_kind NOT NULL,
    run_id UUID NOT NULL,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    run_wait_id UUID NOT NULL,
    source_run_lease_id UUID NOT NULL,
    source_workspace_lease_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    base_workspace_version_id UUID NOT NULL,
    private_workspace_version_id UUID,
    actor_speculative_input_sequence BIGINT CHECK (
        actor_speculative_input_sequence IS NULL
        OR actor_speculative_input_sequence >= 0
    ),
    state TEXT NOT NULL DEFAULT 'creating'
        CHECK (state IN ('creating', 'ready', 'invalid', 'deleted')),
    restore_manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    ready_request_fingerprint TEXT,
    failed_request_fingerprint TEXT,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    invalidation_reason_code TEXT,
    UNIQUE (run_id, id),
    UNIQUE (run_id, attempt_number, id),
    UNIQUE (run_id, attempt_number, workspace_id, id),
    UNIQUE (id, workspace_id),
    UNIQUE (id, run_id, attempt_number, workspace_id),
    FOREIGN KEY (run_id, attempt_number, workspace_id)
        REFERENCES run_attempts(run_id, number, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, attempt_number, workspace_id, source_run_lease_id)
        REFERENCES run_leases(run_id, attempt_number, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, source_run_lease_id, source_workspace_lease_id)
        REFERENCES workspace_leases(workspace_id, owner_run_lease_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, base_workspace_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, private_workspace_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    CHECK (
        jsonb_typeof(restore_manifest) = 'object'
    ),
    CHECK (
        ready_request_fingerprint IS NULL
        OR (
            btrim(ready_request_fingerprint) <> ''
            AND octet_length(ready_request_fingerprint) <= 128
        )
    ),
    CHECK (
        failed_request_fingerprint IS NULL
        OR (
            btrim(failed_request_fingerprint) <> ''
            AND octet_length(failed_request_fingerprint) <= 128
        )
    ),
    CHECK (
        (state = 'creating'
         AND private_workspace_version_id IS NULL
         AND ready_request_fingerprint IS NULL
         AND failed_request_fingerprint IS NULL
         AND ready_at IS NULL
         AND invalidated_at IS NULL
         AND invalidation_reason_code IS NULL)
        OR
        (state = 'ready'
         AND private_workspace_version_id IS NOT NULL
         AND ready_request_fingerprint IS NOT NULL
         AND failed_request_fingerprint IS NULL
         AND ready_at IS NOT NULL
         AND restore_manifest <> '{}'::jsonb
         AND invalidated_at IS NULL
         AND invalidation_reason_code IS NULL)
        OR
        (state IN ('invalid', 'deleted')
         AND invalidated_at IS NOT NULL
         AND invalidation_reason_code IS NOT NULL
         AND btrim(invalidation_reason_code) <> '')
    ),
    CHECK (
        failed_request_fingerprint IS NULL
        OR (state = 'invalid' AND invalidation_reason_code = 'checkpoint_failed')
    )
);

CREATE INDEX run_checkpoints_history_idx
    ON run_checkpoints (run_id, state, created_at DESC, id);

CREATE INDEX run_checkpoints_creation_expiry_idx
    ON run_checkpoints (expires_at, id)
    WHERE state = 'creating' AND expires_at IS NOT NULL;

CREATE INDEX run_checkpoints_wait_idx
    ON run_checkpoints (run_wait_id, state, id);

CREATE UNIQUE INDEX run_checkpoints_creating_uidx
    ON run_checkpoints (run_id, attempt_number, run_wait_id, kind)
    WHERE state = 'creating';

CREATE TYPE run_checkpoint_artifact_role AS ENUM (
    'runtime_config',
    'vm_state',
    'memory',
    'scratch_disk'
);

CREATE TABLE run_checkpoint_artifacts (
    run_checkpoint_id UUID NOT NULL,
    role run_checkpoint_artifact_role NOT NULL,
    ordinal INTEGER NOT NULL DEFAULT 0 CHECK (ordinal >= 0),
    artifact_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_checkpoint_id, role, ordinal),
    FOREIGN KEY (run_checkpoint_id)
        REFERENCES run_checkpoints(id)
        ON DELETE RESTRICT,
    FOREIGN KEY (artifact_id)
        REFERENCES artifacts(id)
        ON DELETE RESTRICT
);

CREATE TABLE meter_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    run_id UUID,
    run_lease_id UUID,
    deployment_id UUID,
    deployment_build_lease_id UUID,
    attempt_number INTEGER CHECK (attempt_number IS NULL OR attempt_number > 0),
    trace_id TEXT CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000')),
    span_id TEXT CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000')),
    meter TEXT NOT NULL CHECK (btrim(meter) <> ''),
    quantity NUMERIC NOT NULL CHECK (quantity >= 0),
    unit TEXT NOT NULL CHECK (btrim(unit) <> ''),
    measured_from TIMESTAMPTZ,
    measured_to TIMESTAMPTZ,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL CHECK (btrim(idempotency_key) <> ''),
    idempotency_fingerprint TEXT NOT NULL CHECK (btrim(idempotency_fingerprint) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    FOREIGN KEY (org_id, project_id, environment_id, run_id, run_lease_id, attempt_number)
        REFERENCES run_leases(org_id, project_id, environment_id, run_id, id, attempt_number)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_id, deployment_build_lease_id)
        REFERENCES deployment_build_leases(org_id, project_id, environment_id, deployment_id, id)
        ON DELETE RESTRICT,
    CHECK (
        (run_id IS NOT NULL AND run_lease_id IS NOT NULL
         AND deployment_id IS NULL AND deployment_build_lease_id IS NULL
         AND attempt_number IS NOT NULL)
        OR
        (run_id IS NULL AND run_lease_id IS NULL
         AND deployment_id IS NOT NULL AND deployment_build_lease_id IS NOT NULL
         AND attempt_number IS NULL)
    ),
    CHECK (
        (measured_from IS NULL AND measured_to IS NULL)
        OR
        (measured_from IS NOT NULL AND measured_to IS NOT NULL AND measured_from < measured_to)
    ),
    CHECK (jsonb_typeof(details) = 'object')
);

ALTER TABLE telemetry_outbox
    ADD CONSTRAINT telemetry_outbox_meter_event_id_fkey
    FOREIGN KEY (meter_event_id)
    REFERENCES meter_events(id)
    ON DELETE RESTRICT;

CREATE UNIQUE INDEX meter_events_run_lease_idempotency_uidx
    ON meter_events (org_id, run_lease_id, meter, idempotency_key)
    WHERE run_lease_id IS NOT NULL;

CREATE UNIQUE INDEX meter_events_deployment_build_lease_idempotency_uidx
    ON meter_events (org_id, deployment_build_lease_id, meter, idempotency_key)
    WHERE deployment_build_lease_id IS NOT NULL;

CREATE INDEX meter_events_scope_meter_time_idx
    ON meter_events (org_id, project_id, environment_id, meter, occurred_at DESC, id DESC);

CREATE INDEX meter_events_trace_idx
    ON meter_events (trace_id, created_at)
    WHERE trace_id IS NOT NULL;

CREATE INDEX meter_events_run_meter_idx
    ON meter_events (org_id, run_id, meter)
    INCLUDE (quantity)
    WHERE run_id IS NOT NULL;

CREATE INDEX meter_events_deployment_meter_idx
    ON meter_events (org_id, deployment_id, meter)
    INCLUDE (quantity)
    WHERE deployment_id IS NOT NULL;

CREATE UNIQUE INDEX telemetry_outbox_meter_event_uidx
    ON telemetry_outbox (meter_event_id)
    WHERE meter_event_id IS NOT NULL;

CREATE TABLE run_waits (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    kind wait_kind NOT NULL,
    condition_state TEXT NOT NULL DEFAULT 'pending'
        CHECK (condition_state IN ('pending', 'completed', 'failed', 'cancelled')),
    due_at TIMESTAMPTZ,
    timeout_at TIMESTAMPTZ,
    idle_timeout_ms BIGINT CHECK (idle_timeout_ms IS NULL OR idle_timeout_ms > 0),
    token_id UUID,
    child_run_id UUID,
    child_parent_owned BOOLEAN,
    child_target_declared_id TEXT,
    child_claim_id UUID,
    child_request JSONB,
    session_id UUID,
    after_input_sequence BIGINT CHECK (after_input_sequence IS NULL OR after_input_sequence >= 0),
    condition_result JSONB,
    condition_error JSONB,
    condition_terminal_at TIMESTAMPTZ,
    condition_reason_code TEXT,
    completed_actor_record_id UUID,
    completed_actor_record_direction TEXT CHECK (
        completed_actor_record_direction IS NULL
        OR completed_actor_record_direction = 'input'
    ),
    suspension_state TEXT NOT NULL DEFAULT 'hot'
        CHECK (suspension_state IN (
            'hot',
            'checkpointing',
            'parked',
            'resume_pending',
            'resuming',
            'released',
            'cancelled',
            'failed'
        )),
    token_registration_run_state_version BIGINT CHECK (token_registration_run_state_version IS NULL OR token_registration_run_state_version >= 0),
    registration_request_fingerprint TEXT CHECK (
        registration_request_fingerprint IS NULL
        OR (registration_request_fingerprint ~ '^sha256:[0-9a-f]{64}$')
    ),
    expected_run_state_version BIGINT NOT NULL CHECK (expected_run_state_version >= 0),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    actor_speculative_input_sequence BIGINT CHECK (
        actor_speculative_input_sequence IS NULL
        OR actor_speculative_input_sequence >= 0
    ),
    current_run_lease_id UUID,
    prior_run_lease_id UUID,
    checkpoint_request_version BIGINT NOT NULL DEFAULT 0 CHECK (checkpoint_request_version >= 0),
    checkpoint_ack_version BIGINT NOT NULL DEFAULT 0 CHECK (checkpoint_ack_version >= 0 AND checkpoint_ack_version <= checkpoint_request_version),
    checkpoint_due_at TIMESTAMPTZ,
    suspend_checkpoint_id UUID,
    handoff_resume_checkpoint_id UUID,
    resume_attach_id UUID NOT NULL,
    resume_request_version BIGINT NOT NULL DEFAULT 0 CHECK (resume_request_version >= 0),
    resume_ack_version BIGINT NOT NULL DEFAULT 0 CHECK (resume_ack_version >= 0 AND resume_ack_version <= resume_request_version),
    base_workspace_version_id UUID,
    base_workspace_content_digest TEXT,
    child_result_version_id UUID,
    resume_workspace_version_id UUID,
    handoff_runtime_instance_id UUID,
    handoff_workspace_mount_id UUID,
    handoff_mount_generation BIGINT CHECK (handoff_mount_generation IS NULL OR handoff_mount_generation > 0),
    ownership_generation BIGINT CHECK (ownership_generation IS NULL OR ownership_generation > 0),
    parent_writer_generation BIGINT CHECK (parent_writer_generation IS NULL OR parent_writer_generation > 0),
    child_writer_generation BIGINT CHECK (child_writer_generation IS NULL OR child_writer_generation > 0),
    resume_writer_generation BIGINT CHECK (resume_writer_generation IS NULL OR resume_writer_generation > 0),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    suspension_terminal_at TIMESTAMPTZ,
    suspension_reason_code TEXT,
    suspension_error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, id),
    UNIQUE (run_id, attempt_number, workspace_id, id),
    FOREIGN KEY (environment_id, run_id)
        REFERENCES runs(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, workspace_id)
        REFERENCES runs(id, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, attempt_number, workspace_id)
        REFERENCES run_attempts(run_id, number, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, attempt_number, workspace_id, current_run_lease_id)
        REFERENCES run_leases(run_id, attempt_number, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, attempt_number, workspace_id, prior_run_lease_id)
        REFERENCES run_leases(run_id, attempt_number, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, token_id)
        REFERENCES tokens(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, child_claim_id)
        REFERENCES idempotency_claims(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (run_id, child_run_id, child_parent_owned)
        REFERENCES runs(parent_run_id, id, parent_owns_lifecycle)
        ON DELETE RESTRICT,
    FOREIGN KEY (session_id, workspace_id, run_id)
        REFERENCES runs(session_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (
        session_id,
        completed_actor_record_id,
        completed_actor_record_direction
    )
        REFERENCES session_records(session_id, id, direction)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, base_workspace_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, child_result_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, resume_workspace_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    CHECK (jsonb_typeof(metadata) = 'object'),
    CHECK (cardinality(tags) <= 32),
    CHECK (condition_error IS NULL OR jsonb_typeof(condition_error) = 'object'),
    CHECK (suspension_error IS NULL OR jsonb_typeof(suspension_error) = 'object'),
    CHECK (
        (completed_actor_record_id IS NULL
         AND completed_actor_record_direction IS NULL
         AND (kind <> 'actor_input' OR condition_state <> 'completed'))
        OR
        (kind = 'actor_input'
         AND condition_state = 'completed'
         AND session_id IS NOT NULL
         AND completed_actor_record_id IS NOT NULL
         AND completed_actor_record_direction = 'input')
    ),
    CHECK (
        (kind = 'timer'
         AND due_at IS NOT NULL
         AND timeout_at IS NULL
         AND token_id IS NULL
         AND token_registration_run_state_version IS NULL
         AND child_run_id IS NULL
         AND child_parent_owned IS NULL
         AND child_target_declared_id IS NULL
         AND child_claim_id IS NULL
         AND child_request IS NULL
         AND session_id IS NULL
         AND after_input_sequence IS NULL)
        OR
        (kind = 'token'
         AND due_at IS NULL
         AND token_id IS NOT NULL
         AND token_registration_run_state_version IS NOT NULL
         AND child_run_id IS NULL
         AND child_parent_owned IS NULL
         AND child_target_declared_id IS NULL
         AND child_claim_id IS NULL
         AND child_request IS NULL
         AND session_id IS NULL
         AND after_input_sequence IS NULL)
        OR
        (kind = 'child'
         AND due_at IS NULL
         AND timeout_at IS NULL
         AND token_id IS NULL
         AND token_registration_run_state_version IS NULL
         AND child_parent_owned IS TRUE
         AND child_target_declared_id IS NOT NULL
         AND btrim(child_target_declared_id) <> ''
         AND child_claim_id IS NOT NULL
         AND child_request IS NOT NULL
         AND session_id IS NULL
         AND after_input_sequence IS NULL)
        OR
        (kind = 'actor_input'
         AND due_at IS NULL
         AND token_id IS NULL
         AND token_registration_run_state_version IS NULL
         AND child_run_id IS NULL
         AND child_parent_owned IS NULL
         AND child_target_declared_id IS NULL
         AND child_claim_id IS NULL
         AND child_request IS NULL
         AND session_id IS NOT NULL
         AND after_input_sequence IS NOT NULL)
    ),
    CHECK (
        (condition_state = 'pending'
         AND condition_result IS NULL
         AND condition_error IS NULL
         AND condition_terminal_at IS NULL
         AND condition_reason_code IS NULL
         AND completed_actor_record_id IS NULL)
        OR
        (condition_state = 'completed'
         AND condition_error IS NULL
         AND condition_terminal_at IS NOT NULL
         AND condition_reason_code IS NULL)
        OR
        (condition_state IN ('failed', 'cancelled')
         AND condition_result IS NULL
         AND condition_terminal_at IS NOT NULL
         AND condition_reason_code IS NOT NULL
         AND btrim(condition_reason_code) <> '')
    ),
    CHECK (
        (suspension_state IN ('hot', 'checkpointing')
         AND current_run_lease_id IS NOT NULL
         AND prior_run_lease_id IS NULL)
        OR
        (suspension_state IN ('parked', 'resume_pending')
         AND current_run_lease_id IS NULL
         AND prior_run_lease_id IS NOT NULL)
        OR
        (suspension_state = 'resuming'
         AND current_run_lease_id IS NOT NULL
         AND prior_run_lease_id IS NOT NULL)
        OR
        suspension_state IN ('released', 'cancelled', 'failed')
    ),
    CHECK (
        (suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming')
         AND suspension_terminal_at IS NULL
         AND suspension_reason_code IS NULL
         AND suspension_error IS NULL)
        OR
        (suspension_state = 'released'
         AND suspension_terminal_at IS NOT NULL
         AND suspension_reason_code IS NULL
         AND suspension_error IS NULL)
        OR
        (suspension_state IN ('cancelled', 'failed')
         AND suspension_terminal_at IS NOT NULL
         AND suspension_reason_code IS NOT NULL
         AND btrim(suspension_reason_code) <> '')
    ),
    CHECK (
        (base_workspace_version_id IS NULL
         AND base_workspace_content_digest IS NULL
         AND child_result_version_id IS NULL
         AND resume_workspace_version_id IS NULL
         AND handoff_runtime_instance_id IS NULL
         AND handoff_workspace_mount_id IS NULL
         AND handoff_mount_generation IS NULL
         AND ownership_generation IS NULL
         AND parent_writer_generation IS NULL
         AND child_writer_generation IS NULL
         AND resume_writer_generation IS NULL
         AND handoff_resume_checkpoint_id IS NULL)
        OR
        (kind = 'child'
         AND child_run_id IS NOT NULL
         AND base_workspace_version_id IS NOT NULL
         AND base_workspace_content_digest IS NOT NULL
         AND handoff_runtime_instance_id IS NOT NULL
         AND handoff_workspace_mount_id IS NOT NULL
         AND handoff_mount_generation IS NOT NULL
         AND ownership_generation IS NOT NULL
         AND parent_writer_generation IS NOT NULL
         AND prior_run_lease_id IS NOT NULL
         AND suspend_checkpoint_id IS NOT NULL
         AND (
             ((condition_state = 'pending'
               OR (condition_state IN ('failed', 'cancelled')
                   AND suspension_state IN ('released', 'cancelled', 'failed')))
              AND child_result_version_id IS NULL
              AND resume_workspace_version_id IS NULL
              AND handoff_resume_checkpoint_id IS NULL
              AND resume_writer_generation IS NULL)
             OR
             (condition_state = 'completed'
              AND child_writer_generation IS NOT NULL
              AND child_result_version_id IS NOT NULL
              AND resume_workspace_version_id IS NOT NULL
              AND resume_workspace_version_id = child_result_version_id
              AND handoff_resume_checkpoint_id IS NOT NULL
              AND (
                  (suspension_state = 'resume_pending'
                   AND resume_writer_generation IS NULL)
                  OR
                  (suspension_state = 'resuming'
                   AND resume_writer_generation IS NOT NULL)
                  OR
                  suspension_state IN ('released', 'cancelled', 'failed')
              ))
             OR
             (condition_state IN ('failed', 'cancelled')
              AND child_result_version_id IS NULL
              AND resume_workspace_version_id IS NOT NULL
              AND resume_workspace_version_id = base_workspace_version_id
              AND handoff_resume_checkpoint_id IS NULL
              AND (
                  (suspension_state = 'resume_pending'
                   AND resume_writer_generation IS NULL)
                  OR
                  (suspension_state = 'resuming'
                   AND resume_writer_generation IS NOT NULL)
                  OR
                  suspension_state IN ('released', 'cancelled', 'failed')
              ))
         ))
    )
);

CREATE UNIQUE INDEX run_waits_active_run_uidx
    ON run_waits (run_id)
    WHERE suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

CREATE INDEX run_waits_checkpoint_replay_idx
    ON run_waits (current_run_lease_id, checkpoint_request_version, checkpoint_ack_version, id)
    WHERE checkpoint_ack_version < checkpoint_request_version;

CREATE INDEX run_waits_resume_replay_idx
    ON run_waits (current_run_lease_id, resume_request_version, resume_ack_version, id)
    WHERE resume_ack_version < resume_request_version;

CREATE INDEX run_waits_checkpoint_due_idx
    ON run_waits (checkpoint_due_at, id)
    WHERE suspension_state = 'hot' AND checkpoint_due_at IS NOT NULL;

CREATE INDEX run_waits_history_idx
    ON run_waits (run_id, created_at, id);

CREATE INDEX run_waits_child_claim_idx
    ON run_waits (child_claim_id)
    WHERE child_claim_id IS NOT NULL;

CREATE INDEX run_waits_condition_timeout_idx
    ON run_waits (timeout_at, id)
    WHERE condition_state = 'pending' AND timeout_at IS NOT NULL;

CREATE INDEX run_waits_timer_due_idx
    ON run_waits (due_at, id)
    WHERE kind = 'timer' AND condition_state = 'pending';

CREATE INDEX run_waits_token_condition_idx
    ON run_waits (token_id, condition_state, id)
    WHERE token_id IS NOT NULL;

CREATE UNIQUE INDEX run_waits_completed_actor_record_active_uidx
    ON run_waits (completed_actor_record_id)
    WHERE completed_actor_record_id IS NOT NULL
      AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

CREATE INDEX run_waits_handoff_runtime_active_idx
    ON run_waits (handoff_runtime_instance_id)
    WHERE handoff_runtime_instance_id IS NOT NULL
      AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

CREATE INDEX run_waits_handoff_mount_active_idx
    ON run_waits (handoff_workspace_mount_id)
    WHERE handoff_workspace_mount_id IS NOT NULL
      AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

CREATE UNIQUE INDEX run_waits_handoff_child_active_uidx
    ON run_waits (child_run_id)
    WHERE handoff_runtime_instance_id IS NOT NULL
      AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

ALTER TABLE run_checkpoints
    ADD CONSTRAINT run_checkpoints_run_wait_id_fkey
    FOREIGN KEY (run_id, attempt_number, workspace_id, run_wait_id)
    REFERENCES run_waits(run_id, attempt_number, workspace_id, id)
    ON DELETE RESTRICT;

ALTER TABLE run_waits
    ADD CONSTRAINT run_waits_suspend_checkpoint_fk
    FOREIGN KEY (run_id, attempt_number, workspace_id, suspend_checkpoint_id)
    REFERENCES run_checkpoints(run_id, attempt_number, workspace_id, id)
    ON DELETE RESTRICT,
    ADD CONSTRAINT run_waits_handoff_resume_checkpoint_fk
    FOREIGN KEY (run_id, attempt_number, workspace_id, handoff_resume_checkpoint_id)
    REFERENCES run_checkpoints(run_id, attempt_number, workspace_id, id)
    ON DELETE RESTRICT;

CREATE TABLE runtime_instances (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    worker_group_id TEXT NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    region_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    runtime_identity_id TEXT NOT NULL REFERENCES runtime_identities(id) ON DELETE RESTRICT,
    deployment_definition_id UUID NOT NULL,
    runtime_substrate_id UUID,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    reserved_cpu_millis BIGINT NOT NULL CHECK (reserved_cpu_millis > 0),
    reserved_memory_bytes BIGINT NOT NULL CHECK (reserved_memory_bytes > 0),
    reserved_guest_ephemeral_disk_bytes BIGINT NOT NULL CHECK (reserved_guest_ephemeral_disk_bytes >= 0),
    reserved_execution_slots INTEGER NOT NULL CHECK (reserved_execution_slots > 0),
    workspace_id UUID NOT NULL,
    program_deployment_id UUID,
    restore_checkpoint_id UUID,
    reserved_run_id UUID,
    reserved_attempt_number INTEGER CHECK (reserved_attempt_number IS NULL OR reserved_attempt_number > 0),
    reserved_process_id UUID,
    reserved_workspace_version_id UUID,
    reservation_expires_at TIMESTAMPTZ,
    desired_state TEXT NOT NULL DEFAULT 'ready'
        CHECK (desired_state IN ('ready', 'closed')),
    desired_version BIGINT NOT NULL DEFAULT 1 CHECK (desired_version > 0),
    desired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    desired_reason TEXT NOT NULL CHECK (btrim(desired_reason) <> ''),
    observed_state TEXT NOT NULL DEFAULT 'allocated'
        CHECK (observed_state IN ('allocated', 'preparing', 'ready', 'closing', 'closed', 'failed', 'lost')),
    observed_version BIGINT NOT NULL DEFAULT 0 CHECK (observed_version >= 0),
    observed_desired_version BIGINT NOT NULL DEFAULT 0 CHECK (observed_desired_version >= 0 AND observed_desired_version <= desired_version),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    allocated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    preparing_at TIMESTAMPTZ,
    ready_at TIMESTAMPTZ,
    closing_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    reclaimed_at TIMESTAMPTZ,
    reclaim_evidence JSONB,
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    terminal_error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (worker_group_id, worker_instance_id, worker_epoch, id),
    UNIQUE (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, id),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (worker_group_id, region_id)
        REFERENCES worker_groups(id, region_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, deployment_definition_id)
        REFERENCES deployment_definitions(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, workspace_id, deployment_definition_id)
        REFERENCES workspaces(environment_id, id, deployment_definition_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, program_deployment_id)
        REFERENCES deployments(environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT runtime_instances_restore_checkpoint_workspace_fkey
    FOREIGN KEY (restore_checkpoint_id, workspace_id)
        REFERENCES run_checkpoints(id, workspace_id)
        ON DELETE RESTRICT,
    CONSTRAINT runtime_instances_restore_checkpoint_execution_fkey
    FOREIGN KEY (restore_checkpoint_id, reserved_run_id, reserved_attempt_number, workspace_id)
        REFERENCES run_checkpoints(id, run_id, attempt_number, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (reserved_run_id, reserved_attempt_number, workspace_id)
        REFERENCES run_attempts(run_id, number, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, reserved_run_id, program_deployment_id)
        REFERENCES runs(environment_id, id, deployment_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, reserved_workspace_version_id)
        REFERENCES workspace_versions(workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (reserved_process_id, workspace_id)
        REFERENCES workspace_processes(id, workspace_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, deployment_definition_id, runtime_substrate_id)
        REFERENCES runtime_substrates(org_id, project_id, environment_id, deployment_definition_id, id)
        ON DELETE RESTRICT,
    CHECK (
        (reserved_run_id IS NULL
         AND reserved_attempt_number IS NULL
         AND reserved_process_id IS NULL
         AND reserved_workspace_version_id IS NULL
         AND reservation_expires_at IS NULL)
        OR
        (reserved_run_id IS NOT NULL
         AND reserved_attempt_number IS NOT NULL
         AND reserved_process_id IS NULL
         AND reserved_workspace_version_id IS NOT NULL
         AND reservation_expires_at IS NOT NULL
         AND program_deployment_id IS NOT NULL)
        OR
        (reserved_run_id IS NULL
         AND reserved_attempt_number IS NULL
         AND reserved_process_id IS NOT NULL
         AND reserved_workspace_version_id IS NOT NULL
         AND reservation_expires_at IS NOT NULL)
    ),
    CHECK (reserved_workspace_version_id IS NULL OR observed_state IN ('allocated', 'preparing', 'ready')),
    CHECK (restore_checkpoint_id IS NULL OR reserved_process_id IS NULL),
    CHECK (desired_state <> 'closed' OR desired_version > 1),
    CHECK (observed_desired_version < desired_version OR desired_state <> 'closed' OR observed_state IN ('closing', 'closed', 'failed', 'lost')),
    CHECK (preparing_at IS NULL OR preparing_at >= allocated_at),
    CHECK (ready_at IS NULL OR (ready_at >= allocated_at AND (preparing_at IS NULL OR ready_at >= preparing_at))),
    CHECK (closing_at IS NULL OR (closing_at >= allocated_at AND (ready_at IS NULL OR closing_at >= ready_at))),
    CHECK (closed_at IS NULL OR (closing_at IS NOT NULL AND closed_at >= closing_at)),
    CHECK (failed_at IS NULL OR failed_at >= GREATEST(allocated_at, COALESCE(preparing_at, allocated_at), COALESCE(ready_at, allocated_at), COALESCE(closing_at, allocated_at))),
    CHECK (lost_at IS NULL OR lost_at >= GREATEST(allocated_at, COALESCE(preparing_at, allocated_at), COALESCE(ready_at, allocated_at), COALESCE(closing_at, allocated_at))),
    CHECK (terminal_at IS NULL OR terminal_at >= GREATEST(allocated_at, COALESCE(preparing_at, allocated_at), COALESCE(ready_at, allocated_at), COALESCE(closing_at, allocated_at))),
    CHECK (reclaimed_at IS NULL OR (observed_state IN ('closed', 'failed', 'lost') AND terminal_at IS NOT NULL AND reclaimed_at >= terminal_at)),
    CHECK ((reclaimed_at IS NULL) = (reclaim_evidence IS NULL)),
    CHECK (reclaim_evidence IS NULL OR jsonb_typeof(reclaim_evidence) = 'object'),
    CHECK (
        (observed_state = 'allocated' AND preparing_at IS NULL AND ready_at IS NULL AND closing_at IS NULL AND closed_at IS NULL AND failed_at IS NULL AND lost_at IS NULL AND reclaimed_at IS NULL AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (observed_state = 'preparing' AND preparing_at IS NOT NULL AND ready_at IS NULL AND closing_at IS NULL AND closed_at IS NULL AND failed_at IS NULL AND lost_at IS NULL AND reclaimed_at IS NULL AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (observed_state = 'ready' AND preparing_at IS NOT NULL AND ready_at IS NOT NULL AND closing_at IS NULL AND closed_at IS NULL AND failed_at IS NULL AND lost_at IS NULL AND reclaimed_at IS NULL AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (observed_state = 'closing' AND closing_at IS NOT NULL AND closed_at IS NULL AND failed_at IS NULL AND lost_at IS NULL AND reclaimed_at IS NULL AND terminal_at IS NULL AND terminal_reason_code IS NULL AND terminal_error IS NULL)
        OR (observed_state = 'closed' AND closing_at IS NOT NULL AND closed_at IS NOT NULL AND failed_at IS NULL AND lost_at IS NULL AND reclaimed_at IS NOT NULL AND terminal_at IS NOT NULL AND terminal_reason_code IS NOT NULL AND terminal_error IS NULL)
        OR (observed_state = 'failed' AND failed_at IS NOT NULL AND closed_at IS NULL AND lost_at IS NULL AND terminal_at IS NOT NULL AND terminal_reason_code IS NOT NULL)
        OR (observed_state = 'lost' AND lost_at IS NOT NULL AND closed_at IS NULL AND failed_at IS NULL AND terminal_at IS NOT NULL AND terminal_reason_code IS NOT NULL)
    ),
    CHECK (terminal_reason_code IS NULL OR (btrim(terminal_reason_code) <> '' AND octet_length(terminal_reason_code) <= 128)),
    CHECK (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object')
);

ALTER TABLE run_leases
    ADD CONSTRAINT run_leases_runtime_instance_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id)
    REFERENCES runtime_instances(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, id)
    ON DELETE RESTRICT;

ALTER TABLE workspace_mounts
    ADD CONSTRAINT workspace_mounts_runtime_instance_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id)
    REFERENCES runtime_instances(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, id)
    ON DELETE RESTRICT;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_runtime_instance_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id)
    REFERENCES runtime_instances(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, id)
    ON DELETE RESTRICT;

ALTER TABLE workspace_processes
    ADD CONSTRAINT workspace_processes_runtime_instance_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id)
    REFERENCES runtime_instances(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, id)
    ON DELETE RESTRICT;

CREATE INDEX runtime_instances_deployment_definition_idx
    ON runtime_instances (environment_id, deployment_definition_id);

CREATE INDEX runtime_instances_worker_active_idx
    ON runtime_instances (worker_instance_id, worker_epoch, observed_state, id)
    WHERE observed_state IN ('allocated', 'preparing', 'ready', 'closing')
       OR (observed_state IN ('failed', 'lost') AND reclaimed_at IS NULL);

CREATE INDEX runtime_instances_reclaim_idx
    ON runtime_instances (observed_state, updated_at, id)
    WHERE observed_state IN ('closed', 'failed', 'lost') AND reclaimed_at IS NULL;

CREATE UNIQUE INDEX runtime_instances_workspace_active_uidx
    ON runtime_instances (workspace_id)
    WHERE reclaimed_at IS NULL;

CREATE UNIQUE INDEX runtime_instances_reserved_run_uidx
    ON runtime_instances (reserved_run_id)
    WHERE reserved_run_id IS NOT NULL;

CREATE UNIQUE INDEX runtime_instances_reserved_process_uidx
    ON runtime_instances (reserved_process_id)
    WHERE reserved_process_id IS NOT NULL;

CREATE INDEX runtime_instances_restore_checkpoint_idx
    ON runtime_instances (restore_checkpoint_id)
    WHERE restore_checkpoint_id IS NOT NULL;

CREATE INDEX runtime_instances_desired_replay_idx
    ON runtime_instances (worker_instance_id, worker_epoch, desired_version, id)
    WHERE observed_desired_version < desired_version;

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
CREATE INDEX workspaces_region_scope_idx
    ON workspaces(region_id, environment_id, id);
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
CREATE INDEX environments_current_deployment_idx
    ON environments(org_id, project_id, current_deployment_id)
    WHERE current_deployment_id IS NOT NULL;
CREATE INDEX deployment_promotions_deployment_idx
    ON deployment_promotions(environment_id, deployment_id);
CREATE INDEX deployment_promotions_environment_created_idx
    ON deployment_promotions(environment_id, created_at DESC);
CREATE INDEX deployments_build_region_status_idx
    ON deployments(build_region_id, status, created_at)
    WHERE status IN ('queued', 'building');
CREATE INDEX artifacts_scope_kind_created_idx
    ON artifacts(org_id, project_id, environment_id, kind, created_at DESC);
CREATE INDEX artifacts_digest_idx
    ON artifacts(digest);
CREATE UNIQUE INDEX telemetry_outbox_run_log_observed_idx
    ON telemetry_outbox(org_id, run_id, attempt_number, stream_name, observed_seq)
    WHERE stream_kind = 'run_log';
CREATE INDEX telemetry_outbox_run_id_idx ON telemetry_outbox(run_id)
    WHERE run_id IS NOT NULL;
CREATE INDEX telemetry_outbox_deployment_id_idx ON telemetry_outbox(deployment_id)
    WHERE deployment_id IS NOT NULL;
CREATE INDEX telemetry_outbox_run_lease_idx ON telemetry_outbox(org_id, run_id, run_lease_id, id)
    WHERE run_lease_id IS NOT NULL;
CREATE INDEX telemetry_outbox_run_attempt_number_idx ON telemetry_outbox(org_id, run_id, attempt_number, id)
    WHERE attempt_number IS NOT NULL;
CREATE INDEX run_checkpoints_run_state_idx ON run_checkpoints(run_id, state, created_at DESC);
CREATE INDEX run_checkpoint_artifacts_role_idx ON run_checkpoint_artifacts(run_checkpoint_id, role, ordinal);
CREATE INDEX tokens_scope_state_idx ON tokens(org_id, project_id, environment_id, state, created_at DESC);
CREATE INDEX tokens_expiry_pending_idx ON tokens(expires_at, id)
    WHERE state = 'pending';
CREATE INDEX tokens_callback_fingerprint_pending_idx ON tokens(callback_secret_fingerprint)
    WHERE state = 'pending' AND callback_secret_fingerprint <> '';
CREATE INDEX run_waits_run_state_idx
    ON run_waits(run_id, suspension_state, created_at DESC);
CREATE INDEX workspaces_state_idx ON workspaces(environment_id, state, updated_at DESC);
CREATE UNIQUE INDEX workspaces_environment_key_uidx ON workspaces(environment_id, key)
    WHERE key IS NOT NULL;
CREATE INDEX workspace_versions_workspace_created_idx ON workspace_versions(workspace_id, created_at DESC);
CREATE INDEX public_access_tokens_expiry_active_idx ON public_access_tokens(expires_at, id)
    WHERE state = 'active';
