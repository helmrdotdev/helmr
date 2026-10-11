SELECT json_build_object(
 'session_id',s.id,'session_status',s.status,
 'checkpoint_id',c.id,'checkpoint_status',c.status,
 'source_lease_epoch',c.source_lease_epoch,'target_lease_epoch',c.target_lease_epoch,
 'prior_runtime_id',old.computer_instance_id,'source_fenced',old.fenced_at IS NOT NULL,
 'source_state',old.status,'controls_reconciled',c.controls_reconciled_at IS NOT NULL,
 'target_runtime_id',restored.computer_instance_id,
 'target_current',COALESCE(restored.status='active' AND restored.fenced_at IS NULL
    AND restored.expires_at>now() AND host.current_epoch=restored.worker_epoch
    AND host.status IN ('active','draining') AND process.status='ready'
    AND process.fenced_at IS NULL AND process.computer_lease_epoch=c.target_lease_epoch,false)
)::text
FROM sessions s
LEFT JOIN LATERAL (
 SELECT checkpoint.*,member.process_epoch
 FROM computer_checkpoint_members member
 JOIN computer_checkpoints checkpoint ON (checkpoint.environment_id,checkpoint.id)=(member.environment_id,member.checkpoint_id)
 WHERE member.environment_id=s.environment_id AND member.session_id=s.id AND checkpoint.computer_id=s.computer_id
 ORDER BY checkpoint.created_at DESC,checkpoint.id DESC LIMIT 1
) c ON true
LEFT JOIN computer_leases old ON (old.environment_id,old.computer_id,old.epoch)=(s.environment_id,s.computer_id,c.source_lease_epoch)
LEFT JOIN computer_leases restored ON (restored.environment_id,restored.computer_id,restored.epoch)=(s.environment_id,s.computer_id,c.target_lease_epoch)
LEFT JOIN worker_hosts host ON host.id=restored.worker_host_id
LEFT JOIN session_processes process ON (process.environment_id,process.session_id,process.epoch)=(s.environment_id,s.id,c.process_epoch)
WHERE s.id=:'session_id'::uuid;
