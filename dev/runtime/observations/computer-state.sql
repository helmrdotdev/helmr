SELECT COALESCE((SELECT jsonb_build_object(
 'id', c.id, 'deleted_at', c.deleted_at, 'preparation_failed_at', c.preparation_failed_at,
 'integrity_fault_at', c.integrity_fault_at,
 'active_leases', (SELECT count(*) FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL),
 'open_sessions', (SELECT count(*) FROM sessions s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('open','closing'))
) FROM computers c,input WHERE c.id=(args->>'computer_id')::uuid), 'null'::jsonb);
