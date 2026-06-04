ALTER TABLE run_runtime_requirements
    DROP CONSTRAINT IF EXISTS run_runtime_requirements_requested_disk_mib_max_check;

ALTER TABLE deployment_tasks
    DROP COLUMN IF EXISTS requested_disk_mib;
