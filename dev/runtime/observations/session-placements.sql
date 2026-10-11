, requested AS (SELECT value::uuid AS id FROM input, jsonb_array_elements_text(args->'session_ids'))
SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'session_id', s.id, 'computer_id', s.computer_id, 'process_epoch', p.epoch,
 'worker_resource_id', h.resource_id, 'worker_host_id', h.id,
 'worker_epoch', l.worker_epoch, 'computer_lease_epoch', l.epoch,
 'computer_instance_id', l.computer_instance_id,
 'network_owner_id', l.computer_instance_id, 'network_generation', 1,
 'netns_name', l.computer_instance_id, 'tap_name', 'tap0'
) ORDER BY s.id), '[]'::jsonb)
FROM requested JOIN sessions s USING(id)
JOIN session_processes p ON p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL
JOIN computer_leases l ON l.environment_id=p.environment_id AND l.computer_id=p.computer_id AND l.epoch=p.computer_lease_epoch
JOIN worker_hosts h ON h.id=l.worker_host_id AND h.current_epoch=l.worker_epoch
WHERE p.status='ready' AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>now() AND h.status IN ('active','draining');
