-- A spec can be referenced by multiple immutable Deployment definitions. These
-- references do not identify which Deployment requested the preparation.
, target AS (
 SELECT b.environment_id,b.id,b.image_spec_id,b.predecessor_build_id,b.status,b.error_code,
        b.worker_host_id,b.worker_epoch,b.allocation_id,b.executor_epoch,
        b.deadline_at,b.executor_expires_at,b.delivered_at,b.started_at,b.fenced_at,
        s.spec_digest,h.resource_id,h.status AS host_status
 FROM input JOIN image_builds b
   ON b.environment_id=(args->>'environment_id')::uuid AND b.id=(args->>'image_build_id')::uuid
 JOIN image_specs s ON s.environment_id=b.environment_id AND s.id=b.image_spec_id
 LEFT JOIN worker_hosts h ON h.id=b.worker_host_id
), definitions AS (
 SELECT d.deployment_id,d.template_id,t.key AS template_key,p.bundle_digest
 FROM target b JOIN template_definitions d
   ON d.environment_id=b.environment_id AND d.image_spec_id=b.image_spec_id
 JOIN templates t ON t.environment_id=d.environment_id AND t.id=d.template_id
 JOIN deployments p ON p.environment_id=d.environment_id AND p.id=d.deployment_id
 ORDER BY d.deployment_id,d.template_id LIMIT 257
)
SELECT jsonb_build_object(
 'observed_at',clock_timestamp(),
 'preparation',(SELECT jsonb_build_object(
   'environment_id',b.environment_id,'image_build_id',b.id,
   'image_spec_id',b.image_spec_id,'spec_digest',b.spec_digest,
   'predecessor_build_id',b.predecessor_build_id,'status',b.status,'error_code',b.error_code,
   'worker_host_id',b.worker_host_id,'worker_epoch',b.worker_epoch,
   'allocation_id',b.allocation_id,'executor_epoch',b.executor_epoch,
   'deadline_at',b.deadline_at,'executor_expires_at',b.executor_expires_at,
   'delivered_at',b.delivered_at,'started_at',b.started_at,'fenced_at',b.fenced_at,
   'recorded_host_resource_id',b.resource_id,'recorded_host_status',b.host_status
 ) FROM target b),
 'definitions_have_more',(SELECT count(*)>256 FROM definitions),
 'referencing_definitions',COALESCE((SELECT jsonb_agg(jsonb_build_object(
   'deployment_id',d.deployment_id,'template_id',d.template_id,
   'template_key',d.template_key,'bundle_digest',d.bundle_digest
 ) ORDER BY d.deployment_id,d.template_id)
 FROM (SELECT * FROM definitions ORDER BY deployment_id,template_id LIMIT 256) d),'[]'::jsonb)
);
