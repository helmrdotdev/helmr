SELECT COALESCE((SELECT jsonb_build_object(
 'id', s.id, 'computer_id', s.computer_id, 'status', s.status,
 'active_leases', (SELECT count(*) FROM computer_leases l WHERE l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.fenced_at IS NULL),
 'unfenced_processes', (SELECT count(*) FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL),
 'outstanding_turns', (SELECT count(*) FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.terminal_at IS NULL)
) FROM sessions s,input WHERE s.id=(args->>'session_id')::uuid), 'null'::jsonb);
