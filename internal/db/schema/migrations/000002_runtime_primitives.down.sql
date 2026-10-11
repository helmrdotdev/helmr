ALTER TABLE worker_hosts RENAME COLUMN admission_paused_reason TO run_paused_reason;
ALTER TABLE worker_hosts ADD COLUMN vm_paused_reason text;
UPDATE worker_hosts SET vm_paused_reason=run_paused_reason;
ALTER TABLE platform_retry_keys DROP CONSTRAINT platform_retry_keys_complete;
DO $$
DECLARE constraint_name text;
BEGIN
    FOR constraint_name IN SELECT conname FROM pg_constraint
        WHERE conrelid='platform_retry_keys'::regclass AND contype='f' AND conname <> 'platform_retry_keys_environment_id_fkey'
    LOOP
        EXECUTE format('ALTER TABLE platform_retry_keys ALTER CONSTRAINT %I DEFERRABLE INITIALLY DEFERRED', constraint_name);
    END LOOP;
END;
$$;
CREATE VIEW computer_secret_revocations AS
    SELECT c.environment_id,c.id AS computer_id,x.secret_id
    FROM computers c JOIN computer_images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id)
    JOIN secret_exposures x ON (x.environment_id,x.preparation_id)=(i.environment_id,i.preparation_id)
    JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
    WHERE s.status='revoked'
    UNION
    SELECT p.environment_id,p.computer_id,x.secret_id FROM secret_exposures x
    JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(x.environment_id,x.session_id,x.process_epoch)
    JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id) WHERE s.status='revoked'
    UNION
    SELECT c.environment_id,c.computer_id,x.secret_id FROM secret_exposures x
    JOIN computer_commands c ON (c.environment_id,c.id)=(x.environment_id,x.command_id)
    JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id) WHERE s.status='revoked'
;
CREATE FUNCTION require_platform_retry_completion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM platform_retry_keys k WHERE k.environment_id=NEW.environment_id AND k.id=NEW.id
        AND (num_nonnulls(k.deployment_id,k.secret_id,k.computer_id,k.command_id)<>1 OR (k.receipt IS NULL AND k.receipt_pruned_at IS NULL))) THEN
        RAISE EXCEPTION 'platform retry target and receipt must commit with mutation' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER platform_retry_completion AFTER INSERT OR UPDATE ON platform_retry_keys
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION require_platform_retry_completion();
