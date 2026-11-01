-- A group without an owner is deployment capacity. An organization group's
-- owner is fixed at creation because its hosts have held that organization's
-- data; runtime privileges exclude updating it.
ALTER TABLE worker_groups
    ADD COLUMN owner_org_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
    DROP CONSTRAINT worker_groups_region_id_name_key,
    ADD UNIQUE NULLS NOT DISTINCT (owner_org_id, region_id, name),
    ADD UNIQUE (id, owner_org_id, region_id);

DROP INDEX worker_groups_one_active_per_region_idx;

CREATE UNIQUE INDEX worker_groups_one_active_deployment_per_region_idx ON worker_groups(region_id) WHERE owner_org_id IS NULL AND status IN ('active', 'paused');

ALTER TABLE projects ADD UNIQUE (org_id, id, default_region_id);

-- An environment maps each placement name its templates use to the deployment
-- capacity of its project's region (no group) or to a group its organization
-- owns in that region. Without a row, default maps to deployment capacity, so
-- a default row always names a group.
CREATE TABLE environment_placements (
    org_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    region_id text NOT NULL,
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    worker_group_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (environment_id, name),
    CHECK (name <> 'default' OR worker_group_id IS NOT NULL),
    FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments(org_id, project_id, id),
    FOREIGN KEY (org_id, project_id, region_id) REFERENCES projects(org_id, id, default_region_id),
    CONSTRAINT environment_placements_group_fkey
        FOREIGN KEY (worker_group_id, org_id, region_id) REFERENCES worker_groups(id, owner_org_id, region_id)
);

CREATE INDEX environment_placements_worker_group ON environment_placements(worker_group_id) WHERE worker_group_id IS NOT NULL;

-- The group this Computer runs on. A Computer never moves between groups.
ALTER TABLE computers ADD COLUMN worker_group_id uuid REFERENCES worker_groups(id);

-- The producing group is recorded with each executor attempt and is the
-- group of its host. A boot seed depends on its producing group's VM platform
-- and CPU shape.
ALTER TABLE image_builds ADD COLUMN worker_group_id uuid REFERENCES worker_groups(id);
UPDATE image_builds SET worker_group_id = worker_hosts.worker_group_id
  FROM worker_hosts
 WHERE worker_hosts.id = image_builds.worker_host_id;
ALTER TABLE image_builds
    ADD CONSTRAINT image_builds_producing_group_check CHECK (worker_host_id IS NULL OR worker_group_id IS NOT NULL),
    ADD CONSTRAINT image_builds_producing_group_fkey FOREIGN KEY (worker_host_id, worker_group_id) REFERENCES worker_hosts(id, worker_group_id);

ALTER TABLE boot_seed_builds ADD COLUMN worker_group_id uuid REFERENCES worker_groups(id);
UPDATE boot_seed_builds SET worker_group_id = worker_hosts.worker_group_id
  FROM worker_hosts
 WHERE worker_hosts.id = boot_seed_builds.worker_host_id;
ALTER TABLE boot_seed_builds
    ADD CONSTRAINT boot_seed_builds_producing_group_check CHECK (worker_host_id IS NULL OR worker_group_id IS NOT NULL),
    ADD CONSTRAINT boot_seed_builds_producing_group_fkey FOREIGN KEY (worker_host_id, worker_group_id) REFERENCES worker_hosts(id, worker_group_id);
