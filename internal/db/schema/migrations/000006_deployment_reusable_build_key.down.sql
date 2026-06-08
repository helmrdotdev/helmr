DROP INDEX IF EXISTS deployments_reusable_build_key_idx;
CREATE UNIQUE INDEX deployments_reusable_build_key_idx
    ON deployments(org_id, project_id, environment_id, content_hash)
    WHERE status IN ('queued', 'building', 'deployed');
