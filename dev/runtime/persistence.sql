SELECT json_build_object(
  'entrypoint_kind', r.entrypoint_kind, 'session_id', r.session_id,
  'run_id', r.id, 'run_status', r.status, 'run_failure', r.failure, 'attempt_number', r.current_attempt_number,
  'wait_id', w.id, 'condition', w.condition_status, 'suspension', w.suspension_status,
  'checkpoint_id', c.id, 'checkpoint_status', c.status,
  'prior_runtime_id', old.id, 'prior_runtime_state', old.observed_state,
  'prior_runtime_reclaimed', old.reclaimed_at IS NOT NULL,
  'restored_runtime_ids', COALESCE((SELECT json_agg(ri.id ORDER BY ri.id)
    FROM computer_instances ri WHERE ri.source_checkpoint_id = c.id
      AND ri.computer_id = r.computer_id AND ri.id <> old.id
      AND ri.ready_at IS NOT NULL), '[]'::json)
)::text
FROM runs r
LEFT JOIN run_waits w ON w.run_id = r.id AND w.kind = 'token' AND w.attempt_number = r.current_attempt_number
LEFT JOIN LATERAL (
  SELECT member.checkpoint_id, member.source_computer_instance_id
  FROM computer_checkpoint_runs member
  JOIN computer_checkpoints checkpoint ON checkpoint.id = member.checkpoint_id
  WHERE member.run_id = r.id AND member.attempt_number = r.current_attempt_number
    AND member.run_wait_id = w.id AND member.computer_id = r.computer_id
    AND member.environment_id = r.environment_id
    AND (w.suspend_checkpoint_id IS NULL OR member.checkpoint_id = w.suspend_checkpoint_id)
  ORDER BY checkpoint.created_at DESC, checkpoint.id DESC LIMIT 1
) captured ON true
LEFT JOIN computer_checkpoints c ON c.id = captured.checkpoint_id
LEFT JOIN computer_instances old ON old.id = captured.source_computer_instance_id
WHERE r.id = :'run_id'::uuid
ORDER BY w.created_at DESC LIMIT 1;
