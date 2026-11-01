-- Encrypts one boot seed's memory, state and scratch objects; wrapped by the
-- deployment key provider and delivered only against live authority.
ALTER TABLE boot_seed_builds
    ADD COLUMN data_key_wrapping_key_id text CHECK (octet_length(data_key_wrapping_key_id) BETWEEN 1 AND 2048),
    ADD COLUMN data_key_wrapped bytea CHECK (octet_length(data_key_wrapped) BETWEEN 1 AND 6144),
    ADD CONSTRAINT boot_seed_builds_data_key_check CHECK ((data_key_wrapping_key_id IS NULL) = (data_key_wrapped IS NULL));

-- Encrypts one checkpoint's objects; delivered only to its capturing and
-- restoring allocations.
ALTER TABLE checkpoints
    ADD COLUMN data_key_wrapping_key_id text CHECK (octet_length(data_key_wrapping_key_id) BETWEEN 1 AND 2048),
    ADD COLUMN data_key_wrapped bytea CHECK (octet_length(data_key_wrapped) BETWEEN 1 AND 6144),
    ADD CONSTRAINT checkpoints_data_key_check CHECK ((data_key_wrapping_key_id IS NULL) = (data_key_wrapped IS NULL));

-- Create-only storage writes granted to one Host allocation of an organization.
-- A receipt lets the same allocation read back its own upload before
-- certification, and lets certification admit an object the organization
-- uploaded; a digest stored by another organization is never receipted.
-- Receipts of ended allocations are removed.
CREATE TABLE allocation_storage_writes (
    worker_host_id uuid NOT NULL REFERENCES worker_hosts(id) ON DELETE CASCADE,
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    allocation_id uuid NOT NULL,
    org_id uuid NOT NULL REFERENCES organizations(id),
    digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    media_type text NOT NULL CHECK (btrim(media_type) <> ''),
    upload_id text CHECK (octet_length(upload_id) BETWEEN 1 AND 1024),
    availability_required boolean GENERATED ALWAYS AS (true) STORED,
    PRIMARY KEY (worker_host_id, allocation_id, digest),
    -- A receipted upload keeps its lifetime available, so retirement cannot
    -- race an allocation that is uploading or adopting the digest.
    FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired)
);

-- Ownership checks look up a digest across organizations and receipts.
CREATE INDEX allocation_storage_writes_digest ON allocation_storage_writes(digest);
CREATE INDEX cas_objects_digest ON cas_objects(digest);
