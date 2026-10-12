CREATE TYPE org_member_role AS ENUM (
    'owner',
    'admin',
    'developer',
    'viewer'
);

CREATE TYPE magic_link_purpose AS ENUM (
    'login',
    'invite_accept'
);

CREATE TABLE organizations (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    slug TEXT NOT NULL UNIQUE CHECK (btrim(slug) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE regions (
    id TEXT PRIMARY KEY CHECK (
        id = btrim(id)
        AND octet_length(id) BETWEEN 1 AND 255
        AND id !~ '[[:cntrl:]]'
        AND id !~ '(^[[:space:]])|([[:space:]]$)'
    ),
    display_name TEXT NOT NULL CHECK (btrim(display_name) <> ''),
    location TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id UUID PRIMARY KEY,
    display_name TEXT NOT NULL CHECK (btrim(display_name) <> ''),
    profile_image_url TEXT CHECK (profile_image_url IS NULL OR btrim(profile_image_url) <> ''),
    primary_email TEXT,
    admin BOOLEAN NOT NULL DEFAULT false,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_primary_email_lower_idx
    ON users (lower(primary_email))
    WHERE primary_email IS NOT NULL AND disabled_at IS NULL;

CREATE TABLE auth_identities (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    provider TEXT NOT NULL CHECK (btrim(provider) <> ''),
    subject TEXT NOT NULL CHECK (btrim(subject) <> ''),
    email TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    UNIQUE (provider, subject),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE org_members (
    org_id UUID NOT NULL,
    user_id UUID NOT NULL,
    role org_member_role NOT NULL,
    display_name TEXT,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id),
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX org_members_user_active_idx ON org_members(user_id, org_id) WHERE disabled_at IS NULL;

CREATE TABLE projects (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    default_region_id TEXT NOT NULL,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, slug),
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
    FOREIGN KEY (default_region_id) REFERENCES regions(id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX projects_one_default ON projects(org_id) WHERE is_default;

CREATE TABLE environments (
    history_retention_mode text NOT NULL CHECK (history_retention_mode IN ('duration','until_environment_deletion')),
    history_retention_seconds bigint,
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    slug TEXT NOT NULL CHECK (btrim(slug) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    color_hex TEXT NOT NULL CHECK (color_hex ~ '^#[0-9A-Fa-f]{6}$'),
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    current_deployment_id UUID,
    retired_at timestamptz,
    max_outstanding_admissions bigint NOT NULL CHECK (max_outstanding_admissions > 0),
    max_causal_depth integer NOT NULL CHECK (max_causal_depth > 0),
    max_resident_computers bigint NOT NULL CHECK (max_resident_computers>0),
    max_cpu_millis bigint NOT NULL CHECK (max_cpu_millis>0),
    max_memory_bytes bigint NOT NULL CHECK (max_memory_bytes>0),
    max_reserved_storage_bytes bigint NOT NULL CHECK (max_reserved_storage_bytes>0),
    preparation_timeout_ms bigint NOT NULL CHECK (preparation_timeout_ms>0),
    admission_rate_per_second bigint NOT NULL CHECK (admission_rate_per_second>0),
    admission_burst bigint NOT NULL CHECK (admission_burst>0),
    admission_tokens numeric NOT NULL CHECK (admission_tokens>=0),
    admission_refilled_at timestamptz NOT NULL,
    CHECK ((history_retention_mode='duration' AND history_retention_seconds IS NOT NULL AND history_retention_seconds>0)
        OR (history_retention_mode='until_environment_deletion' AND history_retention_seconds IS NULL)),
    UNIQUE (org_id, project_id, id),
    UNIQUE (org_id, project_id, slug),
    FOREIGN KEY (org_id, project_id)
        REFERENCES projects(org_id, id)
        ON DELETE CASCADE,
    -- Creation supplies the operating limits with the row; there is no unlimited
    -- or unconfigured Environment.
    CHECK (admission_tokens<=admission_burst)
);

CREATE UNIQUE INDEX environments_one_default ON environments(org_id,project_id) WHERE is_default;

CREATE INDEX environments_current_deployment_idx
    ON environments(org_id, project_id, current_deployment_id)
    WHERE current_deployment_id IS NOT NULL;

CREATE TABLE login_sessions (
    id UUID PRIMARY KEY,
    org_id UUID,
    user_id UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE SET NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX login_sessions_user_active_idx ON login_sessions(user_id) WHERE revoked_at IS NULL;

CREATE INDEX login_sessions_expiry_active_idx ON login_sessions(expires_at) WHERE revoked_at IS NULL;

CREATE TABLE invitations (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
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
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
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

CREATE UNIQUE INDEX invitations_pending_invitee_idx ON invitations(org_id, invitee_email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX invitations_email_lookup_idx ON invitations(org_id, invitee_email);

CREATE TABLE magic_links (
    id UUID PRIMARY KEY,
    purpose magic_link_purpose NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    email TEXT NOT NULL,
    org_id UUID,
    invitation_id UUID,
    redirect_after TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    sent_at TIMESTAMPTZ,
    delivery_failed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_by_user_id UUID,
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
    FOREIGN KEY (invitation_id) REFERENCES invitations(id) ON DELETE CASCADE,
    FOREIGN KEY (consumed_by_user_id) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX magic_links_active_token_idx ON magic_links(token_hash)
    WHERE sent_at IS NOT NULL AND consumed_at IS NULL AND revoked_at IS NULL;

CREATE INDEX magic_links_email_purpose_recent_idx ON magic_links(email, purpose, created_at DESC)
    WHERE delivery_failed_at IS NULL;

CREATE INDEX magic_links_invitation_active_idx ON magic_links(invitation_id, created_at DESC)
    WHERE invitation_id IS NOT NULL AND sent_at IS NOT NULL AND consumed_at IS NULL AND revoked_at IS NULL;

CREATE TABLE api_keys (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    created_by_user_id UUID,
    role org_member_role NOT NULL,
    permissions TEXT[] NOT NULL CHECK (cardinality(permissions) > 0),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    key_prefix TEXT NOT NULL CHECK (btrim(key_prefix) <> ''),
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
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

CREATE UNIQUE INDEX api_keys_scope_active_name_idx ON api_keys(org_id, project_id, environment_id, name) WHERE revoked_at IS NULL;

CREATE INDEX api_keys_org_active_idx ON api_keys(org_id, created_at DESC) WHERE revoked_at IS NULL;

CREATE INDEX api_keys_scope_created_idx ON api_keys(org_id, project_id, environment_id, created_at DESC, id DESC);

CREATE TABLE device_codes (
    id UUID PRIMARY KEY,
    org_id UUID,
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
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, decided_by_user_id)
        REFERENCES org_members(org_id, user_id)
        ON DELETE SET NULL (decided_by_user_id)
);

CREATE INDEX device_codes_pending_expiry_idx ON device_codes(expires_at) WHERE status = 'pending';

CREATE TABLE secrets (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    current_version_id UUID,
    revocation_generation BIGINT NOT NULL DEFAULT 0 CHECK (revocation_generation >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, name),
    CONSTRAINT secrets_lifecycle_check CHECK (
        (status = 'active' AND current_version_id IS NOT NULL AND revoked_at IS NULL)
        OR
        (status = 'revoked' AND current_version_id IS NULL AND revoked_at IS NOT NULL)
    ),
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE RESTRICT
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

CREATE TABLE cas_blobs (
    digest TEXT PRIMARY KEY CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    retired_at TIMESTAMPTZ,
    not_retired BOOLEAN GENERATED ALWAYS AS (retired_at IS NULL) STORED,
    next_reclaim_at TIMESTAMPTZ,
    last_reclaim_error TEXT,
    UNIQUE (digest, size_bytes, not_retired),
    CHECK ((retired_at IS NULL) = (next_reclaim_at IS NULL))
);

CREATE TABLE cas_upload_reclaims (
    digest TEXT NOT NULL,
    upload_id TEXT NOT NULL CHECK (upload_id <> ''),
    next_reclaim_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (digest, upload_id),
    FOREIGN KEY (digest) REFERENCES cas_blobs(digest)
);

CREATE TABLE cas_objects (
    org_id UUID NOT NULL,
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    availability_required BOOLEAN GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (org_id, digest),
    CONSTRAINT cas_objects_descriptor_key
        UNIQUE (org_id, digest, size_bytes, media_type),
    FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired)
);

CREATE TABLE worker_group_tokens (
    id UUID PRIMARY KEY,
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE worker_groups (
    id UUID PRIMARY KEY,
    token_id UUID NOT NULL UNIQUE,
    region_id TEXT NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'draining', 'disabled')),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    primary_pool_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (region_id, name),
    FOREIGN KEY (token_id) REFERENCES worker_group_tokens(id) ON DELETE RESTRICT,
    FOREIGN KEY (region_id) REFERENCES regions(id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX worker_groups_one_active_per_region_idx
    ON worker_groups(region_id) WHERE status IN ('active','paused');

CREATE INDEX worker_groups_active_dispatch_idx
    ON worker_groups (region_id, id)
    WHERE status = 'active';

CREATE TABLE vm_platforms (
    id TEXT PRIMARY KEY CHECK (id ~ '^sha256:[0-9a-f]{64}$'),
    descriptor_digest TEXT NOT NULL CHECK (descriptor_digest ~ '^sha256:[0-9a-f]{64}$'),
    firecracker_digest TEXT NOT NULL CHECK (firecracker_digest ~ '^sha256:[0-9a-f]{64}$'),
    firecracker_version TEXT NOT NULL CHECK (firecracker_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
    snapshot_format_version TEXT NOT NULL CHECK (snapshot_format_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
    host_kernel_release TEXT NOT NULL CHECK (btrim(host_kernel_release) <> '' AND octet_length(host_kernel_release) <= 255),
    cpu_template_digest TEXT CHECK (cpu_template_digest IS NULL OR cpu_template_digest ~ '^sha256:[0-9a-f]{64}$'),
    kernel_digest TEXT NOT NULL CHECK (kernel_digest ~ '^sha256:[0-9a-f]{64}$'),
    initramfs_digest TEXT NOT NULL CHECK (initramfs_digest ~ '^sha256:[0-9a-f]{64}$'),
    rootfs_digest TEXT NOT NULL CHECK (rootfs_digest ~ '^sha256:[0-9a-f]{64}$'),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE worker_pools (
    id UUID PRIMARY KEY,
    worker_group_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> '' AND octet_length(name) <= 128),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'active', 'draining', 'disabled')),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    vm_platform_id TEXT,
    capacity_cpu_millis BIGINT CHECK (capacity_cpu_millis IS NULL OR capacity_cpu_millis > 0),
    capacity_memory_bytes BIGINT CHECK (capacity_memory_bytes IS NULL OR capacity_memory_bytes > 0),
    capacity_guest_ephemeral_disk_bytes BIGINT CHECK (capacity_guest_ephemeral_disk_bytes IS NULL OR capacity_guest_ephemeral_disk_bytes > 0),
    per_vm_cpu_millis BIGINT CHECK (per_vm_cpu_millis IS NULL OR per_vm_cpu_millis > 0),
    per_vm_memory_bytes BIGINT CHECK (per_vm_memory_bytes IS NULL OR per_vm_memory_bytes > 0),
    per_vm_guest_ephemeral_disk_bytes BIGINT CHECK (per_vm_guest_ephemeral_disk_bytes IS NULL OR per_vm_guest_ephemeral_disk_bytes > 0),
    max_vm_slots INTEGER CHECK (max_vm_slots IS NULL OR max_vm_slots >= 0),
    sealed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (worker_group_id, id),
    UNIQUE (worker_group_id, name),
    CONSTRAINT worker_pools_seal_shape_check CHECK (
        (status IN ('pending', 'disabled')
         AND sealed_at IS NULL
         AND vm_platform_id IS NULL
         AND capacity_cpu_millis IS NULL
         AND capacity_memory_bytes IS NULL
         AND capacity_guest_ephemeral_disk_bytes IS NULL
         AND per_vm_cpu_millis IS NULL
         AND per_vm_memory_bytes IS NULL
         AND per_vm_guest_ephemeral_disk_bytes IS NULL
		 AND max_vm_slots IS NULL)
        OR
        (status IN ('active', 'draining', 'disabled')
         AND sealed_at IS NOT NULL
         AND vm_platform_id IS NOT NULL
         AND capacity_cpu_millis IS NOT NULL
         AND capacity_memory_bytes IS NOT NULL
         AND capacity_guest_ephemeral_disk_bytes IS NOT NULL
         AND per_vm_cpu_millis IS NOT NULL
         AND per_vm_memory_bytes IS NOT NULL
		 AND per_vm_guest_ephemeral_disk_bytes IS NOT NULL
		 AND max_vm_slots IS NOT NULL)
    ),
    CONSTRAINT worker_pools_cpu_capacity_check CHECK (sealed_at IS NULL OR per_vm_cpu_millis <= capacity_cpu_millis),
    CONSTRAINT worker_pools_memory_capacity_check CHECK (sealed_at IS NULL OR per_vm_memory_bytes <= capacity_memory_bytes),
    CONSTRAINT worker_pools_disk_capacity_check CHECK (sealed_at IS NULL OR per_vm_guest_ephemeral_disk_bytes <= capacity_guest_ephemeral_disk_bytes),
    CONSTRAINT worker_pools_sealed_slots_check CHECK (sealed_at IS NULL OR max_vm_slots > 0),
    FOREIGN KEY (worker_group_id) REFERENCES worker_groups(id) ON DELETE RESTRICT,
    FOREIGN KEY (vm_platform_id) REFERENCES vm_platforms(id) ON DELETE RESTRICT
);

CREATE INDEX worker_pools_active_dispatch_idx
    ON worker_pools (worker_group_id, id)
    WHERE status = 'active';

CREATE TABLE worker_pool_cpu_shapes (
    worker_pool_id UUID NOT NULL,
    vcpu_count INTEGER NOT NULL CHECK (vcpu_count > 0),
    cpu_config_digest TEXT NOT NULL CHECK (cpu_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    PRIMARY KEY (worker_pool_id, vcpu_count),
    FOREIGN KEY (worker_pool_id) REFERENCES worker_pools(id) ON DELETE RESTRICT
);

CREATE TABLE worker_hosts (
    id UUID PRIMARY KEY,
    resource_id TEXT NOT NULL CHECK (btrim(resource_id) <> ''),
    worker_group_id UUID NOT NULL,
    worker_pool_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'registering'
        CHECK (status IN ('registering', 'active', 'draining', 'termination_ready', 'lost')),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    current_epoch BIGINT CHECK (current_epoch IS NULL OR current_epoch > 0),
    current_service_id UUID,
    vm_platform_id TEXT,
    epoch_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (epoch_cpu_millis >= 0),
    epoch_memory_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_memory_bytes >= 0),
    epoch_guest_ephemeral_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (epoch_guest_ephemeral_disk_bytes >= 0),
    per_vm_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_cpu_millis >= 0),
    per_vm_memory_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_memory_bytes >= 0),
    per_vm_guest_ephemeral_disk_bytes BIGINT NOT NULL DEFAULT 0 CHECK (per_vm_guest_ephemeral_disk_bytes >= 0),
    max_vm_slots INTEGER NOT NULL DEFAULT 0 CHECK (max_vm_slots >= 0),
    max_vm_starts INTEGER NOT NULL DEFAULT 0 CHECK (max_vm_starts >= 0),
    cpu_environment JSONB,
    cpu_environment_digest TEXT CHECK (cpu_environment_digest IS NULL OR cpu_environment_digest ~ '^sha256:[0-9a-f]{64}$'),
    observed_at TIMESTAMPTZ,
    admission_paused_reason TEXT,
    epoch_started_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    draining_at TIMESTAMPTZ,
    drain_reason TEXT CONSTRAINT worker_hosts_drain_reason_value_check CHECK (drain_reason IN (
        'replacement', 'capacity_reduction', 'idle_scale_in',
        'shutdown', 'admin', 'incompatible_worker'
    )),
    termination_ready_at TIMESTAMPTZ,
    lost_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, worker_group_id),
    CONSTRAINT worker_hosts_resource_id_length_check CHECK (octet_length(resource_id) <= 512),
    CONSTRAINT worker_hosts_epoch_identity_check CHECK (
        (current_epoch IS NULL AND current_service_id IS NULL AND epoch_started_at IS NULL)
        OR (current_epoch IS NOT NULL AND current_service_id IS NOT NULL AND epoch_started_at IS NOT NULL)
    ),
    CONSTRAINT worker_hosts_live_epoch_check CHECK (status NOT IN ('active', 'draining', 'termination_ready') OR current_epoch IS NOT NULL),
    CONSTRAINT worker_hosts_epoch_shape_check CHECK (
        status <> 'active'
        OR (
            activated_at IS NOT NULL
            AND epoch_cpu_millis > 0
            AND epoch_memory_bytes > 0
            AND per_vm_cpu_millis > 0
            AND per_vm_memory_bytes > 0
			AND per_vm_guest_ephemeral_disk_bytes > 0
        )
    ),
    CONSTRAINT worker_hosts_active_vm_check CHECK (status <> 'active' OR (vm_platform_id IS NOT NULL AND max_vm_slots > 0 AND max_vm_starts > 0)),
    CONSTRAINT worker_hosts_active_cpu_environment_check CHECK (
        status <> 'active'
        OR (
            cpu_environment IS NOT NULL
            AND jsonb_typeof(cpu_environment) = 'object'
            AND pg_column_size(cpu_environment) <= 4096
            AND cpu_environment_digest IS NOT NULL
        )
    ),
    CONSTRAINT worker_hosts_cpu_environment_pair_check CHECK ((cpu_environment IS NULL) = (cpu_environment_digest IS NULL)),
    CONSTRAINT worker_hosts_draining_time_check CHECK (status NOT IN ('draining', 'termination_ready') OR draining_at IS NOT NULL),
    CONSTRAINT worker_hosts_drain_reason_check CHECK ((draining_at IS NULL) = (drain_reason IS NULL)),
    CONSTRAINT worker_hosts_termination_ready_time_check CHECK ((status = 'termination_ready') = (termination_ready_at IS NOT NULL)),
    CONSTRAINT worker_hosts_lost_time_check CHECK ((status = 'lost') = (lost_at IS NOT NULL)),
    FOREIGN KEY (vm_platform_id) REFERENCES vm_platforms(id) ON DELETE RESTRICT,
    FOREIGN KEY (worker_group_id, worker_pool_id)
        REFERENCES worker_pools(worker_group_id, id)
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX worker_hosts_live_resource ON worker_hosts(worker_group_id,resource_id) WHERE status IN ('registering','active','draining');

CREATE INDEX worker_hosts_resource ON worker_hosts(worker_group_id,resource_id);

CREATE INDEX worker_hosts_active_dispatch_idx
    ON worker_hosts (worker_group_id, id)
    WHERE status = 'active';

CREATE TABLE worker_host_secrets (
    id UUID PRIMARY KEY,
    worker_group_id UUID NOT NULL,
    worker_host_id UUID NOT NULL,
    key_prefix TEXT NOT NULL UNIQUE CHECK (btrim(key_prefix) <> ''),
    claim_version BIGINT NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    expires_at TIMESTAMPTZ,
    secret_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(secret_hash) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    CONSTRAINT worker_host_secrets_worker_scope_fkey FOREIGN KEY (worker_host_id, worker_group_id)
        REFERENCES worker_hosts(id, worker_group_id)
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX worker_host_secrets_one_active_idx
    ON worker_host_secrets (worker_host_id)
    WHERE revoked_at IS NULL;

CREATE TABLE deployments (
    environment_id uuid NOT NULL REFERENCES environments(id),
    id uuid NOT NULL,
    bundle_digest text NOT NULL CHECK (bundle_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    execution_revoked_at timestamptz,
    PRIMARY KEY (environment_id, id),
    UNIQUE (environment_id,bundle_digest)
);

-- Immutable executable dependencies remain owned independently of Computer
-- graphs that happen to share their physical bytes.
CREATE TABLE deployment_objects (
    environment_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    org_id uuid NOT NULL,
    project_id uuid NOT NULL,
    digest text NOT NULL,
    size_bytes bigint NOT NULL,
    media_type text NOT NULL,
    PRIMARY KEY (environment_id,deployment_id,digest),
    FOREIGN KEY (environment_id,deployment_id) REFERENCES deployments(environment_id,id),
    FOREIGN KEY (org_id,project_id,environment_id) REFERENCES environments(org_id,project_id,id),
    FOREIGN KEY (org_id,digest,size_bytes,media_type) REFERENCES cas_objects(org_id,digest,size_bytes,media_type)
);

CREATE INDEX deployment_objects_cas ON deployment_objects(org_id,digest);

-- Preparation input identity is immutable. Scheduling policy is derived from
-- promoted definitions and never changes the input specification.
CREATE TABLE image_specs (
    environment_id uuid NOT NULL REFERENCES environments(id),
    id uuid NOT NULL,
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
    seed jsonb NOT NULL CHECK (jsonb_typeof(seed) = 'object'),
    refresh_every_ms bigint CHECK (refresh_every_ms > 0),
    next_refresh_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,spec_digest),
    CHECK ((refresh_every_ms IS NULL) = (next_refresh_at IS NULL))
);

CREATE INDEX image_specs_due ON image_specs(next_refresh_at)
    WHERE next_refresh_at IS NOT NULL;

CREATE INDEX image_specs_refresh ON image_specs(environment_id,id)
    WHERE refresh_every_ms IS NOT NULL;

CREATE TABLE templates (
    environment_id uuid NOT NULL REFERENCES environments(id),
    id uuid NOT NULL,
    key text NOT NULL CHECK (key ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,key)
);

CREATE TABLE template_definitions (
    environment_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    template_id uuid NOT NULL,
    image_spec_id uuid NOT NULL,
    cpu_millis bigint NOT NULL CHECK (cpu_millis > 0),
    memory_bytes bigint NOT NULL CHECK (memory_bytes > 0 AND memory_bytes % 1048576 = 0),
    disk_bytes bigint CHECK (disk_bytes > 0 AND disk_bytes % 1048576 = 0),
    refresh_every_ms bigint CHECK (refresh_every_ms > 0),
    max_image_age_ms bigint CHECK (max_image_age_ms > 0),
    delete_after_unused_ms bigint CHECK (delete_after_unused_ms > 0),
    delete_after_unused_never boolean NOT NULL DEFAULT false,
    CHECK (NOT delete_after_unused_never OR delete_after_unused_ms IS NULL),
    PRIMARY KEY (environment_id,template_id,deployment_id),
    FOREIGN KEY (environment_id,template_id) REFERENCES templates(environment_id,id),
    FOREIGN KEY (environment_id,deployment_id) REFERENCES deployments(environment_id,id),
    FOREIGN KEY (environment_id,image_spec_id) REFERENCES image_specs(environment_id,id)
);

CREATE INDEX template_definitions_deployment ON template_definitions(environment_id,deployment_id,template_id);

CREATE INDEX template_definitions_preparation_spec ON template_definitions(environment_id,image_spec_id);

CREATE TABLE agents (
    environment_id uuid NOT NULL REFERENCES environments(id),
    id uuid NOT NULL,
    key text NOT NULL CHECK (key ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,key)
);

CREATE TABLE agent_definitions (
    environment_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    template_id uuid NOT NULL,
    has_init boolean NOT NULL,
    max_turn_duration_ms bigint CHECK (max_turn_duration_ms > 0),
    close_after_idle_ms bigint CHECK (close_after_idle_ms > 0),
    close_after_idle_never boolean NOT NULL DEFAULT false,
    CHECK (NOT close_after_idle_never OR close_after_idle_ms IS NULL),
    PRIMARY KEY (environment_id,agent_id,deployment_id),
    FOREIGN KEY (environment_id,template_id,deployment_id) REFERENCES template_definitions(environment_id,template_id,deployment_id),
    FOREIGN KEY (environment_id,agent_id) REFERENCES agents(environment_id,id),
    FOREIGN KEY (environment_id,deployment_id) REFERENCES deployments(environment_id,id)
);

CREATE INDEX agent_definitions_deployment ON agent_definitions(environment_id,deployment_id,agent_id);

CREATE TABLE disk_encryption_keys (
    id UUID PRIMARY KEY,
    environment_id UUID NOT NULL,
    writer_computer_id UUID,
    writer_image_build_id UUID,
    wrapping_key_id TEXT NOT NULL CHECK (octet_length(wrapping_key_id) BETWEEN 1 AND 2048),
    wrapped_key BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at TIMESTAMPTZ,
    available BOOLEAN GENERATED ALWAYS AS (retired_at IS NULL) STORED,
    CONSTRAINT disk_encryption_keys_material_check CHECK (
        (retired_at IS NULL AND wrapped_key IS NOT NULL AND octet_length(wrapped_key) BETWEEN 1 AND 6144)
        OR (retired_at IS NOT NULL AND wrapped_key IS NULL)
    ),
    UNIQUE (environment_id, id, available),
    UNIQUE (environment_id, writer_computer_id, id, available),
    UNIQUE (environment_id, writer_image_build_id, id, available),
    CHECK (writer_computer_id IS NULL OR writer_image_build_id IS NULL),
    -- Preparation capture history retains key identity after physical closure,
    -- while the published graph independently retains every usable dependency.
    UNIQUE(environment_id,writer_image_build_id,id),
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE RESTRICT,
    -- Historical lease identity is retained after the physical writer and its
    -- pending publication obligations have released their usable dependencies.
    UNIQUE(environment_id,writer_computer_id,id)
);

-- Logical outcome never substitutes for proof that a preparation writer stopped.
CREATE TABLE image_builds (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    image_spec_id uuid NOT NULL,
    successor_of uuid,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed')),
    deadline_at timestamptz NOT NULL,
    error_code text,
    executor_epoch bigint NOT NULL DEFAULT 0 CHECK (executor_epoch>=0),
    worker_host_id uuid REFERENCES worker_hosts(id),
    worker_epoch bigint CHECK (worker_epoch>0),
    allocation_id uuid UNIQUE,
    channel_credential_digest bytea CHECK (octet_length(channel_credential_digest)=32),
    executor_expires_at timestamptz,
    delivered_at timestamptz,
    reserved_cpu_millis bigint CHECK (reserved_cpu_millis>0 AND reserved_cpu_millis%1000=0),
    reserved_memory_bytes bigint CHECK (reserved_memory_bytes>0),
    reserved_scratch_bytes bigint CHECK (reserved_scratch_bytes>0),
    vm_platform_id text REFERENCES vm_platforms(id),
    vm_vcpu_count integer CHECK (vm_vcpu_count>0),
    cpu_config_digest text CHECK (cpu_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    proxy_ca_certificate bytea,
    proxy_ca_private_key_nonce bytea,
    proxy_ca_private_key_ciphertext bytea,
    proxy_ca_not_after timestamptz,
    fenced_at timestamptz,
    fence_evidence text,
    stdout_accepted_through bigint NOT NULL DEFAULT 0 CHECK (stdout_accepted_through>=0),
    stdout_byte_offset bigint NOT NULL DEFAULT 0 CHECK (stdout_byte_offset>=0),
    stdout_ended boolean NOT NULL DEFAULT false,
    stdout_end_complete boolean NOT NULL DEFAULT false CHECK (NOT stdout_end_complete OR stdout_ended),
    stdout_gapped boolean NOT NULL DEFAULT false,
    stdout_expired_through bigint NOT NULL DEFAULT 0 CHECK (stdout_expired_through BETWEEN 0 AND stdout_accepted_through),
    stdout_last_sequence bigint,
    stdout_last_digest bytea CHECK (stdout_last_digest IS NULL OR octet_length(stdout_last_digest)=32),
    stdout_last_accepted_at timestamptz,
    stderr_accepted_through bigint NOT NULL DEFAULT 0 CHECK (stderr_accepted_through>=0),
    stderr_byte_offset bigint NOT NULL DEFAULT 0 CHECK (stderr_byte_offset>=0),
    stderr_ended boolean NOT NULL DEFAULT false,
    stderr_end_complete boolean NOT NULL DEFAULT false CHECK (NOT stderr_end_complete OR stderr_ended),
    stderr_gapped boolean NOT NULL DEFAULT false,
    stderr_expired_through bigint NOT NULL DEFAULT 0 CHECK (stderr_expired_through BETWEEN 0 AND stderr_accepted_through),
    stderr_last_sequence bigint,
    stderr_last_digest bytea CHECK (stderr_last_digest IS NULL OR octet_length(stderr_last_digest)=32),
    stderr_last_accepted_at timestamptz,
    write_key_id uuid,
    disk_released_at timestamptz,
    write_key_required boolean GENERATED ALWAYS AS
        (CASE WHEN disk_released_at IS NULL THEN true END) STORED,
    -- Capture seals further build-secret delivery. It is not physical stop evidence.
    logical_bytes bigint CHECK (logical_bytes>0 AND logical_bytes%4096=0),
    capture_root text CHECK (capture_root ~ '^sha256:[0-9a-f]{64}$'),
    capture_evidence text CHECK (octet_length(capture_evidence) BETWEEN 1 AND 4096),
    CHECK (num_nonnulls(proxy_ca_certificate,proxy_ca_private_key_nonce,proxy_ca_private_key_ciphertext,proxy_ca_not_after) IN (0,4)),
    CHECK (proxy_ca_certificate IS NULL OR (status='running' AND octet_length(proxy_ca_certificate) BETWEEN 1 AND 16384 AND octet_length(proxy_ca_private_key_nonce)=12 AND octet_length(proxy_ca_private_key_ciphertext)>0)),
    CHECK ((stdout_accepted_through=0 AND stdout_last_sequence IS NULL AND stdout_last_digest IS NULL AND stdout_last_accepted_at IS NULL AND NOT stdout_ended)
        OR (stdout_accepted_through>0 AND stdout_last_sequence IS NOT NULL AND stdout_last_sequence BETWEEN 1 AND stdout_accepted_through AND stdout_last_accepted_at IS NOT NULL
            AND (stdout_last_digest IS NOT NULL OR stdout_expired_through=stdout_accepted_through))),
    CHECK ((stderr_accepted_through=0 AND stderr_last_sequence IS NULL AND stderr_last_digest IS NULL AND stderr_last_accepted_at IS NULL AND NOT stderr_ended)
        OR (stderr_accepted_through>0 AND stderr_last_sequence IS NOT NULL AND stderr_last_sequence BETWEEN 1 AND stderr_accepted_through AND stderr_last_accepted_at IS NOT NULL
            AND (stderr_last_digest IS NOT NULL OR stderr_expired_through=stderr_accepted_through))),
    UNIQUE(environment_id,id,executor_epoch),
    PRIMARY KEY(environment_id,id),
    UNIQUE(environment_id,image_spec_id,id),
    UNIQUE(environment_id,image_spec_id,idempotency_key),
    UNIQUE(environment_id,successor_of),
    FOREIGN KEY(environment_id,image_spec_id,successor_of) REFERENCES image_builds(environment_id,image_spec_id,id),
    CHECK (successor_of IS NULL OR successor_of<>id),
    FOREIGN KEY(environment_id,image_spec_id) REFERENCES image_specs(environment_id,id),
    CHECK ((status='failed')=(error_code IS NOT NULL)),
    CHECK ((fenced_at IS NULL)=(fence_evidence IS NULL)),
    CHECK ((worker_host_id IS NULL AND executor_epoch=0 AND worker_epoch IS NULL AND allocation_id IS NULL AND channel_credential_digest IS NULL AND executor_expires_at IS NULL AND fenced_at IS NULL)
        OR (worker_host_id IS NOT NULL AND executor_epoch>0 AND worker_epoch IS NOT NULL AND allocation_id IS NOT NULL AND channel_credential_digest IS NOT NULL)),
    CHECK ((delivered_at IS NULL)=(executor_expires_at IS NULL)),
    CHECK ((worker_host_id IS NULL AND reserved_cpu_millis IS NULL AND reserved_memory_bytes IS NULL AND reserved_scratch_bytes IS NULL AND vm_platform_id IS NULL AND vm_vcpu_count IS NULL AND cpu_config_digest IS NULL)
        OR (worker_host_id IS NOT NULL AND reserved_cpu_millis IS NOT NULL AND reserved_memory_bytes IS NOT NULL AND reserved_scratch_bytes IS NOT NULL AND vm_platform_id IS NOT NULL AND vm_vcpu_count IS NOT NULL AND cpu_config_digest IS NOT NULL AND reserved_cpu_millis=vm_vcpu_count::bigint*1000)),
    CHECK (status<>'running' OR worker_host_id IS NOT NULL),
    CHECK (status<>'queued' OR worker_host_id IS NULL),
    CHECK (fenced_at IS NULL OR status IN ('succeeded','failed')),
    CHECK (disk_released_at IS NULL OR (fenced_at IS NOT NULL AND write_key_id IS NOT NULL)),
    FOREIGN KEY (environment_id,id,write_key_id)
        REFERENCES disk_encryption_keys(environment_id,writer_image_build_id,id),
    FOREIGN KEY (environment_id,id,write_key_id,write_key_required)
        REFERENCES disk_encryption_keys(environment_id,writer_image_build_id,id,available),
    CHECK ((capture_root IS NULL)=(capture_evidence IS NULL)),
    CHECK (status<>'succeeded' OR capture_root IS NOT NULL),
    CHECK (capture_root IS NULL OR (logical_bytes IS NOT NULL AND write_key_id IS NOT NULL))
);

CREATE UNIQUE INDEX image_builds_exclusive ON image_builds(environment_id,image_spec_id)
    WHERE status IN ('queued','running') OR (worker_host_id IS NOT NULL AND fenced_at IS NULL);

CREATE INDEX image_builds_deadline ON image_builds(deadline_at) WHERE status IN ('queued','running');

CREATE INDEX image_builds_expiry ON image_builds(executor_expires_at) WHERE status='running';

CREATE INDEX image_builds_host ON image_builds(worker_host_id,worker_epoch) WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL;

CREATE INDEX image_builds_environment_allocations ON image_builds(environment_id) WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL;

CREATE TABLE disk_objects (
    environment_id UUID NOT NULL,
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    org_id UUID NOT NULL,
    project_id UUID NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    media_type TEXT NOT NULL CHECK (btrim(media_type) <> ''),
    kind TEXT NOT NULL CHECK (kind IN ('segment', 'index', 'root')),
    rank INTEGER NOT NULL CHECK ((kind = 'segment' AND rank = 0) OR (kind <> 'segment' AND rank BETWEEN 1 AND 6)),
    inspection JSONB NOT NULL CHECK (jsonb_typeof(inspection) = 'object' AND pg_column_size(inspection) <= 16777216),
    certified_at TIMESTAMPTZ,
    certified BOOLEAN GENERATED ALWAYS AS (certified_at IS NOT NULL) STORED,
    certified_org_id UUID GENERATED ALWAYS AS (CASE WHEN certified_at IS NOT NULL THEN org_id END) STORED,
    availability_required BOOLEAN GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (environment_id, digest),
    UNIQUE (environment_id, digest, rank),
    UNIQUE (environment_id, digest, rank, certified),
    UNIQUE (environment_id, digest, size_bytes, rank, certified, kind),
    FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments(org_id, project_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired) ON DELETE RESTRICT,
    FOREIGN KEY (certified_org_id, digest, size_bytes, media_type)
        REFERENCES cas_objects(org_id, digest, size_bytes, media_type) ON DELETE RESTRICT
);

CREATE TABLE disk_object_keys (
    environment_id UUID NOT NULL,
    digest TEXT NOT NULL,
    key_id UUID NOT NULL,
    is_direct BOOLEAN NOT NULL,
    availability_required BOOLEAN GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (environment_id, digest, key_id),
    UNIQUE (environment_id, digest, key_id, is_direct),
    FOREIGN KEY (environment_id, digest)
        REFERENCES disk_objects(environment_id, digest) ON DELETE CASCADE,
    FOREIGN KEY (environment_id, key_id, availability_required)
        REFERENCES disk_encryption_keys(environment_id, id, available) ON DELETE RESTRICT
);

CREATE INDEX disk_object_keys_key ON disk_object_keys(environment_id,key_id);

CREATE TABLE disk_object_edges (
    environment_id UUID NOT NULL,
    parent_digest TEXT NOT NULL,
    child_digest TEXT NOT NULL,
    parent_rank INTEGER NOT NULL,
    child_rank INTEGER NOT NULL,
    certification_required BOOLEAN GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (environment_id, parent_digest, child_digest),
    CHECK (child_rank < parent_rank),
    FOREIGN KEY (environment_id, parent_digest, parent_rank)
        REFERENCES disk_objects(environment_id, digest, rank) ON DELETE CASCADE,
    FOREIGN KEY (environment_id, child_digest, child_rank, certification_required)
        REFERENCES disk_objects(environment_id, digest, rank, certified) ON DELETE RESTRICT
);

CREATE INDEX disk_object_edges_child ON disk_object_edges(environment_id,child_digest);

CREATE TABLE disk_roots (
    environment_id UUID NOT NULL,
    id UUID PRIMARY KEY,
    format_version INTEGER NOT NULL CHECK (format_version = 1),
    logical_bytes BIGINT NOT NULL CHECK (logical_bytes > 0 AND logical_bytes % 4096 = 0),
    pack_digest TEXT NOT NULL CHECK (pack_digest ~ '^sha256:[0-9a-f]{64}$'),
    pack_size_bytes BIGINT NOT NULL,
    pack_rank INTEGER NOT NULL,
    page_digest TEXT NOT NULL CHECK (page_digest ~ '^sha256:[0-9a-f]{64}$'),
    page_salt TEXT NOT NULL CHECK (page_salt ~ '^[0-9a-f]{64}$'),
    page_key_id UUID NOT NULL,
    page_kind INTEGER NOT NULL,
    page_count INTEGER NOT NULL,
    page_size_bytes BIGINT NOT NULL,
    page_offset BIGINT NOT NULL,
    root_kind TEXT GENERATED ALWAYS AS ('root'::text) STORED,
    direct_key_required BOOLEAN GENERATED ALWAYS AS (true) STORED,
    certification_required BOOLEAN GENERATED ALWAYS AS (true) STORED,
    -- A root is one encrypted metadata page inside a rank-separated pack.
    CHECK (pack_size_bytes BETWEEN 8 AND 4194304 AND pack_rank BETWEEN 2 AND 6
        AND page_kind = 3 AND page_count = 1 AND page_size_bytes > 20 AND page_size_bytes <= pack_size_bytes
        AND page_offset >= 8 AND page_offset <= pack_size_bytes - page_size_bytes),
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, pack_digest, page_offset),
    FOREIGN KEY (environment_id, pack_digest, pack_size_bytes, pack_rank, certification_required, root_kind)
        REFERENCES disk_objects(environment_id, digest, size_bytes, rank, certified, kind) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id, pack_digest, page_key_id, direct_key_required)
        REFERENCES disk_object_keys(environment_id, digest, key_id, is_direct) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE RESTRICT
);

CREATE TABLE images (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    image_spec_id uuid NOT NULL,
    image_build_id uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq>0),
    root_id uuid,
    payload_retired_at timestamptz,
    publication_evidence text NOT NULL CHECK (octet_length(publication_evidence) BETWEEN 1 AND 4096),
    published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((root_id IS NOT NULL) <> (payload_retired_at IS NOT NULL)),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,image_build_id),
    UNIQUE (environment_id,image_spec_id,id),
    UNIQUE (environment_id,image_spec_id,seq),
    FOREIGN KEY (environment_id,image_spec_id,image_build_id) REFERENCES image_builds(environment_id,image_spec_id,id),
    FOREIGN KEY (environment_id,root_id) REFERENCES disk_roots(environment_id,id)
);

CREATE TABLE computers (
    delete_after_unused_ms bigint CHECK (delete_after_unused_ms > 0),
    unused_since timestamptz,
    environment_id uuid NOT NULL REFERENCES environments(id),
    id uuid NOT NULL,
    key text CHECK (key IS NULL OR octet_length(key) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_activity_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    initial_root_id uuid,
    initial_root_digest bytea CHECK (octet_length(initial_root_digest)=32),
    initial_payload_retired_at timestamptz,
    image_spec_id uuid,
    origin_deployment_id uuid NOT NULL,
    origin_template_id uuid NOT NULL,
    cpu_millis bigint NOT NULL CHECK (cpu_millis>0),
    memory_bytes bigint NOT NULL CHECK (memory_bytes>0 AND memory_bytes%1048576=0),
    disk_bytes bigint CHECK (disk_bytes>0 AND disk_bytes%1048576=0),
    storage_reservation_bytes bigint CHECK (storage_reservation_bytes>0),
    image_build_id uuid,
    preparation_deadline_at timestamptz,
    preparation_max_age_ms bigint CHECK (preparation_max_age_ms>0),
    image_id uuid,
    preparation_failed_at timestamptz,
    next_save_seq bigint NOT NULL DEFAULT 1 CHECK (next_save_seq > 0),
    next_control_version bigint NOT NULL DEFAULT 1 CHECK (next_control_version > 0),
    recovery_save_id uuid,
    integrity_fault_at timestamptz,
    integrity_fault_reason text,
    deleted_at timestamptz,
    proxy_ca_certificate bytea,
    proxy_ca_not_after timestamptz,
    proxy_ca_private_key_nonce bytea,
    proxy_ca_private_key_ciphertext bytea,
    recovery_published boolean GENERATED ALWAYS AS (true) STORED,
    CHECK (num_nonnulls(proxy_ca_certificate,proxy_ca_not_after,proxy_ca_private_key_nonce,proxy_ca_private_key_ciphertext) IN (0,4)),
    CHECK (proxy_ca_certificate IS NULL OR (octet_length(proxy_ca_certificate) BETWEEN 1 AND 16384 AND octet_length(proxy_ca_private_key_nonce)=12 AND octet_length(proxy_ca_private_key_ciphertext)>0)),
    PRIMARY KEY (environment_id,id),
    FOREIGN KEY(environment_id,origin_template_id,origin_deployment_id) REFERENCES template_definitions(environment_id,template_id,deployment_id),
    CHECK (storage_reservation_bytes IS NOT NULL OR deleted_at IS NOT NULL),
    FOREIGN KEY(environment_id,image_spec_id) REFERENCES image_specs(environment_id,id),
    FOREIGN KEY(environment_id,image_spec_id,image_build_id) REFERENCES image_builds(environment_id,image_spec_id,id),
    CHECK (initial_root_digest IS NOT NULL OR (image_spec_id IS NOT NULL AND preparation_deadline_at IS NOT NULL)),
    CHECK (image_build_id IS NULL OR image_spec_id IS NOT NULL),
    CHECK (preparation_failed_at IS NULL OR (initial_root_digest IS NULL AND preparation_deadline_at IS NOT NULL)),
    CHECK ((integrity_fault_at IS NULL) = (integrity_fault_reason IS NULL)),
    CHECK ((initial_root_digest IS NOT NULL) = (initial_root_id IS NOT NULL OR initial_payload_retired_at IS NOT NULL)),
    CHECK (initial_payload_retired_at IS NULL OR (initial_root_id IS NULL AND deleted_at IS NOT NULL)),
    FOREIGN KEY (environment_id,initial_root_id)
    REFERENCES disk_roots(environment_id,id),
    FOREIGN KEY (environment_id,image_spec_id,image_id) REFERENCES images(environment_id,image_spec_id,id),
    CHECK (image_id IS NULL OR (image_spec_id IS NOT NULL AND initial_root_digest IS NOT NULL))
);

CREATE INDEX computers_unused_policy ON computers(environment_id,id) WHERE deleted_at IS NULL AND delete_after_unused_ms IS NOT NULL;

CREATE UNIQUE INDEX computers_key ON computers(environment_id,key) WHERE key IS NOT NULL;

CREATE INDEX computers_storage_reservations ON computers(environment_id) INCLUDE (storage_reservation_bytes) WHERE storage_reservation_bytes IS NOT NULL;

CREATE INDEX computers_preparation ON computers(environment_id,image_spec_id,image_build_id) WHERE image_build_id IS NOT NULL;

CREATE INDEX computers_preparation_deadline ON computers(preparation_deadline_at) WHERE initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL;

CREATE INDEX computers_image ON computers(environment_id,image_id) WHERE image_id IS NOT NULL;

CREATE TABLE secret_bindings (
    environment_id uuid NOT NULL,
    image_spec_id uuid,
    deployment_id uuid,
    template_id uuid,
    computer_id uuid,
    secret_id uuid NOT NULL,
    placement_kind text NOT NULL CHECK (placement_kind IN ('env','file')),
    placement_target text NOT NULL CHECK (octet_length(placement_target) BETWEEN 1 AND 4096),
    mode text NOT NULL CHECK (mode IN ('raw','protected')),
    placeholder text,
    allowed_origins text[] NOT NULL DEFAULT '{}',
    CHECK ((mode='protected' AND placeholder IS NOT NULL AND placeholder ~ '^hlmr_protected_[0-9a-f]{64}$') OR (mode='raw' AND placeholder IS NULL)),
    CHECK (num_nonnulls(image_spec_id,deployment_id,computer_id)=1),
    CHECK ((deployment_id IS NULL)=(template_id IS NULL)),
    CHECK ((mode='protected' AND placement_kind='env' AND cardinality(allowed_origins) BETWEEN 1 AND 16)
        OR (mode='raw' AND cardinality(allowed_origins)=0)),
    UNIQUE NULLS NOT DISTINCT (environment_id,image_spec_id,deployment_id,template_id,computer_id,placement_kind,placement_target),
    FOREIGN KEY (environment_id,image_spec_id) REFERENCES image_specs(environment_id,id),
    FOREIGN KEY (environment_id,template_id,deployment_id) REFERENCES template_definitions(environment_id,template_id,deployment_id),
    FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
    FOREIGN KEY (environment_id,secret_id) REFERENCES secrets(environment_id,id)
);

CREATE INDEX secret_bindings_secret ON secret_bindings(environment_id,secret_id);

CREATE TABLE computer_leases (
    environment_id uuid NOT NULL,
    computer_id uuid NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    worker_host_id uuid NOT NULL REFERENCES worker_hosts(id),
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    expires_at timestamptz,
    allocation_id uuid NOT NULL UNIQUE,
    channel_credential_digest bytea NOT NULL CHECK (octet_length(channel_credential_digest)=32),
    restored_from_save_id uuid,
    delivered_at timestamptz,
    initialized_at timestamptz,
    reserved_cpu_millis bigint NOT NULL CHECK (reserved_cpu_millis>0 AND reserved_cpu_millis%1000=0),
    reserved_memory_bytes bigint NOT NULL CHECK (reserved_memory_bytes>0),
    reserved_scratch_bytes bigint NOT NULL CHECK (reserved_scratch_bytes>0),
    vm_platform_id text NOT NULL REFERENCES vm_platforms(id),
    vm_vcpu_count integer NOT NULL CHECK (vm_vcpu_count>0),
    cpu_config_digest text NOT NULL CHECK (cpu_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    status text NOT NULL CHECK (status IN ('acquiring','active','releasing','lost','released')),
    fenced_at timestamptz,
    fence_evidence text,
    restored_published boolean GENERATED ALWAYS AS (true) STORED,
    base_root_id uuid,
    write_key_id uuid,
    disk_released_at timestamptz,
    retained_base_root_id uuid GENERATED ALWAYS AS
        (CASE WHEN disk_released_at IS NULL THEN base_root_id END) STORED,
    write_key_required boolean GENERATED ALWAYS AS
        (CASE WHEN disk_released_at IS NULL THEN true END) STORED,
    PRIMARY KEY (environment_id,computer_id,epoch),
    FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
    CHECK ((delivered_at IS NULL)=(expires_at IS NULL)),
    CHECK (reserved_cpu_millis=vm_vcpu_count::bigint*1000),
    CHECK (initialized_at IS NULL OR delivered_at IS NOT NULL),
    CHECK (status<>'active' OR initialized_at IS NOT NULL),
    CHECK ((fenced_at IS NULL) = (fence_evidence IS NULL)),
    CHECK (status <> 'released' OR fenced_at IS NOT NULL),
    CHECK (status NOT IN ('active','acquiring') OR fenced_at IS NULL),
    CHECK ((base_root_id IS NULL)=(write_key_id IS NULL)),
    CHECK (disk_released_at IS NULL OR (fenced_at IS NOT NULL AND base_root_id IS NOT NULL)),
    FOREIGN KEY (environment_id,retained_base_root_id) REFERENCES disk_roots(environment_id,id),
    FOREIGN KEY (environment_id,computer_id,write_key_id)
        REFERENCES disk_encryption_keys(environment_id,writer_computer_id,id),
    FOREIGN KEY (environment_id,computer_id,write_key_id,write_key_required)
        REFERENCES disk_encryption_keys(environment_id,writer_computer_id,id,available)
);

CREATE UNIQUE INDEX computer_leases_one_writer ON computer_leases(environment_id,computer_id) WHERE fenced_at IS NULL;

CREATE INDEX computer_leases_expiring ON computer_leases(expires_at,environment_id,computer_id,epoch) WHERE fenced_at IS NULL AND status IN ('active','acquiring','releasing');

CREATE INDEX computer_leases_host_unfenced ON computer_leases(worker_host_id,worker_epoch) WHERE fenced_at IS NULL;

-- Ordinary commands are pinned to one Computer lease when execution is admitted.
CREATE TABLE commands (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    computer_id uuid NOT NULL,
    computer_lease_epoch bigint CHECK (computer_lease_epoch>0),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','starting','running','stopping','exited','failed','cancelled','timed_out','lost')),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
    argv text[] CHECK (cardinality(argv)>0 AND array_position(argv,NULL) IS NULL),
    cwd text,
    env jsonb CHECK (jsonb_typeof(env)='object'),
    stdin bytea CHECK (octet_length(stdin)<=1048576),
    timeout_ms bigint NOT NULL CHECK (timeout_ms>0),
    caller_kind text NOT NULL CHECK (caller_kind IN ('api_key','session')),
    caller_id uuid NOT NULL,
    started_at timestamptz,
    cancel_requested_at timestamptz,
    process_exited_at timestamptz,
    process_reconciled_at timestamptz,
    stdout_final_through bigint CHECK (stdout_final_through>0),
    stdout_final_complete boolean,
    stdout_final_gapped boolean,
    stderr_final_through bigint CHECK (stderr_final_through>0),
    stderr_final_complete boolean,
    stderr_final_gapped boolean,
    output_fenced boolean NOT NULL DEFAULT false,
    exit_code integer,
    error jsonb CHECK (jsonb_typeof(error)='object'),
    terminal_at timestamptz,
    terminal_reason_code text,
    failure_reason text CHECK (failure_reason IN ('guest_failure','dispatch_failed','scope_termination_failed')),
    result_expires_at timestamptz,
    result_pruned_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    stdout_accepted_through bigint NOT NULL DEFAULT 0 CHECK (stdout_accepted_through>=0),
    stdout_byte_offset bigint NOT NULL DEFAULT 0 CHECK (stdout_byte_offset>=0),
    stdout_ended boolean NOT NULL DEFAULT false,
    stdout_end_complete boolean NOT NULL DEFAULT false CHECK (NOT stdout_end_complete OR stdout_ended),
    stdout_gapped boolean NOT NULL DEFAULT false,
    stdout_expired_through bigint NOT NULL DEFAULT 0 CHECK (stdout_expired_through BETWEEN 0 AND stdout_accepted_through),
    stdout_last_sequence bigint,
    stdout_last_digest bytea CHECK (stdout_last_digest IS NULL OR octet_length(stdout_last_digest)=32),
    stdout_last_accepted_at timestamptz,
    stderr_accepted_through bigint NOT NULL DEFAULT 0 CHECK (stderr_accepted_through>=0),
    stderr_byte_offset bigint NOT NULL DEFAULT 0 CHECK (stderr_byte_offset>=0),
    stderr_ended boolean NOT NULL DEFAULT false,
    stderr_end_complete boolean NOT NULL DEFAULT false CHECK (NOT stderr_end_complete OR stderr_ended),
    stderr_gapped boolean NOT NULL DEFAULT false,
    stderr_expired_through bigint NOT NULL DEFAULT 0 CHECK (stderr_expired_through BETWEEN 0 AND stderr_accepted_through),
    stderr_last_sequence bigint,
    stderr_last_digest bytea CHECK (stderr_last_digest IS NULL OR octet_length(stderr_last_digest)=32),
    stderr_last_accepted_at timestamptz,
    CHECK ((stdout_final_through IS NULL AND stdout_final_complete IS NULL AND stdout_final_gapped IS NULL AND stderr_final_through IS NULL AND stderr_final_complete IS NULL AND stderr_final_gapped IS NULL)
       OR (stdout_final_through IS NOT NULL AND stdout_final_complete IS NOT NULL AND stdout_final_gapped IS NOT NULL AND stderr_final_through IS NOT NULL AND stderr_final_complete IS NOT NULL AND stderr_final_gapped IS NOT NULL AND terminal_at IS NOT NULL)),
    CHECK (NOT output_fenced OR terminal_at IS NOT NULL),
    CHECK ((stdout_accepted_through=0 AND stdout_last_sequence IS NULL AND stdout_last_digest IS NULL AND stdout_last_accepted_at IS NULL AND NOT stdout_ended)
        OR (stdout_accepted_through>0 AND stdout_last_sequence IS NOT NULL AND stdout_last_sequence BETWEEN 1 AND stdout_accepted_through AND stdout_last_accepted_at IS NOT NULL
            AND (stdout_last_digest IS NOT NULL OR stdout_expired_through=stdout_accepted_through))),
    CHECK ((stderr_accepted_through=0 AND stderr_last_sequence IS NULL AND stderr_last_digest IS NULL AND stderr_last_accepted_at IS NULL AND NOT stderr_ended)
        OR (stderr_accepted_through>0 AND stderr_last_sequence IS NOT NULL AND stderr_last_sequence BETWEEN 1 AND stderr_accepted_through AND stderr_last_accepted_at IS NOT NULL
            AND (stderr_last_digest IS NOT NULL OR stderr_expired_through=stderr_accepted_through))),
    PRIMARY KEY(environment_id,id),
    UNIQUE(environment_id,computer_id,id),
    FOREIGN KEY(environment_id,computer_id) REFERENCES computers(environment_id,id),
    FOREIGN KEY(environment_id,computer_id,computer_lease_epoch) REFERENCES computer_leases(environment_id,computer_id,epoch),
    CHECK (status NOT IN ('starting','running','stopping','exited','timed_out') OR computer_lease_epoch IS NOT NULL),
    CHECK ((status IN ('exited','failed','cancelled','timed_out','lost'))=(terminal_at IS NOT NULL)),
    CHECK ((terminal_at IS NULL)=(terminal_reason_code IS NULL)),
    CHECK (status<>'exited' OR (exit_code IS NOT NULL AND process_exited_at IS NOT NULL)),
    CHECK (process_reconciled_at IS NULL OR terminal_at IS NOT NULL),
    CHECK ((status IN ('failed','lost'))=(failure_reason IS NOT NULL)),
    CHECK ((result_pruned_at IS NULL AND argv IS NOT NULL AND env IS NOT NULL AND stdin IS NOT NULL)
        OR (result_pruned_at IS NOT NULL AND result_expires_at IS NOT NULL AND result_pruned_at>=result_expires_at
            AND terminal_at IS NOT NULL AND (computer_lease_epoch IS NULL OR process_reconciled_at IS NOT NULL)
            AND argv IS NULL AND cwd IS NULL AND env IS NULL AND stdin IS NULL AND error IS NULL))
);

CREATE INDEX commands_pending ON commands(created_at,id) WHERE status='pending';

CREATE INDEX commands_unreconciled ON commands(environment_id,computer_id,computer_lease_epoch) WHERE computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL;

-- Scoped platform mutation identity survives response expiry. Domain mutation and
-- target binding commit together; an unfinished acquisition is never durable work.
CREATE TABLE idempotency_receipts (
    environment_id uuid NOT NULL REFERENCES environments(id),
    id uuid NOT NULL,
    operation text NOT NULL CHECK (operation IN ('deployment.finalize','secret.create','secret.rotate','secret.revoke','computer.create','computer.delete','computer.exec','command.cancel')),
    slot_hash bytea NOT NULL CHECK (octet_length(slot_hash)=32),
    request_fingerprint bytea NOT NULL CHECK (octet_length(request_fingerprint)=32),
    scope_secret_name text,
    scope_secret_id uuid,
    scope_computer_id uuid,
    scope_command_id uuid,
    scope_caller_kind text,
    scope_computer_key text,
    deployment_id uuid,
    secret_id uuid,
    secret_version_id uuid,
    computer_id uuid,
    command_id uuid,
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    receipt jsonb CHECK (jsonb_typeof(receipt)='object'),
    receipt_pruned_at timestamptz,
    PRIMARY KEY(environment_id,id),
    UNIQUE(environment_id,operation,slot_hash),
    FOREIGN KEY(environment_id,scope_secret_id) REFERENCES secrets(environment_id,id),
    FOREIGN KEY(environment_id,scope_computer_id) REFERENCES computers(environment_id,id),
    FOREIGN KEY(environment_id,scope_command_id) REFERENCES commands(environment_id,id),
    FOREIGN KEY(environment_id,deployment_id) REFERENCES deployments(environment_id,id),
    FOREIGN KEY(environment_id,secret_id) REFERENCES secrets(environment_id,id),
    FOREIGN KEY(secret_id,secret_version_id) REFERENCES secret_versions(secret_id,id),
    FOREIGN KEY(environment_id,computer_id) REFERENCES computers(environment_id,id),
    FOREIGN KEY(environment_id,command_id) REFERENCES commands(environment_id,id),
    FOREIGN KEY(environment_id,scope_computer_id,command_id) REFERENCES commands(environment_id,computer_id,id),
    CHECK (receipt_pruned_at IS NULL OR (receipt IS NULL AND receipt_pruned_at>=accepted_at+interval '720 hours')),
    CHECK ((
        (operation='deployment.finalize' AND num_nonnulls(scope_secret_name,scope_secret_id,scope_computer_id,scope_command_id,scope_caller_kind,scope_computer_key)=0)
        OR (operation='secret.create' AND scope_secret_name IS NOT NULL AND octet_length(scope_secret_name)>0 AND num_nonnulls(scope_secret_id,scope_computer_id,scope_command_id,scope_caller_kind,scope_computer_key)=0)
        OR (operation IN ('secret.rotate','secret.revoke') AND scope_secret_id IS NOT NULL AND num_nonnulls(scope_secret_name,scope_computer_id,scope_command_id,scope_caller_kind,scope_computer_key)=0)
        OR (operation='computer.create' AND scope_caller_kind='external' AND scope_computer_key IS NOT NULL AND octet_length(scope_computer_key)>0 AND num_nonnulls(scope_secret_name,scope_secret_id,scope_computer_id,scope_command_id)=0)
        OR (operation IN ('computer.delete','computer.exec') AND scope_computer_id IS NOT NULL AND num_nonnulls(scope_secret_name,scope_secret_id,scope_command_id,scope_caller_kind,scope_computer_key)=0)
        OR (operation='command.cancel' AND scope_command_id IS NOT NULL AND num_nonnulls(scope_secret_name,scope_secret_id,scope_computer_id,scope_caller_kind,scope_computer_key)=0)
    ) IS TRUE),
    CONSTRAINT idempotency_receipts_complete CHECK ((
        num_nonnulls(deployment_id,secret_id,computer_id,command_id)=1
        AND (receipt IS NOT NULL OR receipt_pruned_at IS NOT NULL)
        AND (
            (operation='deployment.finalize' AND deployment_id IS NOT NULL AND secret_version_id IS NULL)
            OR (operation='secret.create' AND secret_id IS NOT NULL AND secret_version_id IS NOT NULL)
            OR (operation='secret.rotate' AND secret_id=scope_secret_id AND secret_version_id IS NOT NULL)
            OR (operation='secret.revoke' AND secret_id=scope_secret_id AND secret_version_id IS NULL)
            OR (operation='computer.create' AND computer_id IS NOT NULL AND secret_version_id IS NULL)
            OR (operation='computer.delete' AND computer_id=scope_computer_id AND secret_version_id IS NULL)
            OR (operation='computer.exec' AND command_id IS NOT NULL AND secret_version_id IS NULL)
            OR (operation='command.cancel' AND command_id=scope_command_id AND secret_version_id IS NULL)
        )
    ) IS TRUE)
);

CREATE UNIQUE INDEX idempotency_receipts_command_creation ON idempotency_receipts(environment_id,command_id) WHERE operation='computer.exec';

CREATE INDEX idempotency_receipts_receipt_expiry ON idempotency_receipts(accepted_at,id) WHERE receipt_pruned_at IS NULL;

-- Slack configuration generations never own execution state.
CREATE TABLE slack_app_registrations (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL REFERENCES organizations(id),
    app_id text CHECK (app_id<>''),
    client_id text CHECK (client_id<>''),
    credential_revision bigint NOT NULL DEFAULT 0 CHECK (credential_revision>=0),
    credential_ciphertext bytea,
    credential_nonce bytea,
    app_name text,
    app_icon_url text,
    created_by_user_id uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    retired_at timestamptz,
    UNIQUE(id,org_id),
    CHECK ((credential_revision=0)=(client_id IS NULL)),
    CHECK ((credential_ciphertext IS NULL)=(credential_nonce IS NULL)),
    CHECK ((retired_at IS NULL AND credential_revision>0)=(credential_ciphertext IS NOT NULL)),
    CHECK (credential_nonce IS NULL OR octet_length(credential_nonce)=12),
    CHECK (credential_ciphertext IS NULL OR octet_length(credential_ciphertext)>=16)
);

CREATE UNIQUE INDEX slack_app_registrations_active_app ON slack_app_registrations(app_id) WHERE retired_at IS NULL AND app_id IS NOT NULL;

CREATE TABLE slack_installations (
    id uuid PRIMARY KEY,
    app_registration_id uuid NOT NULL,
    org_id uuid NOT NULL REFERENCES organizations(id),
    app_id text NOT NULL CHECK (app_id<>''),
    team_id text NOT NULL CHECK (team_id<>''),
    bot_user_id text NOT NULL CHECK (bot_user_id<>''),
    workspace_name text,
    credential_revision bigint NOT NULL CHECK (credential_revision>0),
    credential_ciphertext bytea,
    credential_nonce bytea,
    credential_expires_at timestamptz,
    refresh_attempt_id uuid,
    last_refresh_attempt_id uuid,
    refresh_deadline timestamptz,
    refresh_next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    refresh_error text,
    granted_scopes text[] NOT NULL,
    connected_by_user_id uuid NOT NULL REFERENCES users(id),
    connected_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    authorized_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivery_next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    history_next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    authorization_lost_at timestamptz,
    disconnected_at timestamptz,
    FOREIGN KEY(app_registration_id,org_id) REFERENCES slack_app_registrations(id,org_id),
    UNIQUE(id,app_registration_id),
    UNIQUE(id,org_id,team_id),
    CHECK ((credential_ciphertext IS NULL)=(credential_nonce IS NULL)),
    CHECK (credential_nonce IS NULL OR octet_length(credential_nonce)=12),
    CHECK ((refresh_attempt_id IS NULL)=(refresh_deadline IS NULL)),
    CHECK ((disconnected_at IS NULL)=(credential_ciphertext IS NOT NULL)),
    CHECK (credential_ciphertext IS NULL OR octet_length(credential_ciphertext)>=16)
);

CREATE UNIQUE INDEX slack_installations_active ON slack_installations(app_id,team_id) WHERE disconnected_at IS NULL;

CREATE TABLE slack_connections (
    id uuid NOT NULL UNIQUE,
    environment_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    slack_app_registration_id uuid NOT NULL REFERENCES slack_app_registrations(id),
    slack_installation_id uuid,
    created_by_user_id uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    PRIMARY KEY(environment_id,id),
    UNIQUE(id,environment_id,slack_installation_id),
    FOREIGN KEY(environment_id,agent_id) REFERENCES agents(environment_id,id),
    FOREIGN KEY(slack_installation_id,slack_app_registration_id) REFERENCES slack_installations(id,app_registration_id)
);

CREATE UNIQUE INDEX slack_connections_configured ON slack_connections(environment_id,agent_id) WHERE revoked_at IS NULL;

CREATE UNIQUE INDEX slack_connections_installation_claim ON slack_connections(slack_installation_id) WHERE revoked_at IS NULL AND slack_installation_id IS NOT NULL;

CREATE TABLE slack_user_links (
    team_id text NOT NULL CHECK (team_id<>''),
    slack_user_id text NOT NULL CHECK (slack_user_id<>''),
    user_id uuid NOT NULL REFERENCES users(id),
    linked_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(team_id,slack_user_id),
    UNIQUE(team_id,user_id)
);

CREATE TABLE slack_channel_bindings (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL,
    connection_id uuid NOT NULL,
    installation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    team_id text NOT NULL,
    slack_channel_id text NOT NULL CHECK (slack_channel_id<>''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(connection_id,slack_channel_id),
    UNIQUE(id,environment_id),
    UNIQUE(id,environment_id,org_id,team_id,slack_channel_id),
    FOREIGN KEY(connection_id,environment_id,installation_id) REFERENCES slack_connections(id,environment_id,slack_installation_id),
    FOREIGN KEY(installation_id,org_id,team_id) REFERENCES slack_installations(id,org_id,team_id)
);

CREATE INDEX slack_channel_bindings_environment ON slack_channel_bindings(environment_id,id);

-- Every promotion starts a new half-open schedule activation interval.
-- A trigger as authored on one Agent definition. Only time triggers exist; a
-- second kind adds its own declaration shape and identity rule.
CREATE TABLE agent_triggers (
    environment_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    key text NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 256),
    cron text NOT NULL CHECK (cron<>''),
    timezone text NOT NULL CHECK (timezone<>''),
    input bytea NOT NULL,
    slack_destination text CHECK (slack_destination ~ '^[CG][A-Z0-9]{1,99}$'),
    PRIMARY KEY (environment_id,agent_id,deployment_id,key),
    FOREIGN KEY (environment_id,agent_id,deployment_id) REFERENCES agent_definitions(environment_id,agent_id,deployment_id)
);

-- The interval during which one promotion's declaration fires, with its cursor
-- and the Slack channel binding pinned at promotion.
CREATE TABLE trigger_activations (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    agent_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    trigger_key text NOT NULL,
    active_from timestamptz NOT NULL,
    active_until timestamptz,
    next_fire_at timestamptz NOT NULL,
    slack_channel_id uuid,
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,id,agent_id,trigger_key),
    FOREIGN KEY (environment_id,agent_id,deployment_id,trigger_key) REFERENCES agent_triggers(environment_id,agent_id,deployment_id,key),
    CHECK (active_until IS NULL OR active_until >= active_from),
    CHECK (next_fire_at >= active_from),
    FOREIGN KEY(slack_channel_id,environment_id) REFERENCES slack_channel_bindings(id,environment_id)
);

CREATE UNIQUE INDEX trigger_activations_current ON trigger_activations(environment_id,agent_id,trigger_key) WHERE active_until IS NULL;

CREATE INDEX trigger_activations_due ON trigger_activations(next_fire_at,environment_id,id) WHERE active_until IS NULL OR next_fire_at<active_until;

CREATE TABLE sessions (
    close_after_idle_ms bigint CHECK (close_after_idle_ms > 0),
    idle_since timestamptz,
    history_retention_mode text NOT NULL CHECK (history_retention_mode IN ('duration','until_environment_deletion')),
    history_retention_seconds bigint,
    history_eligible_at timestamptz,
    -- Exact Unix seconds support every positive bigint duration without timestamp overflow.
    history_expires_at numeric,
    history_expired_at timestamptz,
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    agent_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    computer_id uuid,
    root_session_id uuid NOT NULL,
    parent_session_id uuid,
    requester_session_id uuid,
    origin_turn_id uuid,
    causal_depth integer NOT NULL CHECK (causal_depth >= 0),
    key text CHECK (octet_length(key) BETWEEN 1 AND 512),
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','closing','closed','cancelled')),
    authority_generation bigint NOT NULL DEFAULT 1 CHECK (authority_generation > 0),
    next_turn_seq bigint NOT NULL DEFAULT 1 CHECK (next_turn_seq > 0),
    next_event_seq bigint NOT NULL DEFAULT 1 CHECK (next_event_seq > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    creation_caller_kind text CHECK (creation_caller_kind IN ('user','api_key','session')),
    creation_caller_id uuid,
    creation_method text CHECK (creation_method IN ('create','spawn')),
    creation_idempotency_key text CHECK (octet_length(creation_idempotency_key) BETWEEN 1 AND 512),
    creation_digest bytea CHECK (octet_length(creation_digest)=32),
    creation_turn_id uuid,
    CHECK (num_nonnulls(creation_caller_kind,creation_caller_id,creation_method,creation_digest) IN (0,4)),
    CHECK (creation_idempotency_key IS NULL OR creation_digest IS NOT NULL),
    CHECK (creation_turn_id IS NULL OR (creation_digest IS NOT NULL AND computer_id IS NOT NULL)),
    UNIQUE(environment_id,creation_caller_kind,creation_caller_id,creation_method,agent_id,creation_idempotency_key),
    CHECK ((history_retention_mode='duration' AND history_retention_seconds IS NOT NULL AND history_retention_seconds>0)
        OR (history_retention_mode='until_environment_deletion' AND history_retention_seconds IS NULL)),
    CHECK (history_eligible_at IS NULL OR status IN ('closed','cancelled')),
    CHECK ((history_eligible_at IS NULL AND history_expires_at IS NULL)
        OR (history_eligible_at IS NOT NULL AND ((history_retention_mode='duration' AND history_expires_at IS NOT NULL AND history_expires_at=EXTRACT(epoch FROM history_eligible_at)+history_retention_seconds)
          OR (history_retention_mode='until_environment_deletion' AND history_expires_at IS NULL)))),
    CHECK (history_expired_at IS NULL OR (history_eligible_at IS NOT NULL AND history_expires_at IS NOT NULL AND EXTRACT(epoch FROM history_expired_at)>=history_expires_at)),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,id,computer_id),
    UNIQUE (environment_id,id,root_session_id),
    UNIQUE (environment_id,agent_id,key),
    FOREIGN KEY (environment_id,agent_id,deployment_id) REFERENCES agent_definitions(environment_id,agent_id,deployment_id),
    FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
    FOREIGN KEY (environment_id,root_session_id) REFERENCES sessions(environment_id,id),
    FOREIGN KEY (environment_id,parent_session_id,root_session_id) REFERENCES sessions(environment_id,id,root_session_id),
    FOREIGN KEY (environment_id,requester_session_id) REFERENCES sessions(environment_id,id),
    CHECK ((parent_session_id IS NULL AND root_session_id=id)
        OR (parent_session_id IS NOT NULL AND root_session_id=parent_session_id AND root_session_id<>id AND requester_session_id IS NOT NULL AND requester_session_id=parent_session_id)),
    CHECK (origin_turn_id IS NULL OR requester_session_id IS NOT NULL),
    CHECK ((requester_session_id IS NULL AND causal_depth=0) OR (requester_session_id IS NOT NULL AND causal_depth>0))
);

CREATE INDEX sessions_idle_policy ON sessions(environment_id,id) WHERE status='open' AND close_after_idle_ms IS NOT NULL;

CREATE INDEX sessions_closing ON sessions(environment_id,id) WHERE status='closing';

CREATE INDEX sessions_children ON sessions(environment_id,parent_session_id) WHERE parent_session_id IS NOT NULL;

CREATE TABLE session_processes (
    environment_id uuid NOT NULL,
    session_id uuid NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    computer_id uuid NOT NULL,
    computer_lease_epoch bigint NOT NULL,
    status text NOT NULL CHECK (status IN ('starting','ready','stopping','lost','stopped')),
    attachment_sequence bigint NOT NULL DEFAULT 0 CHECK (attachment_sequence >= 0),
    control_sequence bigint NOT NULL DEFAULT 0 CHECK (control_sequence >= 0),
    control_attachment bigint NOT NULL DEFAULT 0 CHECK (control_attachment >= 0),
    control_generation bigint NOT NULL DEFAULT 0 CHECK (control_generation >= 0),
    control_kind text CHECK (control_kind IN ('suspend','resume','shutdown')),
    stop_sequence bigint NOT NULL DEFAULT 0 CHECK (stop_sequence >= 0),
    stop_turn_id uuid,
    stop_deadline_at timestamptz,
    CHECK (stop_turn_id IS NULL OR stop_deadline_at IS NOT NULL),
    control_acknowledged_at timestamptz,
    failure_recorded_at timestamptz,
    control_error text,
    fenced_at timestamptz,
    stdout_accepted_through bigint NOT NULL DEFAULT 0 CHECK (stdout_accepted_through>=0),
    stdout_byte_offset bigint NOT NULL DEFAULT 0 CHECK (stdout_byte_offset>=0),
    stdout_ended boolean NOT NULL DEFAULT false,
    stdout_end_complete boolean NOT NULL DEFAULT false CHECK (NOT stdout_end_complete OR stdout_ended),
    stdout_gapped boolean NOT NULL DEFAULT false,
    stdout_expired_through bigint NOT NULL DEFAULT 0 CHECK (stdout_expired_through BETWEEN 0 AND stdout_accepted_through),
    stdout_last_sequence bigint,
    stdout_last_digest bytea CHECK (stdout_last_digest IS NULL OR octet_length(stdout_last_digest)=32),
    stdout_last_accepted_at timestamptz,
    stderr_accepted_through bigint NOT NULL DEFAULT 0 CHECK (stderr_accepted_through>=0),
    stderr_byte_offset bigint NOT NULL DEFAULT 0 CHECK (stderr_byte_offset>=0),
    stderr_ended boolean NOT NULL DEFAULT false,
    stderr_end_complete boolean NOT NULL DEFAULT false CHECK (NOT stderr_end_complete OR stderr_ended),
    stderr_gapped boolean NOT NULL DEFAULT false,
    stderr_expired_through bigint NOT NULL DEFAULT 0 CHECK (stderr_expired_through BETWEEN 0 AND stderr_accepted_through),
    stderr_last_sequence bigint,
    stderr_last_digest bytea CHECK (stderr_last_digest IS NULL OR octet_length(stderr_last_digest)=32),
    stderr_last_accepted_at timestamptz,
    CHECK ((stdout_accepted_through=0 AND stdout_last_sequence IS NULL AND stdout_last_digest IS NULL AND stdout_last_accepted_at IS NULL AND NOT stdout_ended)
        OR (stdout_accepted_through>0 AND stdout_last_sequence IS NOT NULL AND stdout_last_sequence BETWEEN 1 AND stdout_accepted_through AND stdout_last_accepted_at IS NOT NULL
            AND (stdout_last_digest IS NOT NULL OR stdout_expired_through=stdout_accepted_through))),
    CHECK ((stderr_accepted_through=0 AND stderr_last_sequence IS NULL AND stderr_last_digest IS NULL AND stderr_last_accepted_at IS NULL AND NOT stderr_ended)
        OR (stderr_accepted_through>0 AND stderr_last_sequence IS NOT NULL AND stderr_last_sequence BETWEEN 1 AND stderr_accepted_through AND stderr_last_accepted_at IS NOT NULL
            AND (stderr_last_digest IS NOT NULL OR stderr_expired_through=stderr_accepted_through))),
    PRIMARY KEY (environment_id,session_id,epoch),
    UNIQUE (environment_id,computer_id,session_id,epoch),
    FOREIGN KEY (environment_id,session_id,computer_id) REFERENCES sessions(environment_id,id,computer_id),
    FOREIGN KEY (environment_id,computer_id,computer_lease_epoch) REFERENCES computer_leases(environment_id,computer_id,epoch),
    CHECK (status <> 'stopped' OR fenced_at IS NOT NULL)
);

CREATE UNIQUE INDEX session_processes_one_writer ON session_processes(environment_id,session_id) WHERE fenced_at IS NULL;

CREATE INDEX session_processes_computer_custody
    ON session_processes(environment_id,computer_id,computer_lease_epoch,session_id,epoch)
    WHERE fenced_at IS NULL;

CREATE TABLE secret_exposures (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL,
    image_build_id uuid,
    session_id uuid,
    process_epoch bigint CHECK (process_epoch>0),
    command_id uuid,
    secret_id uuid NOT NULL,
    version_id uuid NOT NULL,
    revocation_generation bigint NOT NULL CHECK (revocation_generation>=0),
    exposed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (num_nonnulls(image_build_id,session_id,command_id)=1),
    CHECK ((session_id IS NULL)=(process_epoch IS NULL)),
    FOREIGN KEY(environment_id,image_build_id) REFERENCES image_builds(environment_id,id),
    FOREIGN KEY(environment_id,session_id,process_epoch) REFERENCES session_processes(environment_id,session_id,epoch),
    FOREIGN KEY(environment_id,command_id) REFERENCES commands(environment_id,id),
    FOREIGN KEY(environment_id,secret_id) REFERENCES secrets(environment_id,id),
    FOREIGN KEY(secret_id,version_id) REFERENCES secret_versions(secret_id,id)
);

CREATE UNIQUE INDEX secret_exposures_preparation ON secret_exposures(environment_id,image_build_id,secret_id) WHERE image_build_id IS NOT NULL;

CREATE UNIQUE INDEX secret_exposures_process ON secret_exposures(environment_id,session_id,process_epoch,secret_id) WHERE session_id IS NOT NULL;

CREATE UNIQUE INDEX secret_exposures_command ON secret_exposures(environment_id,command_id,secret_id) WHERE command_id IS NOT NULL;

CREATE INDEX secret_exposures_secret ON secret_exposures(environment_id,secret_id);

CREATE TABLE turns (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    computer_id uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','finalizing','completed','failed','interrupted','cancelled')),
    caller_kind text NOT NULL CHECK (caller_kind IN ('user','api_key','session','schedule')),
    caller_id uuid NOT NULL,
    origin_turn_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    admission_method text NOT NULL CHECK (admission_method IN ('create','spawn','enqueue','slack_route','schedule')),
    target_id uuid NOT NULL,
    idempotency_key text CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    input bytea,
    result jsonb,
    error_code text,
    error_message bytea,
    payload_expired_at timestamptz,
    progress_bytes bigint NOT NULL DEFAULT 0 CHECK (progress_bytes BETWEEN 0 AND 8388608),
    response_id uuid,
    response_digest bytea CHECK (octet_length(response_digest)=32),
    response bytea,
    response_staged_at timestamptz,
    response_expired_at timestamptz,
    process_epoch bigint,
    messages_registered_at timestamptz,
    next_message_seq bigint NOT NULL DEFAULT 1 CHECK (next_message_seq>0),
    processing_closed_at timestamptz,
    result_recorded_at timestamptz,
    result_digest bytea CHECK (octet_length(result_digest)=32),
    drain_evidence text,
    completion_save_id uuid,
    has_recorded_result boolean GENERATED ALWAYS AS (result_recorded_at IS NOT NULL) STORED,
    started_at timestamptz,
    deadline_at timestamptz,
    terminal_at timestamptz,
    completion_published boolean GENERATED ALWAYS AS (true) STORED,
    CHECK ((payload_expired_at IS NULL AND ((error_code IS NULL)=(error_message IS NULL)))
      OR (payload_expired_at IS NOT NULL AND input IS NULL AND result IS NULL AND response IS NULL AND error_message IS NULL AND terminal_at IS NOT NULL)),
    CHECK (error_code IS NULL OR (status='failed' AND error_code<>'')),
    CHECK ((response_id IS NULL)=(response_digest IS NULL)),
    CHECK ((response_id IS NULL)=(response_staged_at IS NULL)),
    CHECK ((response_id IS NULL AND response IS NULL AND response_expired_at IS NULL)
      OR (response_id IS NOT NULL AND ((response IS NOT NULL AND response_expired_at IS NULL) OR (response IS NULL AND response_expired_at IS NOT NULL)))),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,session_id,id),
    UNIQUE (environment_id,session_id,id,process_epoch),
    UNIQUE (environment_id,id,computer_id),
    UNIQUE (environment_id,id,computer_id,has_recorded_result),
    UNIQUE (environment_id,session_id,seq),
    UNIQUE (environment_id,caller_kind,caller_id,admission_method,target_id,idempotency_key),
    FOREIGN KEY (environment_id,caller_id,origin_turn_id) REFERENCES turns(environment_id,session_id,id),
    CHECK (origin_turn_id IS NULL OR caller_kind='session'),
    FOREIGN KEY (environment_id,session_id,computer_id) REFERENCES sessions(environment_id,id,computer_id),
    FOREIGN KEY (environment_id,session_id,process_epoch) REFERENCES session_processes(environment_id,session_id,epoch),
    CHECK (status NOT IN ('running','finalizing','completed') OR (process_epoch IS NOT NULL AND started_at IS NOT NULL)),
    CHECK (status NOT IN ('finalizing','completed') OR (processing_closed_at IS NOT NULL AND result_recorded_at IS NOT NULL AND result_digest IS NOT NULL AND drain_evidence IS NOT NULL)),
    CHECK (status <> 'finalizing' OR result IS NOT NULL),
    CHECK ((result_recorded_at IS NULL) = (result_digest IS NULL)),
    CHECK ((status IN ('completed','failed','interrupted','cancelled')) = (terminal_at IS NOT NULL)),
    CHECK ((status='completed') = (completion_save_id IS NOT NULL))
);

ALTER TABLE sessions ADD FOREIGN KEY (environment_id,id,creation_turn_id) REFERENCES turns(environment_id,session_id,id) DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX turns_one_active ON turns(environment_id,session_id) WHERE status IN ('running','finalizing');

CREATE INDEX turns_queue ON turns(environment_id,session_id,seq) WHERE status='queued';

CREATE INDEX turns_deadlines ON turns(deadline_at,environment_id,session_id) WHERE status IN ('running','finalizing') AND deadline_at IS NOT NULL;

-- One scheduled instant of an Agent's trigger, across every activation and
-- deployment: the identity is (agent, key, scheduled_at).
CREATE TABLE trigger_occurrences (
    environment_id uuid NOT NULL,
    activation_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    trigger_key text NOT NULL,
    scheduled_at timestamptz NOT NULL,
    session_id uuid,
    turn_id uuid,
    disposition text NOT NULL CHECK (disposition IN ('admitted','rejected','missed')),
    reason text,
    evaluated_at timestamptz NOT NULL,
    PRIMARY KEY (environment_id,agent_id,trigger_key,scheduled_at),
    CHECK ((disposition = 'admitted' AND session_id IS NOT NULL AND turn_id IS NOT NULL)
     OR (disposition <> 'admitted' AND session_id IS NULL AND turn_id IS NULL AND reason IS NOT NULL)),
    FOREIGN KEY (environment_id,activation_id,agent_id,trigger_key)
    REFERENCES trigger_activations(environment_id,id,agent_id,trigger_key),
    FOREIGN KEY (environment_id,session_id,turn_id) REFERENCES turns(environment_id,session_id,id)
);

CREATE INDEX trigger_occurrences_activation ON trigger_occurrences(environment_id,activation_id);

CREATE INDEX trigger_occurrences_turn ON trigger_occurrences(environment_id,session_id,turn_id);

CREATE TABLE session_holds (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    scope text NOT NULL CHECK (scope IN ('local','subtree')),
    reason text NOT NULL CHECK (octet_length(reason) <= 4096),
    issuer_kind text NOT NULL DEFAULT 'system' CHECK (issuer_kind IN ('user','api_key','session','system')),
    issuer_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    released_at timestamptz,
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,session_id,id),
    FOREIGN KEY (environment_id,session_id) REFERENCES sessions(environment_id,id),
    CHECK ((issuer_kind='system') = (issuer_id IS NULL))
);

CREATE INDEX session_holds_active ON session_holds(environment_id,session_id) WHERE released_at IS NULL;

CREATE TABLE session_controls (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('interrupt','turn_interrupt','resume','close','cancel')),
    caller_kind text NOT NULL CHECK (caller_kind IN ('user','api_key','session')),
    caller_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    hold_id uuid,
    turn_id uuid,
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (environment_id,id),
    UNIQUE NULLS NOT DISTINCT (environment_id,session_id,turn_id,caller_kind,caller_id,kind,idempotency_key),
    FOREIGN KEY (environment_id,session_id,turn_id) REFERENCES turns(environment_id,session_id,id),
    CHECK ((kind='turn_interrupt') = (turn_id IS NOT NULL)),
    FOREIGN KEY (environment_id,session_id) REFERENCES sessions(environment_id,id),
    FOREIGN KEY (environment_id,session_id,hold_id) REFERENCES session_holds(environment_id,session_id,id),
    CHECK ((kind IN ('interrupt','resume')) = (hold_id IS NOT NULL)),
    UNIQUE(environment_id,session_id,id)
);

ALTER TABLE session_processes ADD FOREIGN KEY (environment_id,session_id,stop_turn_id) REFERENCES turns(environment_id,session_id,id);
CREATE INDEX session_processes_stop_due ON session_processes(stop_deadline_at) WHERE stop_deadline_at IS NOT NULL AND fenced_at IS NULL;

CREATE UNIQUE INDEX session_controls_interrupt_hold ON session_controls(environment_id,hold_id) WHERE kind='interrupt';

CREATE TABLE computer_saves (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    computer_id uuid NOT NULL,
    computer_lease_epoch bigint NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    turn_id uuid,
    status text NOT NULL DEFAULT 'requested' CHECK (status IN ('requested','captured','published','failed')),
    requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    flush_acknowledged_at timestamptz,
    captured_at timestamptz,
    captured_root_digest bytea CHECK (octet_length(captured_root_digest)=32),
    root_id uuid,
    payload_retired_at timestamptz,
    capture_evidence text,
    publication_evidence text,
    failure_evidence text,
    published boolean GENERATED ALWAYS AS (status='published') STORED,
    requires_recorded_result boolean GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,computer_id,seq),
    UNIQUE (environment_id,computer_id,id,published),
    UNIQUE (environment_id,computer_id,turn_id,id,published),
    UNIQUE (environment_id,turn_id),
    FOREIGN KEY (environment_id,computer_id,computer_lease_epoch) REFERENCES computer_leases(environment_id,computer_id,epoch),
    FOREIGN KEY (environment_id,turn_id,computer_id,requires_recorded_result) REFERENCES turns(environment_id,id,computer_id,has_recorded_result),
    CHECK (status<>'requested' OR (captured_root_digest IS NULL AND capture_evidence IS NULL AND flush_acknowledged_at IS NULL AND captured_at IS NULL)),
    CHECK (status NOT IN ('captured','published') OR (flush_acknowledged_at IS NOT NULL AND captured_at IS NOT NULL AND captured_root_digest IS NOT NULL AND capture_evidence IS NOT NULL)),
    CHECK (captured_at IS NULL OR (captured_at >= requested_at AND captured_at >= flush_acknowledged_at)),
    CHECK ((status='published') = (publication_evidence IS NOT NULL)),
    CHECK (status='published' OR (root_id IS NULL AND payload_retired_at IS NULL)),
    CHECK (status<>'published' OR ((root_id IS NOT NULL) <> (payload_retired_at IS NOT NULL))),
    CHECK ((status='failed') = (failure_evidence IS NOT NULL)),
    UNIQUE (environment_id,computer_id,id),
    FOREIGN KEY (environment_id,root_id)
    REFERENCES disk_roots(environment_id,id)
);

CREATE TABLE disk_object_pins (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL,
    save_id uuid,
    image_build_id uuid,
    digest text NOT NULL,
    CHECK (num_nonnulls(save_id,image_build_id)=1),
    UNIQUE NULLS NOT DISTINCT (environment_id,save_id,image_build_id,digest),
    FOREIGN KEY (environment_id,save_id) REFERENCES computer_saves(environment_id,id),
    FOREIGN KEY (environment_id,image_build_id) REFERENCES image_builds(environment_id,id),
    FOREIGN KEY (environment_id,digest) REFERENCES disk_objects(environment_id,digest)
);

CREATE INDEX disk_object_pins_digest ON disk_object_pins(environment_id,digest);

CREATE TABLE checkpoints (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    computer_id uuid NOT NULL,
    source_lease_epoch bigint NOT NULL,
    control_version bigint NOT NULL CHECK (control_version > 0),
    disk_save_id uuid NOT NULL,
    status text NOT NULL CHECK (status IN ('capturing','sealed','ready','restoring','aborting','consumed','cancelled','lost')),
    capture_request bytea CHECK (octet_length(capture_request) BETWEEN 1 AND 16777216),
    capture_expires_at timestamptz NOT NULL,
    manifest bytea CHECK (octet_length(manifest) BETWEEN 1 AND 1048576),
    vm_platform_id text REFERENCES vm_platforms(id),
    ready_at timestamptz,
    target_lease_epoch bigint,
    restore_control_version bigint CHECK (restore_control_version > 0),
    restore_identity bytea CHECK (octet_length(restore_identity)=32),
    abort_identity bytea CHECK (octet_length(abort_identity)=32),
    controls_reconciled_at timestamptz,
    capture_digest bytea NOT NULL CHECK (octet_length(capture_digest)=32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    terminal_evidence text,
    CHECK ((manifest IS NULL) = (vm_platform_id IS NULL)),
    CHECK (ready_at IS NULL OR manifest IS NOT NULL),
    CHECK (status NOT IN ('ready','restoring') OR ready_at IS NOT NULL),
    CHECK ((target_lease_epoch IS NULL) = (restore_control_version IS NULL)),
    CHECK (target_lease_epoch IS NULL OR target_lease_epoch > source_lease_epoch),
    CHECK (status <> 'restoring' OR target_lease_epoch IS NOT NULL),
    CHECK (restore_identity IS NULL OR target_lease_epoch IS NOT NULL),
    FOREIGN KEY (environment_id,computer_id,target_lease_epoch) REFERENCES computer_leases(environment_id,computer_id,epoch),
    CHECK (controls_reconciled_at IS NULL OR status='consumed'),
    PRIMARY KEY (environment_id,id),
    UNIQUE (environment_id,computer_id,id),
    UNIQUE (environment_id,computer_id,control_version),
    FOREIGN KEY (environment_id,computer_id,source_lease_epoch) REFERENCES computer_leases(environment_id,computer_id,epoch),
    FOREIGN KEY (environment_id,computer_id,disk_save_id) REFERENCES computer_saves(environment_id,computer_id,id),
    CHECK ((status IN ('cancelled','lost')) = (terminal_evidence IS NOT NULL)),
    CHECK ((status IN ('cancelled','lost') OR controls_reconciled_at IS NOT NULL) = (capture_request IS NULL))
);

CREATE UNIQUE INDEX checkpoints_one_pending ON checkpoints(environment_id,computer_id)
    WHERE status IN ('capturing','sealed','ready','restoring','aborting');

CREATE TABLE checkpoint_processes (
    environment_id uuid NOT NULL,
    computer_id uuid NOT NULL,
    checkpoint_id uuid NOT NULL,
    session_id uuid NOT NULL,
    process_epoch bigint NOT NULL,
    PRIMARY KEY (environment_id,checkpoint_id,session_id),
    FOREIGN KEY (environment_id,computer_id,checkpoint_id) REFERENCES checkpoints(environment_id,computer_id,id),
    FOREIGN KEY (environment_id,computer_id,session_id,process_epoch) REFERENCES session_processes(environment_id,computer_id,session_id,epoch)
);

CREATE TABLE checkpoint_objects (
    environment_id uuid NOT NULL,
    checkpoint_id uuid NOT NULL,
    role text NOT NULL CHECK (role IN ('vm_config','vm_state','memory','scratch_disk')),
    digest text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    media_type text NOT NULL,
    availability_required boolean GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (environment_id,checkpoint_id,role),
    FOREIGN KEY (environment_id,checkpoint_id) REFERENCES checkpoints(environment_id,id),
    FOREIGN KEY (digest,size_bytes,availability_required) REFERENCES cas_blobs(digest,size_bytes,not_retired)
);

CREATE TABLE session_events (
    environment_id uuid NOT NULL,
    session_id uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    turn_id uuid,
    kind text NOT NULL,
    data bytea,
    payload_expired_at timestamptz,
    operation_id uuid,
    operation_digest bytea CHECK (octet_length(operation_digest)=32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((operation_id IS NULL)=(operation_digest IS NULL)),
    CHECK (operation_id IS NULL OR (turn_id IS NOT NULL AND kind='turn.output')),
    CHECK ((data IS NULL)=(payload_expired_at IS NOT NULL)),
    UNIQUE (environment_id,session_id,turn_id,operation_id),
    PRIMARY KEY (environment_id,session_id,seq),
    FOREIGN KEY (environment_id,session_id) REFERENCES sessions(environment_id,id),
    FOREIGN KEY (environment_id,session_id,turn_id) REFERENCES turns(environment_id,session_id,id)
);

CREATE INDEX session_events_expired_floor ON session_events(environment_id,session_id,seq)
    WHERE payload_expired_at IS NOT NULL;

CREATE TABLE turn_asks (
    environment_id uuid NOT NULL,
    session_id uuid NOT NULL,
    turn_id uuid NOT NULL,
    id uuid NOT NULL,
    process_epoch bigint NOT NULL,
    created_event_seq bigint NOT NULL,
    question_digest bytea NOT NULL CHECK (octet_length(question_digest)=32),
    question bytea,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','responded','cancelled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    responded_at timestamptz,
    cancelled_at timestamptz,
    response_id text CHECK (octet_length(response_id) BETWEEN 1 AND 512),
    answer_digest bytea CHECK (octet_length(answer_digest)=32),
    answer bytea,
    responded_by_user_id uuid REFERENCES users(id),
    responded_by_api_key_id uuid REFERENCES api_keys(id),
    payload_expired_at timestamptz,
    PRIMARY KEY(environment_id,session_id,turn_id,id),
    UNIQUE(environment_id,session_id,created_event_seq),
    FOREIGN KEY(environment_id,session_id,turn_id,process_epoch) REFERENCES turns(environment_id,session_id,id,process_epoch),
    FOREIGN KEY(environment_id,session_id,created_event_seq) REFERENCES session_events(environment_id,session_id,seq),
    CHECK ((status='responded')=(responded_at IS NOT NULL)),
    CHECK ((status='cancelled')=(cancelled_at IS NOT NULL)),
    CHECK ((status='responded' AND response_id IS NOT NULL AND answer_digest IS NOT NULL AND num_nonnulls(responded_by_user_id,responded_by_api_key_id)=1)
       OR (status<>'responded' AND response_id IS NULL AND answer_digest IS NULL AND responded_by_user_id IS NULL AND responded_by_api_key_id IS NULL)),
    CHECK ((payload_expired_at IS NULL AND question IS NOT NULL AND ((status='responded')=(answer IS NOT NULL)))
       OR (payload_expired_at IS NOT NULL AND status<>'pending' AND question IS NULL AND answer IS NULL)),
    UNIQUE(environment_id,id)
);

CREATE INDEX turn_asks_pending ON turn_asks(environment_id,session_id,turn_id) WHERE status='pending';

CREATE TABLE turn_messages (
    environment_id uuid NOT NULL,
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    turn_id uuid NOT NULL,
    process_epoch bigint NOT NULL,
    seq bigint NOT NULL CHECK (seq>0),
    message bytea,
    payload_expired_at timestamptz,
    caller_kind text NOT NULL CHECK (caller_kind IN ('user','api_key','session')),
    caller_id uuid NOT NULL,
    origin_turn_id uuid,
    admission_method text NOT NULL CHECK (admission_method IN ('slack_route','turn_send')),
    target_id uuid NOT NULL,
    idempotency_key text CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    status text NOT NULL DEFAULT 'admitted' CHECK (status IN ('admitted','started','delivered','rejected')),
    rejection_reason text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    started_at timestamptz,
    terminal_at timestamptz,
    PRIMARY KEY(environment_id,id),
    UNIQUE(environment_id,session_id,turn_id,id),
    UNIQUE(environment_id,turn_id,seq),
    UNIQUE(environment_id,caller_kind,caller_id,admission_method,target_id,idempotency_key),
    FOREIGN KEY(environment_id,session_id,turn_id,process_epoch) REFERENCES turns(environment_id,session_id,id,process_epoch),
    FOREIGN KEY(environment_id,caller_id,origin_turn_id) REFERENCES turns(environment_id,session_id,id),
    CHECK (origin_turn_id IS NULL OR caller_kind='session'),
    CHECK ((admission_method='slack_route' AND target_id=session_id) OR (admission_method='turn_send' AND target_id=turn_id)),
    CHECK ((message IS NULL)=(payload_expired_at IS NOT NULL)),
    CHECK (payload_expired_at IS NULL OR terminal_at IS NOT NULL),
    CHECK ((status IN ('delivered','rejected'))=(terminal_at IS NOT NULL)),
    CHECK (status NOT IN ('started','delivered') OR started_at IS NOT NULL),
    CHECK ((status='rejected')=(rejection_reason IS NOT NULL))
);

CREATE UNIQUE INDEX turn_messages_one_started ON turn_messages(environment_id,turn_id) WHERE status='started';

CREATE INDEX turn_messages_pending ON turn_messages(environment_id,session_id,turn_id,seq) WHERE status IN ('admitted','started');

-- One queue retains accepted diagnostics and Deployment events. The typed owner
-- remains while payload delivery is outstanding; only diagnostic inserts share
-- the bounded admission gate. Deployment delivery has its own claims/lifetime.
CREATE TABLE diagnostic_chunks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    environment_id uuid NOT NULL REFERENCES environments(id),
    session_id uuid,
    process_epoch bigint,
    image_build_id uuid,
    preparation_epoch bigint,
    command_id uuid,
    source_kind text GENERATED ALWAYS AS (CASE WHEN session_id IS NOT NULL THEN 'session' WHEN image_build_id IS NOT NULL THEN 'image_build' ELSE 'computer_command' END) STORED NOT NULL,
    source_id uuid GENERATED ALWAYS AS (COALESCE(session_id,image_build_id,command_id)) STORED NOT NULL,
    producer_epoch bigint GENERATED ALWAYS AS (COALESCE(process_epoch,preparation_epoch,1)) STORED NOT NULL,
    stream text NOT NULL CHECK (stream IN ('stdout','stderr')),
    sequence bigint NOT NULL,
    through_sequence bigint NOT NULL,
    byte_offset bigint NOT NULL,
    through_byte_offset bigint NOT NULL,
    kind text NOT NULL,
    observed_at_unix_nano bigint NOT NULL,
    data bytea NOT NULL,
    dropped_bytes bigint NOT NULL,
    complete boolean NOT NULL,
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    export_claim uuid,
    export_after timestamptz NOT NULL DEFAULT '-infinity',
    ingest_size_bytes bigint GENERATED ALWAYS AS (octet_length(data)::bigint) STORED NOT NULL,
    CHECK (num_nonnulls(session_id,image_build_id,command_id)=1),
    CHECK ((session_id IS NULL)=(process_epoch IS NULL)),
    CHECK ((image_build_id IS NULL)=(preparation_epoch IS NULL)),
    FOREIGN KEY(environment_id,session_id,process_epoch) REFERENCES session_processes(environment_id,session_id,epoch),
    FOREIGN KEY(environment_id,image_build_id,preparation_epoch) REFERENCES image_builds(environment_id,id,executor_epoch),
    FOREIGN KEY(environment_id,command_id) REFERENCES commands(environment_id,id),
    CHECK (sequence>0 AND through_sequence>=sequence AND byte_offset>=0 AND through_byte_offset>=byte_offset AND observed_at_unix_nano>0
        AND dropped_bytes>=0
        AND ((kind='data' AND sequence=through_sequence AND octet_length(data)>0 AND dropped_bytes=0 AND NOT complete AND through_byte_offset=byte_offset+octet_length(data))
          OR (kind='gap' AND octet_length(data)=0 AND dropped_bytes>=through_sequence-sequence+1 AND NOT complete AND through_byte_offset=byte_offset+dropped_bytes)
          OR (kind='end' AND sequence=through_sequence AND octet_length(data)=0 AND dropped_bytes=0 AND through_byte_offset=byte_offset)))
);

CREATE TABLE deployment_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    environment_id uuid NOT NULL REFERENCES environments(id),
    deployment_id uuid NOT NULL,
    kind text NOT NULL CHECK (length(kind)>0),
    message text NOT NULL DEFAULT '',
    payload jsonb NOT NULL DEFAULT '{}',
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY(environment_id,deployment_id) REFERENCES deployments(environment_id,id)
);

CREATE UNIQUE INDEX diagnostic_chunks_identity ON diagnostic_chunks(environment_id,source_kind,source_id,producer_epoch,stream,sequence);

CREATE INDEX diagnostic_chunks_queue ON diagnostic_chunks(id) INCLUDE(environment_id,source_kind,source_id,producer_epoch,ingest_size_bytes);

CREATE INDEX diagnostic_chunks_environment ON diagnostic_chunks(environment_id,id) INCLUDE(ingest_size_bytes);

CREATE INDEX diagnostic_chunks_producer ON diagnostic_chunks(environment_id,source_kind,source_id,producer_epoch,id) INCLUDE(ingest_size_bytes);

CREATE INDEX diagnostic_chunks_pending ON diagnostic_chunks(source_kind,export_after,id);

CREATE INDEX diagnostic_chunks_expiry ON diagnostic_chunks(accepted_at,id);

CREATE INDEX deployment_events_deployment ON deployment_events(environment_id,deployment_id,id);

CREATE INDEX deployment_events_accepted ON deployment_events(accepted_at,id);

CREATE TABLE slack_threads (
    id uuid PRIMARY KEY,
    deleted_at timestamptz,
    channel_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    front_session_id uuid NOT NULL,
    -- Only a root Session owns a Slack route: the carrier equals the front
    -- Session and the key it references holds only rows whose root is itself.
    front_root_session_id uuid GENERATED ALWAYS AS (front_session_id) STORED NOT NULL,
    org_id uuid NOT NULL,
    team_id text NOT NULL,
    slack_channel_id text NOT NULL,
    thread_ts text CHECK (thread_ts<>''),
    opening_post_key text CHECK (opening_post_key<>''),
    recipient_team_id text,
    recipient_user_id text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    desired_status text NOT NULL DEFAULT 'active' CHECK (desired_status IN ('active','processing','suspended')),
    desired_revision bigint NOT NULL DEFAULT 1 CHECK (desired_revision>0),
    confirmed_revision bigint NOT NULL DEFAULT 0 CHECK (confirmed_revision>=0 AND confirmed_revision<=desired_revision),
    status_confirmation text NOT NULL DEFAULT 'unknown' CHECK (status_confirmation IN ('unknown','acknowledged')),
    stream_status_repair boolean NOT NULL DEFAULT false,
    inflight_method text,
    inflight_payload bytea,
    inflight_digest bytea CHECK (octet_length(inflight_digest)=32),
    inflight_revision bigint CHECK (inflight_revision>0 AND inflight_revision<=desired_revision),
    inflight_attempt_id uuid,
    claim_epoch bigint NOT NULL DEFAULT 0 CHECK (claim_epoch>=0),
    claimed_until timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    refresh_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivery_error text,
    CHECK (thread_ts IS NOT NULL OR opening_post_key IS NOT NULL),
    UNIQUE(id,environment_id),
    UNIQUE(environment_id,front_session_id),
    FOREIGN KEY(channel_id,environment_id,org_id,team_id,slack_channel_id) REFERENCES slack_channel_bindings(id,environment_id,org_id,team_id,slack_channel_id),
    FOREIGN KEY(environment_id,front_session_id,front_root_session_id) REFERENCES sessions(environment_id,id,root_session_id),
    UNIQUE(org_id,team_id,slack_channel_id,thread_ts),
    UNIQUE(channel_id,opening_post_key),
    CHECK ((recipient_team_id IS NULL)=(recipient_user_id IS NULL)),
    CHECK (num_nonnulls(inflight_method,inflight_payload,inflight_digest,inflight_revision,inflight_attempt_id,claimed_until) IN (0,6))
);

CREATE INDEX slack_threads_remote_root ON slack_threads(thread_ts,channel_id) WHERE thread_ts IS NOT NULL;

CREATE INDEX slack_threads_status_due ON slack_threads(next_attempt_at,refresh_at,id);

CREATE TABLE slack_thread_sources (
    id uuid PRIMARY KEY,
    thread_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    session_id uuid NOT NULL,
    projected_event_seq bigint NOT NULL DEFAULT 0 CHECK (projected_event_seq>=0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(thread_id,id),
    UNIQUE(thread_id,environment_id,id),
    UNIQUE(environment_id,session_id,id),
    UNIQUE(environment_id,session_id),
    FOREIGN KEY(thread_id,environment_id) REFERENCES slack_threads(id,environment_id),
    FOREIGN KEY(environment_id,session_id) REFERENCES sessions(environment_id,id)
);

CREATE TABLE slack_requests (
    id uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES slack_installations(id),
    request_key text NOT NULL CHECK (octet_length(request_key) BETWEEN 1 AND 1024),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    source_occurred_at timestamptz NOT NULL,
    slack_user_id text NOT NULL CHECK (slack_user_id<>''),
    user_id uuid REFERENCES users(id),
    thread_id uuid REFERENCES slack_threads(id),
    environment_id uuid,
    session_id uuid,
    thread_source_id uuid,
    operation text CHECK (operation IN ('start','enqueue','slack_route','answer','stop')),
    message_id uuid,
    payload bytea,
    payload_expired_at timestamptz,
    status text NOT NULL DEFAULT 'received' CHECK (status IN ('received','accepted','rejected')),
    expires_at timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    turn_id uuid,
    ask_id uuid,
    control_id uuid,
    finished_at timestamptz,
    error text,
    UNIQUE(installation_id,request_key),
    UNIQUE(environment_id,session_id,id),
    FOREIGN KEY(environment_id,session_id,thread_source_id) REFERENCES slack_thread_sources(environment_id,session_id,id),
    FOREIGN KEY(thread_id,thread_source_id) REFERENCES slack_thread_sources(thread_id,id),
    FOREIGN KEY(environment_id,session_id,turn_id) REFERENCES turns(environment_id,session_id,id),
    FOREIGN KEY(environment_id,ask_id) REFERENCES turn_asks(environment_id,id),
    FOREIGN KEY(environment_id,session_id,control_id) REFERENCES session_controls(environment_id,session_id,id),
    FOREIGN KEY(environment_id,session_id,turn_id,message_id) REFERENCES turn_messages(environment_id,session_id,turn_id,id),
    CHECK ((environment_id IS NULL)=(session_id IS NULL)),
    CHECK ((status='received')=(finished_at IS NULL)),
    CHECK ((payload IS NULL)=(payload_expired_at IS NOT NULL)),
    CHECK (payload_expired_at IS NULL OR status<>'received'),
    CHECK (status<>'accepted' OR (user_id IS NOT NULL AND thread_id IS NOT NULL AND thread_source_id IS NOT NULL AND environment_id IS NOT NULL AND session_id IS NOT NULL AND operation IS NOT NULL)),
    CHECK ((status='accepted' AND ((operation IN ('start','enqueue') AND turn_id IS NOT NULL AND message_id IS NULL AND ask_id IS NULL AND control_id IS NULL)
        OR (operation='slack_route' AND turn_id IS NOT NULL AND message_id IS NOT NULL AND ask_id IS NULL AND control_id IS NULL)
        OR (operation='answer' AND turn_id IS NULL AND message_id IS NULL AND ask_id IS NOT NULL AND control_id IS NULL)
        OR (operation='stop' AND turn_id IS NULL AND message_id IS NULL AND ask_id IS NULL AND control_id IS NOT NULL)))
        OR (status<>'accepted' AND turn_id IS NULL AND message_id IS NULL AND ask_id IS NULL AND control_id IS NULL))
);

CREATE INDEX slack_requests_input_source ON slack_requests(environment_id,turn_id,message_id) WHERE status='accepted' AND operation IN ('start','enqueue','slack_route');

CREATE INDEX slack_requests_due ON slack_requests(next_attempt_at,id) WHERE status='received';

CREATE INDEX slack_requests_feedback_due ON slack_requests(finished_at,id) WHERE status='rejected' AND payload_expired_at IS NULL;

CREATE INDEX slack_requests_gesture ON slack_requests(installation_id,request_digest);

CREATE TABLE slack_posts (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL,
    session_id uuid NOT NULL,
    thread_source_id uuid NOT NULL,
    thread_id uuid NOT NULL,
    source_thread_id uuid,
    seq bigint NOT NULL CHECK (seq>0),
    post_key text NOT NULL CHECK (post_key<>''),
    continuation_ordinal integer NOT NULL DEFAULT 0 CHECK (continuation_ordinal>=0),
    role text NOT NULL CHECK (role IN ('intermediate','question','response','lifecycle','request_feedback','activity','opening')),
    turn_id uuid,
    ask_id uuid,
    request_id uuid,
    source_start_seq bigint CHECK (source_start_seq>0),
    source_start_offset bigint CHECK (source_start_offset>=0),
    source_end_seq bigint CHECK (source_end_seq>=source_start_seq),
    source_end_offset bigint CHECK (source_end_offset>=0),
    source_digest bytea CHECK (octet_length(source_digest)=32),
    payload bytea,
    payload_digest bytea NOT NULL CHECK (octet_length(payload_digest)=32),
    payload_expired_at timestamptz,
    desired_revision bigint NOT NULL DEFAULT 1 CHECK (desired_revision>0),
    suppressed_revision bigint NOT NULL DEFAULT 0 CHECK (suppressed_revision>=0 AND suppressed_revision<=desired_revision),
    confirmed_revision bigint NOT NULL DEFAULT 0 CHECK (confirmed_revision>=0 AND confirmed_revision<=desired_revision),
    closed_at timestamptz,
    presentation_path text NOT NULL CHECK (presentation_path IN ('post','stream')),
    recipient_team_id text,
    recipient_user_id text,
    stream_state text NOT NULL DEFAULT 'none' CHECK (stream_state IN ('none','open','stopped','uncertain')),
    confirmed_stream_text text NOT NULL DEFAULT '' CHECK (octet_length(confirmed_stream_text)<=65536),
    inflight_stream_text text CHECK (octet_length(inflight_stream_text)<=65536),
    inflight_method text,
    inflight_payload bytea,
    inflight_digest bytea CHECK (octet_length(inflight_digest)=32),
    inflight_revision bigint CHECK (inflight_revision>0 AND inflight_revision<=desired_revision),
    inflight_attempt_id uuid,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sending','posted','uncertain','failed','suppressed')),
    attempt_count bigint NOT NULL DEFAULT 0 CHECK (attempt_count>=0),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    reconciliation_cursor text NOT NULL DEFAULT '' CHECK (octet_length(reconciliation_cursor)<=4096),
    reconciliation_pages integer NOT NULL DEFAULT 0 CHECK (reconciliation_pages>=0),
    reconciliation_paused_at timestamptz,
    delivery_disposed_at timestamptz,
    delivery_disposed_by uuid REFERENCES users(id),
    claimed_until timestamptz,
    claim_epoch bigint NOT NULL DEFAULT 0 CHECK (claim_epoch>=0),
    message_ts text CHECK (message_ts<>''),
    posted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    error text,
    FOREIGN KEY(thread_id,environment_id,thread_source_id) REFERENCES slack_thread_sources(thread_id,environment_id,id),
    FOREIGN KEY(source_thread_id,environment_id) REFERENCES slack_threads(id,environment_id),
    CHECK ((delivery_disposed_at IS NULL)=(delivery_disposed_by IS NULL)),
    CHECK (delivery_disposed_at IS NULL OR closed_at IS NOT NULL),
    UNIQUE(thread_source_id,post_key,continuation_ordinal),
    UNIQUE(thread_source_id,seq),
    FOREIGN KEY(environment_id,session_id,thread_source_id) REFERENCES slack_thread_sources(environment_id,session_id,id),
    FOREIGN KEY(environment_id,session_id,turn_id) REFERENCES turns(environment_id,session_id,id),
    FOREIGN KEY(environment_id,session_id,turn_id,ask_id) REFERENCES turn_asks(environment_id,session_id,turn_id,id),
    FOREIGN KEY(environment_id,session_id,request_id) REFERENCES slack_requests(environment_id,session_id,id),
    CHECK (role='opening' OR source_thread_id IS NULL),
    CHECK (role<>'opening' OR (source_start_seq IS NULL AND continuation_ordinal=0 AND presentation_path='post')),
    CHECK ((role='question')=(ask_id IS NOT NULL)),
    CHECK (role NOT IN ('intermediate','response','question') OR turn_id IS NOT NULL),
    CHECK ((role='request_feedback')=(request_id IS NOT NULL)),
    CHECK (num_nonnulls(source_start_seq,source_start_offset,source_end_seq,source_end_offset,source_digest) IN (0,5)),
    CHECK (source_start_seq<>source_end_seq OR source_end_offset>=source_start_offset),
    CHECK ((recipient_team_id IS NULL)=(recipient_user_id IS NULL)),
    CHECK (presentation_path<>'stream' OR recipient_user_id IS NOT NULL),
    CHECK (presentation_path<>'post' OR stream_state='none'),
    CHECK (num_nonnulls(inflight_method,inflight_payload,inflight_digest,inflight_revision,inflight_attempt_id) IN (0,5)),
    CHECK (status NOT IN ('sending','uncertain') OR inflight_attempt_id IS NOT NULL),
    CHECK ((status='sending')=(claimed_until IS NOT NULL)),
    CHECK (status<>'posted' OR (message_ts IS NOT NULL AND posted_at IS NOT NULL)),
    CHECK (status NOT IN ('failed','suppressed') OR error IS NOT NULL),
    CHECK ((payload IS NULL)=(payload_expired_at IS NOT NULL)),
    CHECK (payload_expired_at IS NULL OR (status IN ('posted','failed','suppressed') AND inflight_attempt_id IS NULL AND closed_at IS NOT NULL AND (status<>'posted' OR desired_revision=confirmed_revision)))
);

CREATE INDEX slack_posts_due ON slack_posts(next_attempt_at,id) WHERE status='pending';

CREATE INDEX slack_posts_claims ON slack_posts(claimed_until,id) WHERE status='sending';

CREATE INDEX slack_posts_turn_delivery ON slack_posts(environment_id,session_id,turn_id,role,seq);

CREATE INDEX slack_posts_remote_message ON slack_posts(message_ts,thread_source_id) WHERE message_ts IS NOT NULL;

CREATE UNIQUE INDEX slack_posts_one_opening ON slack_posts(environment_id,turn_id) WHERE role='opening';

-- These references close ownership cycles and require both tables to exist.

ALTER TABLE environments
    ADD FOREIGN KEY (id, current_deployment_id) REFERENCES deployments(environment_id,id);

ALTER TABLE secrets
    ADD CONSTRAINT secrets_current_version_fk
    FOREIGN KEY (id, current_version_id)
    REFERENCES secret_versions(secret_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE worker_groups
    ADD CONSTRAINT worker_groups_primary_pool_fkey
    FOREIGN KEY (id, primary_pool_id)
    REFERENCES worker_pools(worker_group_id, id)
    ON DELETE RESTRICT;

ALTER TABLE computers
    ADD FOREIGN KEY (environment_id,id,recovery_save_id,recovery_published)
    REFERENCES computer_saves(environment_id,computer_id,id,published);

ALTER TABLE sessions
    ADD FOREIGN KEY (environment_id,requester_session_id,origin_turn_id) REFERENCES turns(environment_id,session_id,id);

ALTER TABLE computer_leases
    ADD FOREIGN KEY (environment_id,computer_id,restored_from_save_id,restored_published)
    REFERENCES computer_saves(environment_id,computer_id,id,published);

ALTER TABLE turns
    ADD FOREIGN KEY (environment_id,computer_id,id,completion_save_id,completion_published)
    REFERENCES computer_saves(environment_id,computer_id,turn_id,id,published);

ALTER TABLE disk_encryption_keys
    ADD FOREIGN KEY (environment_id, writer_image_build_id) REFERENCES image_builds(environment_id,id),
    ADD FOREIGN KEY (environment_id, writer_computer_id) REFERENCES computers(environment_id,id) ON DELETE RESTRICT;
