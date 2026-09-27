SELECT json_build_object(
  'entrypoint_kind', r.entrypoint_kind, 'session_id', r.session_id,
  'run_id', r.id, 'run_status', r.status, 'run_failure', r.failure, 'attempt_number', r.current_attempt_number,
  'wait_id', w.id, 'condition', w.condition_status, 'suspension', w.suspension_status,
  'checkpoint_id', c.id, 'checkpoint_status', c.status,
  'prior_runtime_id', old.id, 'prior_runtime_state', old.observed_state,
  'prior_runtime_reclaimed', old.reclaimed_at IS NOT NULL,
  'restored_runtime_ids', COALESCE((SELECT json_agg(ri.id ORDER BY ri.id)
    FROM runtime_instances ri WHERE ri.restore_checkpoint_id = c.id
      AND ri.workspace_id = r.workspace_id AND ri.id <> old.id
      AND ri.ready_at IS NOT NULL), '[]'::json)
)::text
FROM runs r
LEFT JOIN run_waits w ON w.run_id = r.id AND w.kind = 'token'
LEFT JOIN run_checkpoints c ON c.id = w.suspend_checkpoint_id
LEFT JOIN run_leases l ON l.id = w.prior_run_lease_id
LEFT JOIN runtime_instances old ON old.id = l.runtime_instance_id
WHERE r.id = :'run_id'::uuid
ORDER BY w.created_at DESC LIMIT 1;
