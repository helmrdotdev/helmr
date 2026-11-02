package db

// ServesTargetSQL is true when worker group g serves target.worker_group_id,
// an organization group or NULL for the deployment capacity of
// project.default_region_id. A pinned group is always in its Computer's
// region, and deployment capacity never includes an organization group.
const ServesTargetSQL = `(g.id=target.worker_group_id OR (target.worker_group_id IS NULL AND g.owner_org_id IS NULL AND g.region_id=project.default_region_id))`
