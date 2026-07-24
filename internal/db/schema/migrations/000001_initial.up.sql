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
    id TEXT PRIMARY KEY CHECK (
        id = btrim(id)
        AND octet_length(id) BETWEEN 1 AND 255
        AND id !~ '[[:cntrl:]]'
        AND id !~ '(^[[:space:]])|([[:space:]]$)'
    ),
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

CREATE TABLE lookup_hmac_versions (
    version INTEGER PRIMARY KEY CHECK (version > 0),
    key_fingerprint BYTEA NOT NULL UNIQUE CHECK (octet_length(key_fingerprint) = 32),
    is_current BOOLEAN NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at TIMESTAMPTZ,
    CHECK (NOT is_current OR retired_at IS NULL),
    CHECK (retired_at IS NULL OR retired_at >= activated_at)
);

CREATE UNIQUE INDEX lookup_hmac_versions_current_uidx
    ON lookup_hmac_versions ((1))
    WHERE is_current;

CREATE TABLE secrets (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    secret_id UUID NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    key_id TEXT NOT NULL CHECK (btrim(key_id) <> '' AND octet_length(key_id) <= 256),
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) >= 16),
    value_authenticator BYTEA NOT NULL CHECK (octet_length(value_authenticator) = 32),
    authenticator_key_version INTEGER NOT NULL CHECK (authenticator_key_version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (secret_id, id),
    UNIQUE (secret_id, version),
    UNIQUE (key_id, nonce),
    FOREIGN KEY (secret_id) REFERENCES secrets(id) ON DELETE RESTRICT,
    FOREIGN KEY (authenticator_key_version)
        REFERENCES lookup_hmac_versions(version)
        ON DELETE RESTRICT
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
    'runtime_substrate',
    'run_checkpoint_config',
    'run_checkpoint_vm_state',
    'run_checkpoint_memory',
    'run_checkpoint_scratch_disk',
    'workspace_process_record',
    'workspace_version'
);

CREATE TYPE worker_instance_state AS ENUM (
    'registering',
    'active',
    'draining',
    'disabled',
    'lost'
);

CREATE TABLE worker_groups (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT NOT NULL DEFAULT '',
    state worker_group_state NOT NULL DEFAULT 'active',
    enrollment_policy_fingerprint TEXT NOT NULL CHECK (btrim(enrollment_policy_fingerprint) <> ''),
    allowed_attestation_fingerprints TEXT[] NOT NULL CHECK (cardinality(allowed_attestation_fingerprints) > 0),
    launch_attestation_fingerprint TEXT CHECK (launch_attestation_fingerprint IS NULL OR btrim(launch_attestation_fingerprint) <> ''),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    allows_run BOOLEAN NOT NULL DEFAULT true,
    allows_build BOOLEAN NOT NULL DEFAULT true,
    required_cpu_millis BIGINT NOT NULL DEFAULT 1 CHECK (required_cpu_millis > 0),
    required_memory_bytes BIGINT NOT NULL DEFAULT 1 CHECK (required_memory_bytes > 0),
    required_workload_disk_bytes BIGINT NOT NULL DEFAULT 1 CHECK (required_workload_disk_bytes > 0),
    required_scratch_bytes BIGINT NOT NULL DEFAULT 1 CHECK (required_scratch_bytes > 0),
    required_build_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (required_build_cache_bytes >= 0),
    required_artifact_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (required_artifact_cache_bytes >= 0),
    required_vm_slots INTEGER NOT NULL DEFAULT 1 CHECK (required_vm_slots >= 0),
    required_build_executors INTEGER NOT NULL DEFAULT 1 CHECK (required_build_executors >= 0),
    last_scale_out_at TIMESTAMPTZ,
    last_scale_in_at TIMESTAMPTZ,
    protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (protocol_version = 'helmr.worker.v0'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, region_id),
    UNIQUE (region_id, name),
    CHECK (allows_run OR allows_build),
    CHECK (NOT allows_run OR required_vm_slots > 0),
    CHECK (NOT allows_build OR required_build_executors > 0),
    CHECK (launch_attestation_fingerprint IS NULL OR launch_attestation_fingerprint = ANY(allowed_attestation_fingerprints))
);

CREATE INDEX worker_groups_active_placement_idx
    ON worker_groups (region_id, id)
    WHERE state = 'active';

CREATE TRIGGER worker_groups_set_updated_at
    BEFORE UPDATE ON worker_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE runtime_identities (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    runtime_arch TEXT NOT NULL CHECK (runtime_arch IN ('aarch64', 'x86_64')),
    runtime_abi TEXT NOT NULL CHECK (btrim(runtime_abi) <> ''),
    kernel_digest TEXT NOT NULL CHECK (btrim(kernel_digest) <> ''),
    initramfs_digest TEXT NOT NULL CHECK (btrim(initramfs_digest) <> ''),
    rootfs_digest TEXT NOT NULL CHECK (btrim(rootfs_digest) <> ''),
    cni_profile TEXT NOT NULL CHECK (btrim(cni_profile) <> ''),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE worker_instances (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    resource_id TEXT NOT NULL CHECK (btrim(resource_id) <> ''),
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    attestation_fingerprint TEXT NOT NULL CHECK (btrim(attestation_fingerprint) <> ''),
    state worker_instance_state NOT NULL DEFAULT 'registering',
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    current_epoch BIGINT CHECK (current_epoch IS NULL OR current_epoch > 0),
    current_service_id UUID,
    protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (protocol_version = 'helmr.worker.v0'),
    supervisor_version TEXT NOT NULL DEFAULT '',
    supports_run BOOLEAN NOT NULL DEFAULT false,
    supports_build BOOLEAN NOT NULL DEFAULT false,
    toolchain_catalog_digest BYTEA,
    runtime_identity_id TEXT REFERENCES runtime_identities(id) ON DELETE RESTRICT,
    substrate_format TEXT NOT NULL DEFAULT '',
    substrate_builder_abi TEXT NOT NULL DEFAULT '',
    substrate_layout_abi TEXT NOT NULL DEFAULT '',
    certified_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (certified_cpu_millis >= 0),
    certified_memory_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_memory_bytes >= 0),
    certified_workload_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_workload_disk_bytes >= 0),
    certified_scratch_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_scratch_bytes >= 0),
    certified_build_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_build_cache_bytes >= 0),
    certified_artifact_cache_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_artifact_cache_bytes >= 0),
    certified_hugepages_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_hugepages_bytes >= 0),
    certified_checkpoint_bytes BIGINT NOT NULL DEFAULT 0 CHECK (certified_checkpoint_bytes >= 0),
    per_vm_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_cpu_millis >= 0),
    per_vm_memory_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_memory_bytes >= 0),
    per_vm_workload_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_workload_disk_bytes >= 0),
    per_vm_scratch_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_scratch_bytes >= 0),
    max_vm_slots INTEGER NOT NULL DEFAULT 0 CHECK (max_vm_slots >= 0),
    max_run_consumers INTEGER NOT NULL DEFAULT 0 CHECK (max_run_consumers >= 0),
    max_build_executors INTEGER NOT NULL DEFAULT 0 CHECK (max_build_executors >= 0),
    max_runtime_starts INTEGER NOT NULL DEFAULT 0 CHECK (max_runtime_starts >= 0),
    certification_profile TEXT NOT NULL DEFAULT '',
    certification_fingerprint TEXT NOT NULL DEFAULT '',
    epoch_started_at TIMESTAMPTZ,
    startup_inventory_epoch BIGINT CHECK (startup_inventory_epoch IS NULL OR startup_inventory_epoch > 0),
    startup_inventory_evidence JSONB,
    drain_cleanup_fingerprint TEXT,
    drain_cleanup_evidence JSONB,
    certified_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    draining_at TIMESTAMPTZ,
    disabled_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    termination_claimed_at TIMESTAMPTZ,
    provider_terminated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (worker_group_id, resource_id),
    UNIQUE (id, worker_group_id),
    CHECK (
        (current_epoch IS NULL AND current_service_id IS NULL AND epoch_started_at IS NULL)
        OR (current_epoch IS NOT NULL AND current_service_id IS NOT NULL AND epoch_started_at IS NOT NULL)
    ),
    CHECK (state NOT IN ('active', 'draining', 'lost') OR current_epoch IS NOT NULL),
    CHECK ((startup_inventory_epoch IS NULL) = (startup_inventory_evidence IS NULL)),
    CHECK (startup_inventory_epoch IS NULL OR startup_inventory_epoch = current_epoch),
    CHECK (startup_inventory_evidence IS NULL OR (jsonb_typeof(startup_inventory_evidence) = 'object' AND octet_length(startup_inventory_evidence::text) <= 16384)),
    CHECK ((drain_cleanup_fingerprint IS NULL) = (drain_cleanup_evidence IS NULL)),
    CHECK (drain_cleanup_fingerprint IS NULL OR drain_cleanup_fingerprint ~ '^[0-9a-f]{64}$'),
    CHECK (drain_cleanup_evidence IS NULL OR (jsonb_typeof(drain_cleanup_evidence) = 'object' AND octet_length(drain_cleanup_evidence::text) <= 16384)),
    CHECK (drain_cleanup_evidence IS NULL OR state = 'disabled'),
    CONSTRAINT worker_instances_certification_shape_check CHECK (
        state NOT IN ('active', 'draining')
        OR (
            btrim(supervisor_version) <> ''
            AND certified_at IS NOT NULL
            AND activated_at IS NOT NULL
            AND btrim(certification_profile) <> ''
            AND btrim(certification_fingerprint) <> ''
            AND certified_cpu_millis > 0
            AND certified_memory_bytes > 0
            AND per_vm_cpu_millis > 0
            AND per_vm_memory_bytes > 0
            AND per_vm_workload_disk_bytes > 0
            AND per_vm_scratch_bytes > 0
            AND (supports_run OR supports_build)
        )
        OR (
            state = 'draining'
            AND supervisor_version = ''
            AND NOT supports_run
            AND NOT supports_build
            AND toolchain_catalog_digest IS NULL
            AND runtime_identity_id IS NULL
            AND substrate_format = ''
            AND substrate_builder_abi = ''
            AND substrate_layout_abi = ''
            AND certified_cpu_millis = 0
            AND certified_memory_bytes = 0
            AND certified_workload_disk_bytes = 0
            AND certified_scratch_bytes = 0
            AND certified_build_cache_bytes = 0
            AND certified_artifact_cache_bytes = 0
            AND certified_hugepages_bytes = 0
            AND certified_checkpoint_bytes = 0
            AND per_vm_cpu_millis = 0
            AND per_vm_memory_bytes = 0
            AND per_vm_workload_disk_bytes = 0
            AND per_vm_scratch_bytes = 0
            AND max_vm_slots = 0
            AND max_run_consumers = 0
            AND max_build_executors = 0
            AND max_runtime_starts = 0
            AND certification_profile = ''
            AND certification_fingerprint = ''
            AND certified_at IS NULL
            AND activated_at IS NULL
        )
    ),
    CHECK (
        state NOT IN ('active', 'draining')
        OR NOT supports_run
        OR (
            runtime_identity_id IS NOT NULL
            AND max_vm_slots > 0
            AND max_run_consumers > 0
            AND max_runtime_starts > 0
        )
    ),
    CHECK (state NOT IN ('active', 'draining') OR NOT supports_build OR max_build_executors > 0),
    CHECK (
        (supports_run
         AND btrim(substrate_format) <> ''
         AND btrim(substrate_builder_abi) <> ''
         AND btrim(substrate_layout_abi) <> '')
        OR
        (NOT supports_run
         AND substrate_format = ''
         AND substrate_builder_abi = ''
         AND substrate_layout_abi = '')
    ),
    CHECK (toolchain_catalog_digest IS NULL OR octet_length(toolchain_catalog_digest) = 32),
    CHECK (supports_build OR toolchain_catalog_digest IS NULL),
    CHECK (
        state NOT IN ('active', 'draining')
        OR NOT supports_build
        OR toolchain_catalog_digest IS NOT NULL
    ),
    CHECK (state <> 'draining' OR draining_at IS NOT NULL),
    CHECK (state <> 'disabled' OR disabled_at IS NOT NULL),
    CHECK (state <> 'lost' OR lost_at IS NOT NULL),
    CHECK (termination_claimed_at IS NULL OR state IN ('disabled', 'lost')),
    CHECK (provider_terminated_at IS NULL OR termination_claimed_at IS NOT NULL),
    CHECK (provider_terminated_at IS NULL OR provider_terminated_at >= termination_claimed_at)
);

