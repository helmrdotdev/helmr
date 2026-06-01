ALTER TABLE task_schedule_instances
    DROP COLUMN IF EXISTS materialize_error_message,
    DROP COLUMN IF EXISTS materialize_attempt_count,
    DROP COLUMN IF EXISTS materialize_lease_expires_at,
    DROP COLUMN IF EXISTS materialize_lease_id;
