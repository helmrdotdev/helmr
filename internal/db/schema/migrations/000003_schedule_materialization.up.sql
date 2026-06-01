ALTER TABLE task_schedule_instances
    ADD COLUMN materialize_lease_id UUID,
    ADD COLUMN materialize_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN materialize_attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (materialize_attempt_count >= 0),
    ADD COLUMN materialize_error_message TEXT NOT NULL DEFAULT '';
