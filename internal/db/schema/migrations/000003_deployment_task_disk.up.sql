ALTER TABLE deployment_tasks
    ADD COLUMN IF NOT EXISTS requested_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (requested_disk_mib >= 0 AND requested_disk_mib <= 2147483647);

ALTER TABLE run_runtime_requirements
    ADD CONSTRAINT run_runtime_requirements_requested_disk_mib_max_check CHECK (requested_disk_mib <= 2147483647);
