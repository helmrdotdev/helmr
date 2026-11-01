-- Organization groups hold their organization's data and have no deployment
-- form, so downgrading requires that none remain. The lock keeps one from
-- being created after the check.
LOCK TABLE worker_groups IN ACCESS EXCLUSIVE MODE;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM worker_groups WHERE owner_org_id IS NOT NULL) THEN
        RAISE EXCEPTION 'organization worker groups must be removed before downgrading; the schema remains at version 5';
    END IF;
END
$$;

ALTER TABLE boot_seed_builds DROP COLUMN worker_group_id;
ALTER TABLE image_builds DROP COLUMN worker_group_id;
ALTER TABLE computers
    DROP COLUMN worker_group_id,
    DROP COLUMN worker_group_region_id,
    DROP COLUMN worker_group_project_id,
    DROP COLUMN worker_group_org_id;

DROP TABLE environment_placements;

ALTER TABLE projects DROP CONSTRAINT projects_org_id_id_default_region_id_key;

DROP INDEX worker_groups_one_active_deployment_per_region_idx;
ALTER TABLE worker_groups DROP COLUMN owner_org_id;
ALTER TABLE worker_groups ADD UNIQUE (region_id, name);
CREATE UNIQUE INDEX worker_groups_one_active_per_region_idx ON worker_groups(region_id) WHERE status IN ('active', 'paused');
