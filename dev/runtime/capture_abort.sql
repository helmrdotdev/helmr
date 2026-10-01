SELECT json_build_object(
  'run_id',r.id,'attempt_number',r.current_attempt_number,
  'checkpoint_id',c.id,'checkpoint_status',c.status,
  'acknowledged',c.abort_acknowledged_at IS NOT NULL,
  'source_instance_id',i.id,'source_reclaimed',i.reclaimed_at IS NOT NULL,
  'source_state',i.observed_state,'writer_generation',i.writer_generation,
  'captured_writer_generation',c.writer_generation,
  'captured_run_ids',(SELECT json_agg(member.run_id ORDER BY member.run_id)
    FROM computer_checkpoint_runs member WHERE member.checkpoint_id=c.id),
  'other_instances',(SELECT count(*) FROM computer_instances x WHERE x.computer_id=r.computer_id AND x.id<>i.id),
  'lease_on_source',EXISTS(SELECT 1 FROM run_leases l WHERE l.run_id=r.id
    AND l.attempt_number=r.current_attempt_number AND l.computer_instance_id=i.id)
)::text
FROM runs r
JOIN computer_checkpoint_runs m ON m.run_id=r.id AND m.attempt_number=r.current_attempt_number
JOIN computer_checkpoints c ON c.id=m.checkpoint_id AND c.status='aborted'
JOIN computer_instances i ON i.id=c.source_computer_instance_id
WHERE r.id=:'run_id'::uuid
ORDER BY c.created_at DESC LIMIT 1;
