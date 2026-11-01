DROP TABLE allocation_storage_writes;

ALTER TABLE checkpoint_objects
    DROP CONSTRAINT checkpoint_objects_org_id_digest_size_bytes_availability_r_fkey,
    DROP CONSTRAINT checkpoint_objects_org_id_environment_id_fkey,
    DROP COLUMN org_id;

ALTER TABLE boot_seed_objects
    DROP CONSTRAINT boot_seed_objects_org_id_digest_size_bytes_availability_re_fkey,
    DROP CONSTRAINT boot_seed_objects_org_id_environment_id_fkey,
    DROP COLUMN org_id;

ALTER TABLE disk_objects DROP CONSTRAINT disk_objects_org_id_digest_size_bytes_availability_require_fkey;
ALTER TABLE cas_objects DROP CONSTRAINT cas_objects_org_id_digest_size_bytes_availability_required_fkey;

ALTER TABLE cas_upload_reclaims
    DROP CONSTRAINT cas_upload_reclaims_org_id_digest_fkey,
    DROP CONSTRAINT cas_upload_reclaims_pkey,
    DROP COLUMN org_id,
    ADD PRIMARY KEY (digest, upload_id);

ALTER TABLE cas_blobs
    DROP CONSTRAINT cas_blobs_org_id_digest_size_bytes_not_retired_key,
    DROP CONSTRAINT cas_blobs_pkey,
    DROP COLUMN org_id,
    ADD PRIMARY KEY (digest),
    ADD UNIQUE (digest, size_bytes, not_retired);

ALTER TABLE cas_upload_reclaims ADD FOREIGN KEY (digest) REFERENCES cas_blobs(digest);
ALTER TABLE cas_objects ADD FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired);
ALTER TABLE disk_objects ADD FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired) ON DELETE RESTRICT;
ALTER TABLE boot_seed_objects ADD FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired);
ALTER TABLE checkpoint_objects ADD FOREIGN KEY (digest, size_bytes, availability_required) REFERENCES cas_blobs(digest, size_bytes, not_retired);

ALTER TABLE checkpoints
    DROP COLUMN data_key_wrapped,
    DROP COLUMN data_key_wrapping_key_id;

ALTER TABLE boot_seed_builds
    DROP COLUMN data_key_wrapped,
    DROP COLUMN data_key_wrapping_key_id;
