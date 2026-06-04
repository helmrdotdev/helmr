ALTER TABLE deployment_tasks
    ADD COLUMN requested_disk_mib BIGINT NOT NULL DEFAULT 0 CHECK (requested_disk_mib >= 0);
