SELECT COALESCE((SELECT jsonb_build_object(
 'id', r.id, 'computer_id', r.computer_id, 'status', r.status,
 'current_run_lease_id', r.current_run_lease_id,
 'active_leases', (SELECT count(*) FROM run_leases l WHERE l.run_id=r.id AND l.status IN ('assigned','starting','running','checkpointing','finalizing')),
 'unreclaimed_instances', (SELECT count(DISTINCT i.id) FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.run_id=r.id AND i.reclaimed_at IS NULL)
) FROM runs r,input WHERE r.id=(args->>'run_id')::uuid), 'null'::jsonb);
