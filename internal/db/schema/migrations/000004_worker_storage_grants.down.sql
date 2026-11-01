DROP INDEX cas_objects_digest;

DROP TABLE allocation_storage_writes;

ALTER TABLE checkpoints
    DROP COLUMN data_key_wrapped,
    DROP COLUMN data_key_wrapping_key_id;

ALTER TABLE boot_seed_builds
    DROP COLUMN data_key_wrapped,
    DROP COLUMN data_key_wrapping_key_id;
