-- Refuse to discard an independently set pause reason on an existing database.
-- Recovery for this specific preflight rejection: keep writers stopped, verify
-- both old pause columns still exist, and reconcile their values deliberately.
-- The migration runner has marked version 2 dirty; after verifying no DDL was
-- applied, set schema_migrations to version=1, dirty=false and rerun migration.
-- Do not clear the dirty marker for other failures without inspecting the schema.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM worker_hosts WHERE run_paused_reason IS DISTINCT FROM vm_paused_reason) THEN
        RAISE EXCEPTION 'worker pause reasons differ; reconcile admission state before migration';
    END IF;
END;
$$;
ALTER TABLE worker_hosts RENAME COLUMN run_paused_reason TO admission_paused_reason;
ALTER TABLE worker_hosts DROP COLUMN vm_paused_reason;
DROP VIEW computer_secret_revocations;

-- Every inserted retry record is complete. Transaction ownership couples it to
-- the domain mutation; ordinary constraints validate its durable shape.
ALTER TABLE platform_retry_keys ADD CONSTRAINT platform_retry_keys_complete CHECK (
    num_nonnulls(deployment_id,secret_id,computer_id,command_id)=1
    AND (receipt IS NOT NULL OR receipt_pruned_at IS NOT NULL)
);
DROP TRIGGER platform_retry_completion ON platform_retry_keys;
DROP FUNCTION require_platform_retry_completion();
DO $$
DECLARE constraint_name text;
BEGIN
    FOR constraint_name IN SELECT conname FROM pg_constraint
        WHERE conrelid='platform_retry_keys'::regclass AND contype='f' AND condeferrable
    LOOP
        EXECUTE format('ALTER TABLE platform_retry_keys ALTER CONSTRAINT %I NOT DEFERRABLE', constraint_name);
    END LOOP;
END;
$$;