CREATE INDEX worker_instances_active_placement_idx
    ON worker_instances (worker_group_id, id)
    WHERE state = 'active';

CREATE INDEX worker_instances_current_epoch_idx
    ON worker_instances (id, current_epoch)
    WHERE current_epoch IS NOT NULL;

CREATE TABLE worker_enrollment_nonces (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    worker_group_id TEXT NOT NULL REFERENCES worker_groups(id) ON DELETE RESTRICT,
    worker_instance_id UUID NOT NULL,
    key_prefix TEXT NOT NULL UNIQUE CHECK (btrim(key_prefix) <> ''),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    allows_run BOOLEAN NOT NULL,
    allows_build BOOLEAN NOT NULL,
    protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (protocol_version = 'helmr.worker.v0'),
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
    workload_disk_pressure_bps INTEGER NOT NULL CHECK (workload_disk_pressure_bps BETWEEN 0 AND 10000),
    scratch_pressure_bps INTEGER NOT NULL CHECK (scratch_pressure_bps BETWEEN 0 AND 10000),
    build_cache_pressure_bps INTEGER NOT NULL CHECK (build_cache_pressure_bps BETWEEN 0 AND 10000),
    artifact_cache_pressure_bps INTEGER NOT NULL CHECK (artifact_cache_pressure_bps BETWEEN 0 AND 10000),
    checkpoint_pressure_bps INTEGER NOT NULL CHECK (checkpoint_pressure_bps BETWEEN 0 AND 10000),
    leaked_slot_count INTEGER NOT NULL CHECK (leaked_slot_count >= 0),
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
    CHECK (jsonb_typeof(health_details) = 'object'),
    CHECK (octet_length(health_details::text) <= 16384)
);

CREATE INDEX worker_observations_freshness_idx
    ON worker_observations (observed_at, worker_instance_id, worker_epoch);

CREATE TABLE artifacts (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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

CREATE FUNCTION reject_artifact_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'Artifact rows are immutable'
        USING ERRCODE = '23514';
END
$$;

CREATE TRIGGER artifacts_immutable
BEFORE UPDATE ON artifacts
FOR EACH ROW
EXECUTE FUNCTION reject_artifact_update();

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
    'token.complete'
);

CREATE TYPE wait_kind AS ENUM (
    'token',
    'timer',
    'child',
    'actor_input'
);

CREATE TYPE wait_state AS ENUM (
    'pending',
    'completed',
    'failed',
    'cancelled'
);

CREATE TYPE run_wait_state AS ENUM (
    'hot',
    'checkpointing',
    'parked',
    'resume_pending',
    'resuming',
    'released',
    'cancelled',
    'failed'
);

CREATE TYPE run_checkpoint_kind AS ENUM (
    'suspend',
    'handoff_resume'
);

CREATE TYPE run_checkpoint_state AS ENUM (
    'creating',
    'ready',
    'invalid',
    'deleted'
);

CREATE TYPE runtime_desired_state AS ENUM (
    'ready',
    'closed'
);

CREATE TYPE runtime_observed_state AS ENUM (
    'allocated',
    'preparing',
    'ready',
    'closing',
    'closed',
    'failed',
    'lost'
);

CREATE TYPE worker_network_slot_state AS ENUM (
    'available',
    'assigned',
    'bound',
    'reclaiming',
    'quarantined',
    'lost'
);

CREATE TYPE run_status AS ENUM (
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
);

CREATE TYPE run_lease_state AS ENUM (
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
);

CREATE TYPE deployment_build_lease_state AS ENUM (
    'assigned',
    'starting',
    'running',
    'succeeded',
    'failed',
    'cancelled',
    'lost',
    'rejected',
    'expired'
);

CREATE TYPE workspace_state AS ENUM (
    'active',
    'deleting',
    'recovery_required',
    'deleted'
);

CREATE TYPE workspace_desired_state AS ENUM (
    'active',
    'stopped',
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
    'private',
    'committed',
    'discarded'
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

CREATE TYPE workspace_lease_state AS ENUM (
    'active',
    'releasing',
    'released',
    'expired',
    'fenced',
    'lost'
);

CREATE TYPE workspace_filesystem_mode AS ENUM (
    'write'
);

CREATE TYPE workspace_process_state AS ENUM (
    'pending',
    'starting',
    'running',
    'exit_requested',
    'exited',
    'cancelled',
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
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    build_region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    build_architecture TEXT NOT NULL CHECK (build_architecture = 'x86_64'),
    build_runtime_digest BYTEA NOT NULL CHECK (octet_length(build_runtime_digest) = 32),
    build_standard_toolchain_digest BYTEA NOT NULL CHECK (octet_length(build_standard_toolchain_digest) = 32),
    build_manager_name TEXT NOT NULL CHECK (build_manager_name IN ('npm', 'bun')),
    build_manager_version TEXT NOT NULL CHECK (
        octet_length(build_manager_version) BETWEEN 1 AND 64
        AND build_manager_version ~
            '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'
    ),
    build_manager_digest BYTEA NOT NULL CHECK (octet_length(build_manager_digest) = 32),
    build_contract_version TEXT NOT NULL CHECK (build_contract_version = 'helmr.program-build.v0'),
    version TEXT NOT NULL CHECK (btrim(version) <> ''),
    content_hash TEXT NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
    api_version TEXT NOT NULL DEFAULT '2026-06-06' CHECK (
        btrim(api_version) <> '' AND octet_length(api_version) <= 255
    ),
    sdk_version TEXT NOT NULL DEFAULT '' CHECK (
        sdk_version = btrim(sdk_version) AND octet_length(sdk_version) <= 255
    ),
    cli_version TEXT NOT NULL DEFAULT '' CHECK (
        cli_version = btrim(cli_version) AND octet_length(cli_version) <= 255
    ),
    worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (worker_protocol_version = 'helmr.worker.v0'),
    deployment_source_artifact_id UUID NOT NULL,
    program_artifact_id UUID,
    program_artifact_kind artifact_kind NOT NULL DEFAULT 'deployment_program'
        CHECK (program_artifact_kind = 'deployment_program'),
    program_runtime_digest BYTEA,
    program_architecture TEXT,
    program_receipt JSONB,
    queue_config JSONB,
    status deployment_status NOT NULL DEFAULT 'queued',
    failure JSONB NOT NULL DEFAULT '{}'::jsonb,
    current_build_lease_id UUID,
    build_requested_cpu_millis BIGINT NOT NULL DEFAULT 3000 CHECK (build_requested_cpu_millis = 3000),
    build_requested_memory_bytes BIGINT NOT NULL DEFAULT 4294967296 CHECK (build_requested_memory_bytes = 4294967296),
    build_requested_workload_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (build_requested_workload_disk_bytes = 0),
    build_requested_scratch_bytes BIGINT NOT NULL DEFAULT 34359738368 CHECK (build_requested_scratch_bytes = 34359738368),
    build_requested_executors INTEGER NOT NULL DEFAULT 1 CHECK (build_requested_executors = 1),
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
         AND program_runtime_digest IS NULL
         AND program_architecture IS NULL
         AND program_receipt IS NULL)
        OR
        (program_artifact_id IS NOT NULL
         AND program_runtime_digest IS NOT NULL
         AND octet_length(program_runtime_digest) = 32
         AND program_architecture IS NOT NULL
         AND program_architecture IN ('aarch64', 'x86_64')
         AND program_architecture = build_architecture
         AND program_runtime_digest = build_runtime_digest
         AND jsonb_typeof(program_receipt) = 'object')
    ),
    CONSTRAINT deployments_queue_config_check CHECK (
        (status = 'deployed' AND jsonb_typeof(queue_config) = 'object')
        OR
        (status <> 'deployed' AND queue_config IS NULL)
    )
);

