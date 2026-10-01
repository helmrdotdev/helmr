, requested AS (SELECT value AS resource_id FROM input, jsonb_array_elements_text(args->'resource_ids')),
current_hosts AS (
 SELECT DISTINCT ON (h.worker_group_id,h.resource_id) h.*
 FROM worker_hosts h JOIN requested USING(resource_id)
 ORDER BY h.worker_group_id,h.resource_id,
 (h.status IN ('registering','active','draining')) DESC,h.created_at DESC,h.id DESC
)
SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'id', h.id, 'worker_group_id', h.worker_group_id, 'resource_id', h.resource_id, 'status', h.status,
 'current_epoch', h.current_epoch, 'vm_platform_id', h.vm_platform_id,
 'observed_epoch', extract(epoch FROM h.observed_at),
 'termination_ready_at', h.termination_ready_at, 'lost_at', h.lost_at,
 'active_leases', (SELECT count(*) FROM run_leases l WHERE l.worker_host_id=h.id AND l.status IN ('assigned','starting','running','checkpointing','finalizing')),
 'unreclaimed_instances', (SELECT count(*) FROM computer_instances i WHERE i.worker_host_id=h.id AND i.reclaimed_at IS NULL)
) ORDER BY h.id), '[]'::jsonb)
FROM current_hosts h;
