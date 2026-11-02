package db

// ServesTargetSQL is true when worker group g serves target.worker_group_id,
// an organization group or NULL for the deployment capacity of
// project.default_region_id. A pinned group is always in its Computer's
// region, and deployment capacity never includes an organization group.
const ServesTargetSQL = `(g.id=target.worker_group_id OR (target.worker_group_id IS NULL AND g.owner_org_id IS NULL AND g.region_id=project.default_region_id))`

// PreparationTargetSQL is a subquery for a lateral join over the placement of
// the image build or boot seed aliased prep. The templates sharing its spec
// name one placement, because the spec includes it. It yields one row, whose
// worker_group_id is the target, only when the placement is bound: assigned,
// or the default placement without an override, which is deployment capacity.
const PreparationTargetSQL = `(SELECT assigned.worker_group_id FROM (SELECT d.placement name FROM template_definitions d
 WHERE d.environment_id=prep.environment_id AND d.image_spec_id=prep.image_spec_id ORDER BY d.deployment_id,d.template_id LIMIT 1) used
 LEFT JOIN environment_placements assigned ON assigned.environment_id=prep.environment_id AND assigned.name=used.name
 WHERE assigned.name IS NOT NULL OR used.name='default')`
