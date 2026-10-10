SELECT json_build_object(
 'session_id',s.id,'session_status',s.status,
 'checkpoint_id',c.id,'checkpoint_status',c.status,
 'acknowledged',c.controls_reconciled_at IS NOT NULL,
 'source_abort',c.abort_identity IS NOT NULL AND c.target_lease_epoch IS NULL,
 'source_instance_id',source.computer_instance_id,'source_fenced',source.fenced_at IS NOT NULL,
 'source_state',source.status,'source_lease_epoch',c.source_lease_epoch,
 'captured_session_ids',(SELECT json_agg(member.session_id ORDER BY member.session_id)
   FROM computer_checkpoint_members member WHERE (member.environment_id,member.checkpoint_id)=(c.environment_id,c.id)),
 'later_leases',(SELECT count(*) FROM computer_leases newer WHERE (newer.environment_id,newer.computer_id)=(s.environment_id,s.computer_id) AND newer.epoch>c.source_lease_epoch),
 'lease_on_source',COALESCE(process.computer_lease_epoch=c.source_lease_epoch AND process.status='ready' AND process.fenced_at IS NULL
   AND source.expires_at>now() AND host.current_epoch=source.worker_epoch AND host.status IN ('active','draining'),false)
)::text
FROM sessions s
LEFT JOIN LATERAL (
 SELECT checkpoint.*,member.process_epoch
 FROM computer_checkpoint_members member
 JOIN computer_checkpoints checkpoint ON (checkpoint.environment_id,checkpoint.id)=(member.environment_id,member.checkpoint_id)
 WHERE member.environment_id=s.environment_id AND member.session_id=s.id AND checkpoint.computer_id=s.computer_id
 ORDER BY checkpoint.created_at DESC,checkpoint.id DESC LIMIT 1
) c ON true
LEFT JOIN computer_leases source ON (source.environment_id,source.computer_id,source.epoch)=(s.environment_id,s.computer_id,c.source_lease_epoch)
LEFT JOIN worker_hosts host ON host.id=source.worker_host_id
LEFT JOIN session_processes process ON (process.environment_id,process.session_id,process.epoch)=(s.environment_id,s.id,c.process_epoch)
WHERE s.id=:'session_id'::uuid;
