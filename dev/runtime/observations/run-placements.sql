, requested AS (SELECT value::uuid AS id FROM input, jsonb_array_elements_text(args->'run_ids'))
SELECT COALESCE(jsonb_agg(jsonb_build_object(
  'run_id', r.id, 'worker_resource_id', h.resource_id, 'worker_host_id', h.id,
  'worker_epoch', l.worker_epoch, 'computer_instance_id', l.computer_instance_id,
  'network_owner_id', l.computer_instance_id, 'network_generation', 1,
  'netns_name', l.computer_instance_id, 'tap_name', 'tap0'
) ORDER BY r.id), '[]'::jsonb)
FROM requested JOIN runs r USING(id)
JOIN run_leases l ON l.id=r.current_run_lease_id AND l.run_id=r.id
JOIN worker_hosts h ON h.id=l.worker_host_id AND h.current_epoch=l.worker_epoch
WHERE l.status='running' AND h.status IN ('active','draining');
