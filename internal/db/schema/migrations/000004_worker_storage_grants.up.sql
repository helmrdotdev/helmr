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

-- Every CAS object lives in its organization's keyspace: the same digest in two
-- organizations is two independent objects with independent lifetimes. Existing
-- lifetimes cannot be attributed to one organization, so this requires an empty
-- content store.
ALTER TABLE cas_upload_reclaims DROP CONSTRAINT cas_upload_reclaims_digest_fkey;
ALTER TABLE cas_objects DROP CONSTRAINT cas_objects_digest_size_bytes_availability_required_fkey;
ALTER TABLE disk_objects DROP CONSTRAINT disk_objects_digest_size_bytes_availability_required_fkey;
ALTER TABLE boot_seed_objects DROP CONSTRAINT boot_seed_objects_digest_size_bytes_availability_required_fkey;
ALTER TABLE checkpoint_objects DROP CONSTRAINT checkpoint_objects_digest_size_bytes_availability_required_fkey;

ALTER TABLE cas_blobs
    DROP CONSTRAINT cas_blobs_pkey,
    DROP CONSTRAINT cas_blobs_digest_size_bytes_not_retired_key,
    ADD COLUMN org_id uuid NOT NULL REFERENCES organizations(id),
    ADD PRIMARY KEY (org_id, digest),
    ADD UNIQUE (org_id, digest, size_bytes, not_retired);

ALTER TABLE cas_upload_reclaims
    DROP CONSTRAINT cas_upload_reclaims_pkey,
    ADD COLUMN org_id uuid NOT NULL,
    ADD PRIMARY KEY (org_id, digest, upload_id),
    ADD FOREIGN KEY (org_id, digest) REFERENCES cas_blobs(org_id, digest);

ALTER TABLE cas_objects
    ADD FOREIGN KEY (org_id, digest, size_bytes, availability_required) REFERENCES cas_blobs(org_id, digest, size_bytes, not_retired);

ALTER TABLE disk_objects
    ADD FOREIGN KEY (org_id, digest, size_bytes, availability_required) REFERENCES cas_blobs(org_id, digest, size_bytes, not_retired) ON DELETE RESTRICT;

ALTER TABLE boot_seed_objects
    ADD COLUMN org_id uuid NOT NULL,
    ADD FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id),
    ADD FOREIGN KEY (org_id, digest, size_bytes, availability_required) REFERENCES cas_blobs(org_id, digest, size_bytes, not_retired);

ALTER TABLE checkpoint_objects
    ADD COLUMN org_id uuid NOT NULL,
    ADD FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id),
    ADD FOREIGN KEY (org_id, digest, size_bytes, availability_required) REFERENCES cas_blobs(org_id, digest, size_bytes, not_retired);

-- Create-only storage writes granted to one Host allocation of an organization.
-- A receipt binds a multipart upload to the allocation that created it, lets
-- the same allocation read back its own upload before certification, and keeps
-- the upload's lifetime registered so the reclaimer can remove an abandoned
-- upload once receipts of ended allocations are removed. Finding an object
-- already stored records no receipt.
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
    -- race an allocation that is uploading the digest.
    FOREIGN KEY (org_id, digest, size_bytes, availability_required) REFERENCES cas_blobs(org_id, digest, size_bytes, not_retired)
);

CREATE INDEX allocation_storage_writes_digest ON allocation_storage_writes(org_id, digest);
