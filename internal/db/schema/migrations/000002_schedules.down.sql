DROP INDEX IF EXISTS runs_schedule_instance_id_idx;
DROP INDEX IF EXISTS runs_schedule_id_idx;
DROP INDEX IF EXISTS runs_schedule_idx;

ALTER TABLE runs
    DROP CONSTRAINT IF EXISTS runs_schedule_instance_id_fkey,
    DROP CONSTRAINT IF EXISTS runs_schedule_id_fkey,
    DROP COLUMN IF EXISTS scheduled_at,
    DROP COLUMN IF EXISTS schedule_instance_id,
    DROP COLUMN IF EXISTS schedule_id;

DROP TABLE IF EXISTS task_schedule_fires;
DROP TABLE IF EXISTS task_schedule_instances;
DROP TABLE IF EXISTS task_schedules;

DROP TYPE IF EXISTS task_schedule_generator_type;
DROP TYPE IF EXISTS task_schedule_type;
DROP TYPE IF EXISTS task_schedule_fire_status;
