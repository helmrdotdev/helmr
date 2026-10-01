SELECT COALESCE((SELECT jsonb_build_object(
 'id', c.id, 'status', c.status, 'desired_state', c.desired_state,
 'latest_run_id', (SELECT id FROM runs WHERE computer_id=c.id ORDER BY created_at DESC,id DESC LIMIT 1),
 'unreclaimed_instances', (SELECT count(*) FROM computer_instances i WHERE i.computer_id=c.id AND i.reclaimed_at IS NULL),
 'active_leases', (SELECT count(*) FROM run_leases l WHERE l.computer_id=c.id AND l.status IN ('assigned','starting','running','checkpointing','finalizing'))
) FROM computers c,input WHERE c.id=(args->>'computer_id')::uuid), 'null'::jsonb);