CREATE FUNCTION enforce_deployment_program_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    program_artifact artifacts%ROWTYPE;
    source_artifact artifacts%ROWTYPE;
    receipt jsonb := NEW.program_receipt;
    program_size numeric;
    source_size numeric;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.program_receipt IS NOT NULL THEN
        IF NEW.program_receipt IS DISTINCT FROM OLD.program_receipt
           OR NEW.program_artifact_id IS DISTINCT FROM OLD.program_artifact_id
           OR NEW.program_runtime_digest IS DISTINCT FROM OLD.program_runtime_digest
           OR NEW.program_architecture IS DISTINCT FROM OLD.program_architecture
           OR NEW.deployment_source_artifact_id IS DISTINCT FROM OLD.deployment_source_artifact_id
           OR NEW.build_architecture IS DISTINCT FROM OLD.build_architecture
           OR NEW.build_runtime_digest IS DISTINCT FROM OLD.build_runtime_digest
           OR NEW.build_standard_toolchain_digest IS DISTINCT FROM OLD.build_standard_toolchain_digest
           OR NEW.build_manager_name IS DISTINCT FROM OLD.build_manager_name
           OR NEW.build_manager_version IS DISTINCT FROM OLD.build_manager_version
           OR NEW.build_manager_digest IS DISTINCT FROM OLD.build_manager_digest
           OR NEW.build_contract_version IS DISTINCT FROM OLD.build_contract_version THEN
            RAISE EXCEPTION 'published deployment Program authority is immutable'
                USING ERRCODE = '23514';
        END IF;
    END IF;

    IF receipt IS NULL THEN
        RETURN NEW;
    END IF;

    IF (
        jsonb_typeof(receipt) = 'object'
        AND receipt - ARRAY[
            'architecture', 'buildContractVersion', 'formatVersion', 'lockfile',
            'manager', 'program', 'runtime', 'source', 'standardToolchainDigest'
        ]::text[] = '{}'::jsonb
        AND jsonb_typeof(receipt->'architecture') = 'string'
        AND jsonb_typeof(receipt->'buildContractVersion') = 'string'
        AND receipt->'formatVersion' = '0'::jsonb
        AND jsonb_typeof(receipt->'lockfile') = 'object'
        AND (receipt->'lockfile') - ARRAY['digest', 'path']::text[] = '{}'::jsonb
        AND jsonb_typeof(receipt#>'{lockfile,digest}') = 'string'
        AND jsonb_typeof(receipt#>'{lockfile,path}') = 'string'
        AND jsonb_typeof(receipt->'manager') = 'object'
        AND (receipt->'manager') - ARRAY['digest', 'name', 'version']::text[] = '{}'::jsonb
        AND jsonb_typeof(receipt#>'{manager,digest}') = 'string'
        AND jsonb_typeof(receipt#>'{manager,name}') = 'string'
        AND jsonb_typeof(receipt#>'{manager,version}') = 'string'
        AND jsonb_typeof(receipt->'program') = 'object'
        AND (receipt->'program') - ARRAY[
            'artifactId', 'digest', 'indexDigest', 'mediaType', 'sizeBytes'
        ]::text[] = '{}'::jsonb
        AND jsonb_typeof(receipt#>'{program,artifactId}') = 'string'
        AND jsonb_typeof(receipt#>'{program,digest}') = 'string'
        AND jsonb_typeof(receipt#>'{program,indexDigest}') = 'string'
        AND jsonb_typeof(receipt#>'{program,mediaType}') = 'string'
        AND jsonb_typeof(receipt#>'{program,sizeBytes}') = 'number'
        AND jsonb_typeof(receipt->'runtime') = 'object'
        AND (receipt->'runtime') - ARRAY['apiVersion', 'digest']::text[] = '{}'::jsonb
        AND jsonb_typeof(receipt#>'{runtime,apiVersion}') = 'string'
        AND jsonb_typeof(receipt#>'{runtime,digest}') = 'string'
        AND jsonb_typeof(receipt->'source') = 'object'
        AND (receipt->'source') - ARRAY[
            'artifactId', 'digest', 'mediaType', 'sizeBytes'
        ]::text[] = '{}'::jsonb
        AND jsonb_typeof(receipt#>'{source,artifactId}') = 'string'
        AND jsonb_typeof(receipt#>'{source,digest}') = 'string'
        AND jsonb_typeof(receipt#>'{source,mediaType}') = 'string'
        AND jsonb_typeof(receipt#>'{source,sizeBytes}') = 'number'
        AND jsonb_typeof(receipt->'standardToolchainDigest') = 'string'
    ) IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION 'deployment Program receipt has an invalid closed shape'
            USING ERRCODE = '23514';
    END IF;

    program_size := (receipt#>>'{program,sizeBytes}')::numeric;
    source_size := (receipt#>>'{source,sizeBytes}')::numeric;
    IF (
        receipt->>'architecture' = NEW.program_architecture
        AND receipt->>'buildContractVersion' = NEW.build_contract_version
        AND receipt#>>'{runtime,apiVersion}' = 'helmr.runtime.v0'
        AND receipt#>>'{runtime,digest}' =
            'sha256:' || encode(NEW.build_runtime_digest, 'hex')
        AND receipt->>'standardToolchainDigest' =
            'sha256:' || encode(NEW.build_standard_toolchain_digest, 'hex')
        AND receipt#>>'{manager,name}' = NEW.build_manager_name
        AND receipt#>>'{manager,version}' = NEW.build_manager_version
        AND receipt#>>'{manager,digest}' =
            'sha256:' || encode(NEW.build_manager_digest, 'hex')
        AND receipt#>>'{program,artifactId}' = NEW.program_artifact_id::text
        AND receipt#>>'{source,artifactId}' = NEW.deployment_source_artifact_id::text
        AND receipt#>>'{program,mediaType}' =
            'application/vnd.helmr.deployment-program.v0+squashfs'
        AND receipt#>>'{source,mediaType}' =
            'application/vnd.helmr.deployment-source.v0+tar'
        AND receipt#>>'{program,digest}' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt#>>'{program,indexDigest}' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt#>>'{source,digest}' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt#>>'{runtime,digest}' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt->>'standardToolchainDigest' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt#>>'{lockfile,digest}' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt#>>'{manager,digest}' ~ '^sha256:[0-9a-f]{64}$'
        AND receipt#>>'{manager,name}' IN ('npm', 'bun')
        AND (
            (receipt#>>'{manager,name}' = 'npm'
             AND receipt#>>'{lockfile,path}' = 'package-lock.json')
            OR
            (receipt#>>'{manager,name}' = 'bun'
             AND receipt#>>'{lockfile,path}' IN ('bun.lock', 'bun.lockb'))
        )
        AND octet_length(receipt#>>'{manager,version}') BETWEEN 1 AND 64
        AND receipt#>>'{manager,version}' ~
            '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'
        AND program_size = trunc(program_size)
        AND program_size BETWEEN 1 AND 13958643712
        AND source_size = trunc(source_size)
        AND source_size BETWEEN 1 AND 9007199254740991
    ) IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION 'deployment Program receipt contradicts immutable build authority'
            USING ERRCODE = '23514';
    END IF;

    SELECT *
      INTO program_artifact
      FROM artifacts
     WHERE environment_id = NEW.environment_id
       AND id = NEW.program_artifact_id;
    IF NOT FOUND
       OR program_artifact.kind <> 'deployment_program'
       OR program_artifact.digest IS DISTINCT FROM receipt#>>'{program,digest}'
       OR program_artifact.size_bytes::numeric IS DISTINCT FROM program_size
       OR program_artifact.media_type IS DISTINCT FROM receipt#>>'{program,mediaType}' THEN
        RAISE EXCEPTION 'deployment Program receipt does not match its Program Artifact'
            USING ERRCODE = '23514';
    END IF;

    SELECT *
      INTO source_artifact
      FROM artifacts
     WHERE environment_id = NEW.environment_id
       AND id = NEW.deployment_source_artifact_id;
    IF NOT FOUND
       OR source_artifact.kind <> 'deployment_source'
       OR source_artifact.digest IS DISTINCT FROM receipt#>>'{source,digest}'
       OR source_artifact.size_bytes::numeric IS DISTINCT FROM source_size
       OR source_artifact.media_type IS DISTINCT FROM receipt#>>'{source,mediaType}' THEN
        RAISE EXCEPTION 'deployment Program receipt does not match its source Artifact'
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END
$$;

CREATE CONSTRAINT TRIGGER deployments_program_receipt_authority
AFTER INSERT OR UPDATE ON deployments
DEFERRABLE INITIALLY IMMEDIATE
FOR EACH ROW
EXECUTE FUNCTION enforce_deployment_program_receipt();

CREATE INDEX deployments_program_artifact_idx
    ON deployments (environment_id, program_artifact_id);

CREATE TABLE deployment_definitions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('task', 'actor', 'workspace')),
    declared_id TEXT NOT NULL CHECK (
        declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(declared_id) BETWEEN 1 AND 128
    ),
    manifest_version INTEGER NOT NULL CHECK (manifest_version = 0),
    manifest JSONB NOT NULL CHECK (jsonb_typeof(manifest) = 'object'),
    manifest_digest BYTEA NOT NULL CHECK (octet_length(manifest_digest) = 32),
    workspace_architecture TEXT,
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
            kind = 'workspace'
            AND workspace_architecture IS NOT NULL
            AND workspace_architecture IN ('aarch64', 'x86_64')
            AND artifact_id IS NOT NULL
        )
        OR
        (
            kind IN ('task', 'actor')
            AND workspace_architecture IS NULL
            AND artifact_id IS NULL
        )
    )
);

CREATE INDEX deployment_definitions_artifact_idx
    ON deployment_definitions (environment_id, artifact_id);

CREATE TABLE deployment_build_leases (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_id UUID NOT NULL,
    build_region_id TEXT NOT NULL,
    lease_sequence BIGINT NOT NULL CHECK (lease_sequence BETWEEN 1 AND 3),
    worker_group_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (worker_protocol_version = 'helmr.worker.v0'),
    requested_cpu_millis BIGINT NOT NULL CHECK (requested_cpu_millis = 3000),
    requested_memory_bytes BIGINT NOT NULL CHECK (requested_memory_bytes = 4294967296),
    requested_workload_disk_bytes BIGINT NOT NULL CHECK (requested_workload_disk_bytes = 0),
    requested_scratch_bytes BIGINT NOT NULL CHECK (requested_scratch_bytes = 34359738368),
    requested_build_executors INTEGER NOT NULL DEFAULT 1 CHECK (requested_build_executors = 1),
    build_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    trace_id TEXT,
    span_id TEXT,
    parent_span_id TEXT,
    traceparent TEXT,
    state deployment_build_lease_state NOT NULL DEFAULT 'assigned',
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
    CHECK (octet_length(build_snapshot::text) <= 16384),
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
    CHECK (terminal_error IS NULL OR (jsonb_typeof(terminal_error) = 'object' AND octet_length(terminal_error::text) <= 16384)),
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

CREATE TABLE runtime_substrates (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    deployment_definition_id UUID NOT NULL,
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
    UNIQUE (environment_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, deployment_definition_id, id),
    CONSTRAINT runtime_substrates_input_key
        UNIQUE (org_id, project_id, environment_id, deployment_definition_id, substrate_format, builder_abi, layout_abi),
    FOREIGN KEY (environment_id, deployment_definition_id)
        REFERENCES deployment_definitions(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, artifact_id)
        REFERENCES artifacts(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE TRIGGER runtime_substrates_set_updated_at
    BEFORE UPDATE ON runtime_substrates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX runtime_substrates_deployment_definition_idx
    ON runtime_substrates (environment_id, deployment_definition_id);

CREATE TABLE idempotency_claims (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    environment_id UUID NOT NULL,
    operation TEXT NOT NULL CHECK (btrim(operation) <> '' AND octet_length(operation) <= 128),
    scope_hash BYTEA NOT NULL CHECK (octet_length(scope_hash) = 32),
    key_hash BYTEA NOT NULL CHECK (octet_length(key_hash) = 32),
    hash_key_version INTEGER NOT NULL CHECK (hash_key_version > 0),
    generation BIGINT NOT NULL CHECK (generation > 0),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed', 'failed')),
    receipt JSONB,
    accepted_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, operation, scope_hash, key_hash, generation),
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE CASCADE,
    FOREIGN KEY (hash_key_version)
        REFERENCES lookup_hmac_versions(version)
        ON DELETE RESTRICT,
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
    ON idempotency_claims (environment_id, operation, scope_hash, key_hash)
    WHERE retired_at IS NULL;

CREATE INDEX idempotency_claims_live_expiry_idx
    ON idempotency_claims (expires_at, id)
    WHERE retired_at IS NULL AND expires_at IS NOT NULL;

CREATE INDEX idempotency_claims_retired_idx
    ON idempotency_claims (retired_at, id)
    WHERE retired_at IS NOT NULL;

CREATE TABLE schedules (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^sch_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    target_kind TEXT NOT NULL DEFAULT 'task' CHECK (target_kind = 'task'),
    task_declared_id TEXT NOT NULL CHECK (
        task_declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(task_declared_id) BETWEEN 1 AND 128
    ),
    deployment_definition_id UUID,
    deployment_id UUID,
    workspace_ref_id UUID,
    workspace_ref_key TEXT,
    workspace_id UUID,
    cron_pattern TEXT NOT NULL CHECK (octet_length(cron_pattern) BETWEEN 1 AND 1024),
    timezone TEXT NOT NULL CHECK (octet_length(timezone) BETWEEN 1 AND 255),
    cron_semantics_version TEXT NOT NULL DEFAULT 'robfig-cron-v3.0.1/standard-5-field'
        CHECK (cron_semantics_version = 'robfig-cron-v3.0.1/standard-5-field'),
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    state TEXT NOT NULL CHECK (state IN ('pending_workspace', 'active', 'errored', 'archived')),
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    effective_from TIMESTAMPTZ NOT NULL,
    next_fire_at TIMESTAMPTZ,
    last_fire_at TIMESTAMPTZ,
    claimed_by TEXT CHECK (claimed_by IS NULL OR btrim(claimed_by) <> ''),
    claim_expires_at TIMESTAMPTZ,
    retry_step SMALLINT CHECK (retry_step BETWEEN 1 AND 10),
    retry_after TIMESTAMPTZ,
    last_error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, task_declared_id),
    UNIQUE (environment_id, id, generation),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
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
    CHECK ((workspace_ref_id IS NULL) <> (workspace_ref_key IS NULL)),
    CHECK (workspace_ref_key IS NULL OR (btrim(workspace_ref_key) <> '' AND octet_length(workspace_ref_key) <= 512)),
    CHECK (
        (state = 'archived'
         AND deployment_definition_id IS NULL
         AND deployment_id IS NULL
         AND next_fire_at IS NULL
         AND claimed_by IS NULL
         AND claim_expires_at IS NULL
         AND retry_step IS NULL
         AND retry_after IS NULL
         AND last_error IS NULL)
        OR
        (state <> 'archived'
         AND deployment_definition_id IS NOT NULL
         AND deployment_id IS NOT NULL
         AND (
             (state = 'pending_workspace'
              AND workspace_ref_key IS NOT NULL
              AND workspace_id IS NULL
              AND next_fire_at IS NULL
              AND claimed_by IS NULL
             AND claim_expires_at IS NULL)
             OR
             (state IN ('active', 'errored')
              AND workspace_id IS NOT NULL
              AND next_fire_at IS NOT NULL)
         ))
    ),
    CHECK ((claimed_by IS NULL) = (claim_expires_at IS NULL)),
    CHECK ((retry_step IS NULL) = (retry_after IS NULL)),
    CHECK (retry_step IS NULL OR state = 'active'),
    CHECK (claimed_by IS NULL OR (state = 'active' AND next_fire_at IS NOT NULL)),
    CHECK (
        (state = 'errored'
         AND last_error IS NOT NULL
         AND jsonb_typeof(last_error) = 'object'
         AND last_error ? 'code'
         AND last_error ? 'message'
         AND last_error - ARRAY['code', 'message'] = '{}'::jsonb
         AND last_error->>'code' IN (
             'task-authority-invalid',
             'workspace-unavailable',
             'architecture-incompatible',
             'generation-invalid',
             'input-invalid'
         )
         AND jsonb_typeof(last_error->'message') = 'string'
         AND btrim(last_error->>'message') <> ''
         AND octet_length(last_error->>'message') <= 1024)
        OR
        (state <> 'errored' AND last_error IS NULL)
    )
);

CREATE INDEX schedules_pending_workspace_idx
    ON schedules (environment_id, workspace_ref_key, id)
    WHERE state = 'pending_workspace';

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

CREATE TABLE workspaces (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^wsp_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    region_id TEXT NOT NULL REFERENCES regions(id) ON DELETE RESTRICT,
    declaration_kind TEXT CHECK (declaration_kind IS NULL OR declaration_kind = 'workspace'),
    workspace_declared_id TEXT CHECK (
        workspace_declared_id IS NULL
        OR (
        workspace_declared_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
        AND octet_length(workspace_declared_id) BETWEEN 1 AND 128
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
    create_idempotency_key TEXT NOT NULL DEFAULT '',
    create_idempotency_expires_at TIMESTAMPTZ,
    create_request_fingerprint TEXT NOT NULL DEFAULT '',
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    stop_generation BIGINT NOT NULL DEFAULT 0 CHECK (stop_generation >= 0),
    owner_actor_id UUID,
    owner_run_id UUID,
    ownership_generation BIGINT NOT NULL DEFAULT 0 CHECK (ownership_generation >= 0),
    writer_generation BIGINT NOT NULL DEFAULT 0 CHECK (writer_generation >= 0),
    head_version_id UUID,
    state workspace_state NOT NULL DEFAULT 'active',
    desired_state workspace_desired_state NOT NULL DEFAULT 'active',
    dirty_state workspace_dirty_state NOT NULL DEFAULT 'clean',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    retention_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    UNIQUE (org_id, id),
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, id, deployment_definition_id),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, region_id, id),
    UNIQUE (org_id, project_id, environment_id, id, region_id),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    CONSTRAINT workspaces_deployment_definition_fk
        FOREIGN KEY (environment_id, deployment_definition_id, declaration_kind, workspace_declared_id)
        REFERENCES deployment_definitions(environment_id, id, kind, declared_id)
        ON DELETE RESTRICT,
    CHECK (num_nonnulls(owner_actor_id, owner_run_id) <= 1),
    CHECK (
        (state <> 'deleted'
         AND declaration_kind = 'workspace'
         AND workspace_declared_id IS NOT NULL
         AND deployment_definition_id IS NOT NULL
         AND head_version_id IS NOT NULL
         AND deleted_at IS NULL)
        OR
        (state = 'deleted'
         AND declaration_kind IS NULL
         AND workspace_declared_id IS NULL
         AND deployment_definition_id IS NULL
         AND head_version_id IS NULL
         AND owner_actor_id IS NULL
         AND owner_run_id IS NULL
         AND metadata = '{}'::jsonb
         AND tags = '{}'::text[]
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
        declaration_kind,
        workspace_declared_id
    );

ALTER TABLE schedules
    ADD CONSTRAINT schedules_workspace_fk
    FOREIGN KEY (environment_id, workspace_id)
    REFERENCES workspaces(environment_id, id)
    ON DELETE RESTRICT;

ALTER TABLE schedules
    ADD CONSTRAINT schedules_workspace_ref_id_fk
    FOREIGN KEY (environment_id, workspace_ref_id)
    REFERENCES workspaces(environment_id, id)
    ON DELETE RESTRICT;

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

CREATE TABLE actors (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^act_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    declaration_kind TEXT NOT NULL DEFAULT 'actor' CHECK (declaration_kind = 'actor'),
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
    failure_code TEXT,
    failure_run_id UUID,
    next_input_sequence BIGINT NOT NULL DEFAULT 1 CHECK (next_input_sequence BETWEEN 1 AND 9007199254740992),
    committed_input_sequence BIGINT NOT NULL DEFAULT 0 CHECK (committed_input_sequence BETWEEN 0 AND 9007199254740991),
    next_output_sequence BIGINT NOT NULL DEFAULT 1 CHECK (next_output_sequence BETWEEN 1 AND 9007199254740992),
    input_retention_floor BIGINT NOT NULL DEFAULT 1 CHECK (input_retention_floor BETWEEN 1 AND 9007199254740992),
    output_retention_floor BIGINT NOT NULL DEFAULT 1 CHECK (output_retention_floor BETWEEN 1 AND 9007199254740992),
    managed_queue_name TEXT NOT NULL CHECK (btrim(managed_queue_name) <> '' AND octet_length(managed_queue_name) <= 256),
    managed_concurrency_key TEXT CHECK (
        managed_concurrency_key IS NULL
        OR (
            octet_length(managed_concurrency_key) BETWEEN 1 AND 512
            AND ascii(left(managed_concurrency_key, 1)) NOT BETWEEN 9 AND 13
            AND ascii(left(managed_concurrency_key, 1)) <> 32
            AND ascii(right(managed_concurrency_key, 1)) NOT BETWEEN 9 AND 13
            AND ascii(right(managed_concurrency_key, 1)) <> 32
        )
    ),
    managed_queue_concurrency_limit BIGINT CHECK (
        managed_queue_concurrency_limit BETWEEN 1 AND 9007199254740991
    ),
    managed_priority INTEGER NOT NULL DEFAULT 0,
    managed_queued_ttl_ms BIGINT CHECK (managed_queued_ttl_ms BETWEEN 1 AND 9007199254740991),
    managed_max_active_duration_ms BIGINT NOT NULL CHECK (managed_max_active_duration_ms BETWEEN 1 AND 9007199254740991),
    managed_retry_policy_version INTEGER NOT NULL DEFAULT 0 CHECK (managed_retry_policy_version = 0),
    managed_retry_policy JSONB NOT NULL DEFAULT '{"enabled":false}'::jsonb CHECK (jsonb_typeof(managed_retry_policy) = 'object'),
    managed_run_metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(managed_run_metadata) = 'object'),
    managed_run_tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    state TEXT NOT NULL DEFAULT 'open' CHECK (
        state IN ('open', 'closing', 'closed', 'cancelling', 'cancelled', 'failed', 'expired')
    ),
    close_sequence BIGINT,
    expires_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    tags TEXT[] NOT NULL DEFAULT '{}'::text[],
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    expired_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (id, workspace_id),
    UNIQUE (id, actor_declared_id, deployment_definition_id),
    UNIQUE (id, actor_declared_id, deployment_definition_id, workspace_id),
    FOREIGN KEY (org_id, project_id, environment_id)
        REFERENCES environments(org_id, project_id, id)
        ON DELETE CASCADE,
    CONSTRAINT actors_deployment_definition_fk
        FOREIGN KEY (environment_id, deployment_definition_id, declaration_kind, actor_declared_id)
        REFERENCES deployment_definitions(environment_id, id, kind, declared_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    CHECK (key IS NULL OR (
        octet_length(key) BETWEEN 1 AND 512
        AND key !~ '^[[:space:]]'
        AND key !~ '[[:space:]]$'
    )),
    CHECK (committed_input_sequence < next_input_sequence),
    CHECK (input_retention_floor <= committed_input_sequence + 1),
    CHECK (output_retention_floor <= next_output_sequence)
    ,
    CHECK (
        (state = 'failed' AND failure_code IN ('no-progress', 'run-failed', 'run-expired', 'platform-failure') AND failure_run_id IS NOT NULL)
        OR
        (state <> 'failed' AND failure_code IS NULL AND failure_run_id IS NULL)
    )
);

CREATE UNIQUE INDEX actors_environment_declared_id_key_uidx
    ON actors (environment_id, actor_declared_id, key)
    WHERE key IS NOT NULL;

CREATE INDEX actors_deployment_definition_idx
    ON actors (
        environment_id,
        deployment_definition_id,
        declaration_kind,
        actor_declared_id
    );

CREATE INDEX actors_list_idx
    ON actors (
        environment_id,
        actor_declared_id,
        created_at DESC,
        public_id DESC
    );

CREATE INDEX actors_expiry_due_idx
    ON actors (org_id, expires_at, id)
    WHERE state = 'open'
      AND current_run_id IS NULL
      AND expires_at IS NOT NULL;

CREATE TABLE runs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^run_[a-z2-7]{26}$'),
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
    actor_id UUID,
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
    actor_start_input_sequence BIGINT,
    actor_start_input_high_watermark BIGINT,
    payload JSONB,
    output JSONB,
    terminal_reason_code TEXT,
    error JSONB,
    status run_status NOT NULL DEFAULT 'queued',
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
    UNIQUE (actor_id, id),
    UNIQUE (actor_id, workspace_id, id),
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT runs_actor_definition_workspace_fk
        FOREIGN KEY (actor_id, entrypoint_declared_id, deployment_definition_id, workspace_id)
        REFERENCES actors(id, actor_declared_id, deployment_definition_id, workspace_id)
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
         AND actor_id IS NULL
         AND actor_start_input_sequence IS NULL
         AND actor_start_input_high_watermark IS NULL)
        OR
        (entrypoint_kind = 'actor'
         AND actor_id IS NOT NULL
         AND actor_start_input_sequence IS NOT NULL
         AND actor_start_input_high_watermark IS NOT NULL
         AND actor_start_input_high_watermark >= actor_start_input_sequence
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
         AND terminal_reason_code IS NULL
         AND output IS NULL
         AND error IS NULL)
        OR
        (status = 'succeeded'
         AND terminal_at IS NOT NULL
         AND terminal_reason_code IS NULL
         AND error IS NULL)
        OR
        (status IN ('failed', 'cancelled', 'expired', 'system_failed')
         AND terminal_at IS NOT NULL
         AND terminal_reason_code IS NOT NULL
         AND btrim(terminal_reason_code) <> ''
         AND output IS NULL)
    ),
    CHECK ((status = 'retry_delayed') = (retry_at IS NOT NULL))
);

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_owner_actor_fk
    FOREIGN KEY (owner_actor_id, id)
    REFERENCES actors(id, workspace_id)
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
    actor_start_input_sequence BIGINT CHECK (actor_start_input_sequence IS NULL OR actor_start_input_sequence >= 0),
    base_workspace_version_id UUID NOT NULL,
    terminal_actor_input_sequence BIGINT CHECK (terminal_actor_input_sequence IS NULL OR terminal_actor_input_sequence >= 0),
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
         AND actor_start_input_sequence IS NULL
         AND terminal_actor_input_sequence IS NULL)
        OR
        (entrypoint_kind = 'actor'
         AND actor_start_input_sequence IS NOT NULL)
    ),
    CHECK (
        (terminal_outcome IS NULL
         AND terminal_at IS NULL
         AND terminal_reason_code IS NULL
         AND terminal_error IS NULL
         AND terminal_actor_input_sequence IS NULL)
        OR
        (terminal_outcome IS NOT NULL
         AND terminal_at IS NOT NULL
         AND terminal_reason_code IS NOT NULL
         AND btrim(terminal_reason_code) <> ''
         AND octet_length(terminal_reason_code) <= 128
         AND (terminal_error IS NULL OR jsonb_typeof(terminal_error) = 'object')
         AND (
             (entrypoint_kind = 'task' AND terminal_actor_input_sequence IS NULL)
             OR
             (entrypoint_kind = 'actor'
              AND (terminal_outcome <> 'succeeded'
                   OR terminal_actor_input_sequence IS NOT NULL))
         ))
    )
);

CREATE TABLE actor_records (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    environment_id UUID NOT NULL,
    actor_id UUID NOT NULL,
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
    UNIQUE (actor_id, direction, sequence),
    UNIQUE (actor_id, id),
    UNIQUE (id, actor_id, direction),
    FOREIGN KEY (environment_id, actor_id)
        REFERENCES actors(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, source_run_id)
        REFERENCES runs(environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (producer_run_id, producer_attempt_number)
        REFERENCES run_attempts(run_id, number)
        ON DELETE RESTRICT,
    FOREIGN KEY (actor_id, producer_run_id)
        REFERENCES runs(actor_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, claim_id)
        REFERENCES idempotency_claims(environment_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT actor_records_data_size_check
        CHECK (octet_length(data::text) <= 1048576),
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

CREATE UNIQUE INDEX actor_records_claim_uidx
    ON actor_records (actor_id, direction, claim_id)
    WHERE claim_id IS NOT NULL;

CREATE INDEX actor_records_claim_idx
    ON actor_records (claim_id)
    WHERE claim_id IS NOT NULL;

CREATE INDEX actor_records_input_sequence_idx
    ON actor_records (actor_id, sequence, id)
    WHERE direction = 'input';

CREATE INDEX actor_records_output_sequence_idx
    ON actor_records (actor_id, sequence, id)
    WHERE direction = 'output';

ALTER TABLE runs
    ADD CONSTRAINT runs_current_attempt_fk
    FOREIGN KEY (id, current_attempt_number)
    REFERENCES run_attempts(run_id, number)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE actors
    ADD CONSTRAINT actors_current_run_fk
    FOREIGN KEY (id, workspace_id, current_run_id)
    REFERENCES runs(actor_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE actors
    ADD CONSTRAINT actors_failure_run_fk
    FOREIGN KEY (id, failure_run_id)
    REFERENCES runs(actor_id, id)
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
    ON runs (actor_id)
    WHERE actor_id IS NOT NULL
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    state workspace_mount_state NOT NULL DEFAULT 'mounting',
    request JSONB NOT NULL DEFAULT '{}'::jsonb,
    dirty_generation BIGINT NOT NULL DEFAULT 0 CHECK (dirty_generation >= 0),
    fencing_generation BIGINT NOT NULL DEFAULT 1 CHECK (fencing_generation > 0),
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    CHECK (jsonb_typeof(request) = 'object' AND octet_length(request::text) <= 16384),
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
    CHECK (terminal_error IS NULL OR (jsonb_typeof(terminal_error) = 'object' AND octet_length(terminal_error::text) <= 16384))
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    state workspace_lease_state NOT NULL DEFAULT 'active',
    owner_run_lease_id UUID,
    owner_process_id UUID,
    base_version_id UUID NOT NULL,
    ownership_generation BIGINT NOT NULL CHECK (ownership_generation > 0),
    writer_generation BIGINT NOT NULL CHECK (writer_generation > 0),
    mount_fencing_generation BIGINT NOT NULL CHECK (mount_fencing_generation > 0),
    fencing_key_fingerprint BYTEA NOT NULL CHECK (
        octet_length(fencing_key_fingerprint) = 32
    ),
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, workspace_mount_id, mount_fencing_generation)
        REFERENCES workspace_mounts(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_id, id, fencing_generation)
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
    CHECK (terminal_error IS NULL OR (jsonb_typeof(terminal_error) = 'object' AND octet_length(terminal_error::text) <= 16384))
);

CREATE TABLE workspace_processes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    region_id TEXT,
    worker_group_id TEXT,
    worker_instance_id UUID,
    worker_epoch BIGINT CHECK (worker_epoch IS NULL OR worker_epoch > 0),
    runtime_instance_id UUID,
    workspace_mount_id UUID,
    kind TEXT NOT NULL CHECK (kind IN ('exec', 'pty')),
    state workspace_process_state NOT NULL DEFAULT 'pending',
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    request JSONB NOT NULL,
    claim_id UUID,
    runtime_process_id TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    signal TEXT NOT NULL DEFAULT '',
    pty_cols INTEGER CHECK (pty_cols IS NULL OR pty_cols > 0),
    pty_rows INTEGER CHECK (pty_rows IS NULL OR pty_rows > 0),
    pending_pty_cols INTEGER CHECK (pending_pty_cols IS NULL OR pending_pty_cols > 0),
    pending_pty_rows INTEGER CHECK (pending_pty_rows IS NULL OR pending_pty_rows > 0),
    resize_generation BIGINT CHECK (resize_generation IS NULL OR resize_generation >= 0),
    pending_resize_generation BIGINT CHECK (pending_resize_generation IS NULL OR pending_resize_generation > 0),
    stdout_cursor BIGINT CHECK (stdout_cursor IS NULL OR stdout_cursor >= 0),
    stderr_cursor BIGINT CHECK (stderr_cursor IS NULL OR stderr_cursor >= 0),
    stdin_cursor BIGINT CHECK (stdin_cursor IS NULL OR stdin_cursor >= 0),
    stdin_delivered_cursor BIGINT CHECK (
        stdin_delivered_cursor IS NULL
        OR (stdin_delivered_cursor >= 0 AND stdin_delivered_cursor <= stdin_cursor)
    ),
    stdin_closed_at TIMESTAMPTZ,
    input_cursor BIGINT CHECK (input_cursor IS NULL OR input_cursor >= 0),
    input_delivered_cursor BIGINT CHECK (
        input_delivered_cursor IS NULL
        OR (input_delivered_cursor >= 0 AND input_delivered_cursor <= input_cursor)
    ),
    output_cursor BIGINT CHECK (output_cursor IS NULL OR output_cursor >= 0),
    created_by_subject_type TEXT NOT NULL DEFAULT '',
    created_by_subject_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    exited_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    terminal_reason_code TEXT,
    error JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (jsonb_typeof(request) = 'object' AND octet_length(request::text) <= 16384),
    CHECK (
        (kind = 'exec'
         AND stdout_cursor IS NOT NULL
         AND stderr_cursor IS NOT NULL
         AND stdin_cursor IS NOT NULL
         AND stdin_delivered_cursor IS NOT NULL
         AND input_cursor IS NULL
         AND input_delivered_cursor IS NULL
         AND output_cursor IS NULL
         AND pty_cols IS NULL
         AND pty_rows IS NULL
         AND pending_pty_cols IS NULL
         AND pending_pty_rows IS NULL
         AND resize_generation IS NULL
         AND pending_resize_generation IS NULL)
        OR
        (kind = 'pty'
         AND stdout_cursor IS NULL
         AND stderr_cursor IS NULL
         AND stdin_cursor IS NULL
         AND stdin_delivered_cursor IS NULL
         AND stdin_closed_at IS NULL
         AND input_cursor IS NOT NULL
         AND input_delivered_cursor IS NOT NULL
         AND output_cursor IS NOT NULL
         AND pty_cols IS NOT NULL
         AND pty_rows IS NOT NULL
         AND resize_generation IS NOT NULL)
    ),
    CHECK (
        (pending_pty_cols IS NULL
         AND pending_pty_rows IS NULL
         AND pending_resize_generation IS NULL)
        OR
        (pending_pty_cols IS NOT NULL
         AND pending_pty_rows IS NOT NULL
         AND pending_resize_generation IS NOT NULL
         AND pending_resize_generation = resize_generation)
    ),
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
        OR
        (state IN ('starting', 'running', 'exit_requested', 'exited', 'lost', 'failed')
         AND region_id IS NOT NULL)
        OR
        (state = 'cancelled')
    ),
    CHECK (
        (state IN ('pending', 'starting', 'running', 'exit_requested')
         AND terminal_at IS NULL
         AND terminal_reason_code IS NULL
         AND error IS NULL)
        OR (
            state IN ('exited', 'cancelled', 'lost', 'failed')
            AND terminal_at IS NOT NULL
            AND terminal_reason_code IS NOT NULL
            AND btrim(terminal_reason_code) <> ''
            AND octet_length(terminal_reason_code) <= 128
        )
    ),
    CHECK (state <> 'cancelled' OR num_nonnulls(region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id, workspace_mount_id) IN (0, 6)),
    CHECK (state <> 'exited' OR exited_at IS NOT NULL),
    CHECK (error IS NULL OR (jsonb_typeof(error) = 'object' AND octet_length(error::text) <= 16384)),
    UNIQUE (org_id, id),
    UNIQUE (workspace_id, id),
    UNIQUE (id, workspace_id, runtime_instance_id),
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, id, kind),
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^wsv_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    parent_version_id UUID,
    artifact_id UUID,
    artifact_kind artifact_kind,
    kind workspace_version_kind NOT NULL DEFAULT 'user',
    content_digest TEXT NOT NULL CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    entry_count INTEGER NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    state workspace_version_state NOT NULL DEFAULT 'private',
    source_workspace_lease_id UUID,
    ownership_generation BIGINT NOT NULL CHECK (ownership_generation >= 0),
    writer_generation BIGINT NOT NULL CHECK (writer_generation >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    discarded_at TIMESTAMPTZ,
    UNIQUE (org_id, project_id, environment_id, id),
    UNIQUE (workspace_id, id),
    UNIQUE (org_id, workspace_id, id),
    UNIQUE (org_id, project_id, environment_id, workspace_id, id),
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id)
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, materialized_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspace_leases
    ADD CONSTRAINT workspace_leases_base_version_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, base_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_head_version_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, id, head_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE runs
    ADD CONSTRAINT runs_base_workspace_version_fk
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, base_workspace_version_id)
    REFERENCES workspace_versions(org_id, project_id, environment_id, workspace_id, id)
    ON DELETE RESTRICT;

ALTER TABLE run_attempts
    ADD CONSTRAINT run_attempts_base_workspace_version_fk
    FOREIGN KEY (workspace_id, base_workspace_version_id)
    REFERENCES workspace_versions(workspace_id, id)
    ON DELETE RESTRICT;

CREATE TABLE workspace_process_records (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    environment_id UUID NOT NULL,
    process_id UUID NOT NULL,
    process_kind TEXT NOT NULL CHECK (process_kind IN ('exec', 'pty')),
    direction TEXT NOT NULL CHECK (direction IN ('input', 'output')),
    stream TEXT NOT NULL CHECK (stream IN ('stdin', 'stdout', 'stderr', 'pty_input', 'pty_output')),
    offset_start BIGINT NOT NULL CHECK (offset_start >= 0),
    offset_end BIGINT NOT NULL CHECK (offset_end > offset_start),
    data BYTEA,
    artifact_id UUID,
    artifact_kind artifact_kind,
    artifact_digest TEXT,
    content_digest BYTEA NOT NULL CHECK (octet_length(content_digest) = 32),
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload_expires_at TIMESTAMPTZ,
    payload_collected_at TIMESTAMPTZ,
    UNIQUE (process_id, stream, offset_start),
    FOREIGN KEY (environment_id, process_id, process_kind)
        REFERENCES workspace_processes(environment_id, id, kind)
        ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, artifact_id, artifact_kind, artifact_digest, size_bytes)
        REFERENCES artifacts(environment_id, id, kind, digest, size_bytes)
        ON DELETE RESTRICT,
    CHECK (offset_end - offset_start = size_bytes),
    CHECK (data IS NULL OR octet_length(data) = size_bytes),
    CHECK (
        (process_kind = 'exec' AND direction = 'input' AND stream = 'stdin')
        OR
        (process_kind = 'exec' AND direction = 'output' AND stream IN ('stdout', 'stderr'))
        OR
        (process_kind = 'pty' AND direction = 'input' AND stream = 'pty_input')
        OR
        (process_kind = 'pty' AND direction = 'output' AND stream = 'pty_output')
    ),
    CHECK (
        (payload_collected_at IS NULL
         AND (
             (data IS NOT NULL
              AND artifact_id IS NULL
              AND artifact_kind IS NULL
              AND artifact_digest IS NULL)
             OR
             (data IS NULL
              AND artifact_id IS NOT NULL
              AND artifact_kind = 'workspace_process_record'
              AND artifact_digest = 'sha256:' || encode(content_digest, 'hex'))
         ))
        OR
        (payload_collected_at IS NOT NULL
         AND data IS NULL
         AND artifact_id IS NULL
         AND artifact_kind IS NULL
         AND artifact_digest IS NULL)
    ),
    CHECK (payload_expires_at IS NULL OR payload_expires_at >= created_at),
    CHECK (payload_collected_at IS NULL OR payload_expires_at IS NOT NULL)
);

CREATE INDEX workspace_process_records_payload_gc_idx
    ON workspace_process_records (payload_expires_at, id)
    WHERE payload_collected_at IS NULL AND payload_expires_at IS NOT NULL;

CREATE TABLE secret_resolutions (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    public_id TEXT NOT NULL UNIQUE CHECK (public_id ~ '^tok_[a-z2-7]{26}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    state token_state NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    callback_key_id TEXT NOT NULL DEFAULT '',
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
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    credential_key_id TEXT NOT NULL,
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
    CHECK (expires_at > created_at),
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
    token_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, project_id, environment_id, id),
    CHECK (scope_type = 'token.complete'),
    FOREIGN KEY (org_id, project_id, environment_id, public_access_token_id)
        REFERENCES public_access_tokens(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, environment_id, token_id)
        REFERENCES tokens(org_id, project_id, environment_id, id)
        ON DELETE CASCADE,
    UNIQUE (public_access_token_id, scope_type),
    UNIQUE (token_id, scope_type)
);

CREATE TABLE outbox_messages (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    lane TEXT NOT NULL CHECK (btrim(lane) <> '' AND octet_length(lane) <= 128),
    topic TEXT NOT NULL CHECK (btrim(topic) <> '' AND octet_length(topic) <= 128),
    partition_key TEXT NOT NULL CHECK (btrim(partition_key) <> '' AND octet_length(partition_key) <= 512),
    payload JSONB NOT NULL CHECK (
        jsonb_typeof(payload) = 'object'
        AND octet_length(payload::text) <= 1048576
    ),
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'claimed', 'delivered', 'dead_lettered')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_by TEXT CHECK (claimed_by IS NULL OR btrim(claimed_by) <> ''),
    claim_expires_at TIMESTAMPTZ,
    last_error JSONB,
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
    ),
    CHECK (
        last_error IS NULL
        OR (jsonb_typeof(last_error) = 'object' AND octet_length(last_error::text) <= 16384)
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    network_slot_id UUID NOT NULL,
    network_slot_generation BIGINT NOT NULL CHECK (network_slot_generation > 0),
    runtime_identity_id TEXT NOT NULL CHECK (btrim(runtime_identity_id) <> ''),
    worker_protocol_version TEXT NOT NULL DEFAULT 'helmr.worker.v0' CHECK (worker_protocol_version = 'helmr.worker.v0'),
    requested_cpu_millis BIGINT NOT NULL CHECK (requested_cpu_millis > 0),
    requested_memory_bytes BIGINT NOT NULL CHECK (requested_memory_bytes > 0),
    requested_workload_disk_bytes BIGINT NOT NULL CHECK (requested_workload_disk_bytes >= 0),
    requested_scratch_bytes BIGINT NOT NULL CHECK (requested_scratch_bytes >= 0),
    requested_execution_slots INTEGER NOT NULL DEFAULT 1 CHECK (requested_execution_slots > 0),
    trace_id TEXT,
    span_id TEXT,
    parent_span_id TEXT,
    traceparent TEXT,
    state run_lease_state NOT NULL DEFAULT 'assigned',
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
    FOREIGN KEY (org_id, project_id, environment_id, workspace_id, region_id)
        REFERENCES workspaces(org_id, project_id, environment_id, id, region_id)
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
    CHECK (terminal_error IS NULL OR (jsonb_typeof(terminal_error) = 'object' AND octet_length(terminal_error::text) <= 16384)),
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    state run_checkpoint_state NOT NULL DEFAULT 'creating',
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
        AND octet_length(restore_manifest::text) <= 65536
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
    source_type TEXT GENERATED ALWAYS AS (
        CASE WHEN run_lease_id IS NOT NULL
             THEN 'run_lease'::text
             ELSE 'deployment_build_lease'::text
        END
    ) STORED NOT NULL,
    source_id UUID GENERATED ALWAYS AS (
        COALESCE(run_lease_id, deployment_build_lease_id)
    ) STORED NOT NULL,
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
    CHECK (jsonb_typeof(details) = 'object' AND octet_length(details::text) <= 16384)
);

ALTER TABLE telemetry_outbox
    ADD CONSTRAINT telemetry_outbox_meter_event_id_fkey
    FOREIGN KEY (meter_event_id)
    REFERENCES meter_events(id)
    ON DELETE RESTRICT;

CREATE UNIQUE INDEX meter_events_idempotency_idx
    ON meter_events (org_id, source_type, source_id, meter, idempotency_key);

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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    environment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    kind wait_kind NOT NULL,
    condition_state wait_state NOT NULL DEFAULT 'pending',
    due_at TIMESTAMPTZ,
    timeout_at TIMESTAMPTZ,
    idle_timeout_ms BIGINT CHECK (idle_timeout_ms IS NULL OR idle_timeout_ms > 0),
    token_id UUID,
    child_run_id UUID,
    child_parent_owned BOOLEAN,
    child_target_declared_id TEXT,
    child_claim_id UUID,
    child_request JSONB,
    actor_id UUID,
    after_input_sequence BIGINT CHECK (after_input_sequence IS NULL OR after_input_sequence >= 0),
    condition_result JSONB,
    condition_error JSONB,
    condition_terminal_at TIMESTAMPTZ,
    condition_reason_code TEXT,
    completed_actor_record_id UUID,
    completed_actor_record_direction TEXT GENERATED ALWAYS AS ('input'::text) STORED,
    suspension_state run_wait_state NOT NULL DEFAULT 'hot',
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
    FOREIGN KEY (actor_id, workspace_id, run_id)
        REFERENCES runs(actor_id, workspace_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (completed_actor_record_id, actor_id, completed_actor_record_direction)
        REFERENCES actor_records(id, actor_id, direction)
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
    CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 65536),
    CHECK (cardinality(tags) <= 32),
    CHECK (condition_error IS NULL OR jsonb_typeof(condition_error) = 'object'),
    CHECK (suspension_error IS NULL OR jsonb_typeof(suspension_error) = 'object'),
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
         AND actor_id IS NULL
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
         AND actor_id IS NULL
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
         AND actor_id IS NULL
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
         AND actor_id IS NOT NULL
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
    id UUID PRIMARY KEY DEFAULT uuidv7(),
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
    network_policy JSONB NOT NULL,
    reserved_cpu_millis BIGINT NOT NULL CHECK (reserved_cpu_millis > 0),
    reserved_memory_bytes BIGINT NOT NULL CHECK (reserved_memory_bytes > 0),
    reserved_workload_disk_bytes BIGINT NOT NULL CHECK (reserved_workload_disk_bytes >= 0),
    reserved_scratch_bytes BIGINT NOT NULL CHECK (reserved_scratch_bytes >= 0),
    reserved_execution_slots INTEGER NOT NULL CHECK (reserved_execution_slots > 0),
    workspace_id UUID NOT NULL,
    program_deployment_id UUID,
    restore_checkpoint_id UUID,
    reserved_run_id UUID,
    reserved_attempt_number INTEGER CHECK (reserved_attempt_number IS NULL OR reserved_attempt_number > 0),
    reserved_process_id UUID,
    reserved_workspace_version_id UUID,
    reservation_expires_at TIMESTAMPTZ,
    desired_state runtime_desired_state NOT NULL DEFAULT 'ready',
    desired_version BIGINT NOT NULL DEFAULT 1 CHECK (desired_version > 0),
    desired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    desired_reason TEXT NOT NULL CHECK (btrim(desired_reason) <> ''),
    observed_state runtime_observed_state NOT NULL DEFAULT 'allocated',
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
    CHECK (jsonb_typeof(network_policy) = 'object' AND octet_length(network_policy::text) <= 16384),
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
    CHECK (terminal_error IS NULL OR (jsonb_typeof(terminal_error) = 'object' AND octet_length(terminal_error::text) <= 16384))
);

CREATE TABLE worker_network_slots (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    worker_group_id TEXT NOT NULL,
    worker_instance_id UUID NOT NULL,
    worker_epoch BIGINT NOT NULL CHECK (worker_epoch > 0),
    slot_name TEXT NOT NULL CHECK (btrim(slot_name) <> ''),
    generation BIGINT NOT NULL CHECK (generation > 0),
    state worker_network_slot_state NOT NULL DEFAULT 'available',
    runtime_instance_id UUID,
    host_interface_name TEXT,
    guest_address INET,
    gateway_address INET,
    subnet CIDR,
    tap_name TEXT,
    netns_name TEXT,
    guest_mac MACADDR,
    assigned_at TIMESTAMPTZ,
    reclaiming_at TIMESTAMPTZ,
    quarantined_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    reclaimed_at TIMESTAMPTZ,
    reclaim_evidence JSONB,
    state_reason_code TEXT,
    state_error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (worker_instance_id, worker_epoch, slot_name),
    UNIQUE (worker_instance_id, worker_epoch, id, generation),
    UNIQUE (id, generation, runtime_instance_id),
    UNIQUE (worker_instance_id, worker_epoch, guest_address),
    FOREIGN KEY (worker_instance_id, worker_group_id)
        REFERENCES worker_instances(id, worker_group_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id)
        REFERENCES runtime_instances(worker_group_id, worker_instance_id, worker_epoch, id)
        ON DELETE RESTRICT,
    CHECK (reclaim_evidence IS NULL OR (jsonb_typeof(reclaim_evidence) = 'object' AND octet_length(reclaim_evidence::text) <= 16384)),
    CHECK (state_error IS NULL OR (jsonb_typeof(state_error) = 'object' AND octet_length(state_error::text) <= 16384)),
    CHECK (state_reason_code IS NULL OR (btrim(state_reason_code) <> '' AND octet_length(state_reason_code) <= 128)),
    CHECK (host_interface_name IS NULL OR btrim(host_interface_name) <> ''),
    CHECK (tap_name IS NULL OR btrim(tap_name) <> ''),
    CHECK (netns_name IS NULL OR btrim(netns_name) <> ''),
    CHECK (
        (state = 'available' AND runtime_instance_id IS NULL AND host_interface_name IS NULL AND guest_address IS NULL AND gateway_address IS NULL AND subnet IS NULL AND tap_name IS NULL AND netns_name IS NULL AND guest_mac IS NULL AND state_reason_code IS NULL AND state_error IS NULL)
        OR (state = 'assigned' AND runtime_instance_id IS NOT NULL AND host_interface_name IS NULL AND guest_address IS NULL AND gateway_address IS NULL AND subnet IS NULL AND tap_name IS NULL AND netns_name IS NULL AND guest_mac IS NULL AND assigned_at IS NOT NULL AND state_reason_code IS NULL AND state_error IS NULL)
        OR (state = 'bound' AND runtime_instance_id IS NOT NULL AND host_interface_name IS NOT NULL AND guest_address IS NOT NULL AND gateway_address IS NOT NULL AND subnet IS NOT NULL AND tap_name IS NOT NULL AND netns_name IS NOT NULL AND guest_mac IS NOT NULL AND assigned_at IS NOT NULL AND state_reason_code IS NULL AND state_error IS NULL)
        OR (state = 'reclaiming' AND runtime_instance_id IS NOT NULL AND reclaiming_at IS NOT NULL)
        OR (state = 'quarantined' AND quarantined_at IS NOT NULL AND state_reason_code IS NOT NULL)
        OR (state = 'lost' AND lost_at IS NOT NULL AND state_reason_code IS NOT NULL)
    ),
    CHECK (generation = 1 OR state <> 'available' OR (reclaimed_at IS NOT NULL AND reclaim_evidence IS NOT NULL))
);

CREATE UNIQUE INDEX network_slots_runtime_active_uidx
    ON worker_network_slots (runtime_instance_id)
    WHERE state IN ('assigned', 'bound', 'reclaiming');

CREATE INDEX network_slots_worker_replay_idx
    ON worker_network_slots (worker_instance_id, worker_epoch, state, slot_name);

CREATE INDEX network_slots_reclaim_idx
    ON worker_network_slots (state, updated_at, id)
    WHERE state IN ('reclaiming', 'quarantined', 'lost');

ALTER TABLE run_leases
    ADD CONSTRAINT run_leases_runtime_instance_id_fkey
    FOREIGN KEY (org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, runtime_instance_id)
    REFERENCES runtime_instances(org_id, project_id, environment_id, region_id, worker_group_id, worker_instance_id, worker_epoch, id)
    ON DELETE RESTRICT;

ALTER TABLE run_leases
    ADD CONSTRAINT run_leases_network_slot_id_fkey
    FOREIGN KEY (network_slot_id)
    REFERENCES worker_network_slots(id)
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
    ON workspaces(region_id, org_id, project_id, environment_id, id);
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
    ON deployment_promotions(org_id, project_id, environment_id, deployment_id);
CREATE INDEX deployment_promotions_environment_created_idx
    ON deployment_promotions(org_id, project_id, environment_id, created_at DESC);
CREATE INDEX deployments_build_region_status_idx
    ON deployments(build_region_id, status, created_at)
    WHERE status IN ('queued', 'building');
CREATE INDEX artifacts_scope_kind_created_idx
    ON artifacts(org_id, project_id, environment_id, kind, created_at DESC);
CREATE INDEX artifacts_digest_idx
    ON artifacts(digest);
CREATE UNIQUE INDEX artifacts_runtime_substrate_digest_uidx
    ON artifacts(org_id, project_id, environment_id, digest, kind)
    WHERE kind = 'runtime_substrate';
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
CREATE INDEX run_checkpoints_run_state_idx ON run_checkpoints(run_id, state, created_at DESC);
CREATE INDEX run_checkpoint_artifacts_role_idx ON run_checkpoint_artifacts(run_checkpoint_id, role, ordinal);
CREATE INDEX tokens_scope_state_idx ON tokens(org_id, project_id, environment_id, state, created_at DESC);
CREATE INDEX tokens_expiry_pending_idx ON tokens(expires_at, id)
    WHERE state = 'pending';
CREATE INDEX tokens_callback_fingerprint_pending_idx ON tokens(callback_key_id, callback_secret_fingerprint)
    WHERE state = 'pending' AND callback_key_id <> '' AND callback_secret_fingerprint <> '';
CREATE INDEX run_waits_run_state_idx
    ON run_waits(run_id, suspension_state, created_at DESC);
CREATE INDEX workspaces_state_idx ON workspaces(org_id, project_id, environment_id, state, updated_at DESC);
CREATE INDEX workspaces_tags_idx ON workspaces USING GIN (tags);
CREATE UNIQUE INDEX workspaces_environment_key_uidx ON workspaces(environment_id, key)
    WHERE key IS NOT NULL;
CREATE INDEX workspaces_create_idempotency_expiry_idx ON workspaces(org_id, project_id, environment_id, create_idempotency_expires_at)
    WHERE create_idempotency_key <> '';
CREATE UNIQUE INDEX workspaces_create_idempotency_idx ON workspaces(org_id, project_id, environment_id, create_idempotency_key)
    WHERE create_idempotency_key <> '';
CREATE INDEX workspace_versions_workspace_created_idx ON workspace_versions(org_id, workspace_id, created_at DESC);
CREATE INDEX public_access_tokens_scope_expiry_idx ON public_access_tokens(org_id, project_id, environment_id, expires_at)
    WHERE state = 'active';
CREATE INDEX public_access_tokens_expiry_active_idx ON public_access_tokens(expires_at, id)
    WHERE state = 'active';
CREATE INDEX public_access_token_scopes_token_idx ON public_access_token_scopes(org_id, project_id, environment_id, token_id, scope_type)
    WHERE token_id IS NOT NULL;

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
