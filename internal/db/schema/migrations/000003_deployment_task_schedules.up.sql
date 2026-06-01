ALTER TABLE deployment_tasks
    ADD COLUMN IF NOT EXISTS schedule_declarations JSONB NOT NULL DEFAULT '[]'::jsonb;
