, target AS (SELECT r.* FROM runs r,input WHERE r.id=(args->>'run_id')::uuid),
leases AS (
 SELECT l.id, l.run_id, l.computer_id, l.lease_sequence, l.attempt_number,
 l.status, l.worker_host_id, l.worker_epoch, l.computer_instance_id,
 l.created_at, l.claimed_at, l.started_at, l.checkpointed_at, l.terminal_at,
 l.terminal_reason_code, i.allocated_at, i.ready_at, i.reclaimed_at,
 i.observed_state, i.source_checkpoint_id, i.source_disk_version_id,
 i.mount_state, i.mounted_at, i.unmounted_at,
 a.created_at AS attempt_created_at,
 EXISTS (SELECT 1 FROM run_waits w WHERE w.run_id=l.run_id AND w.current_run_lease_id=l.id
   AND w.prior_run_lease_id IS NULL AND w.suspend_checkpoint_id IS NULL
   AND w.suspension_status IN ('hot','released')) AS has_live_wait,
 EXISTS (SELECT 1 FROM computer_checkpoint_runs m JOIN computer_checkpoints cp ON cp.id=m.checkpoint_id
   WHERE m.checkpoint_id=i.source_checkpoint_id AND m.run_id=l.run_id
     AND m.attempt_number=l.attempt_number AND m.computer_id=l.computer_id
     AND cp.ready_at IS NOT NULL) AS has_checkpoint_resume
 FROM target r JOIN run_leases l ON l.run_id=r.id
 JOIN computer_instances i ON i.id=l.computer_instance_id AND i.computer_id=l.computer_id
 JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number AND a.computer_id=l.computer_id
), checkpoints AS (
 SELECT c.id, c.computer_id, c.status, c.created_at, c.ready_at, c.invalidated_at,
 c.expires_at, c.invalidation_reason_code, c.source_computer_instance_id,
 c.base_computer_disk_version_id, c.private_computer_disk_version_id,
 m.attempt_number, m.run_wait_id, m.source_run_lease_id,
 source.worker_host_id AS source_worker_host_id,source.worker_epoch AS source_worker_epoch,
 host.resource_id AS source_worker_resource_id,source.reclaimed_at AS source_reclaimed_at,
 (SELECT jsonb_agg(jsonb_build_object('run_id',member.run_id,'attempt_number',member.attempt_number,
 'source_run_lease_id',member.source_run_lease_id,'run_wait_id',member.run_wait_id) ORDER BY member.run_id)
 FROM computer_checkpoint_runs member WHERE member.checkpoint_id=c.id AND member.computer_id=c.computer_id) AS members,
 (SELECT COALESCE(jsonb_agg(jsonb_build_object('role', descriptor.role, 'digest', o.digest,
 'size_bytes', o.size_bytes, 'media_type', o.media_type) ORDER BY descriptor.role),'[]'::jsonb)
 FROM (VALUES ('vm_config',c.vm_config_artifact_id),('vm_state',c.vm_state_artifact_id),('memory',c.memory_artifact_id),('scratch_disk',c.scratch_disk_artifact_id)) descriptor(role,id) JOIN artifacts o ON o.id=descriptor.id) AS artifacts
 FROM target r JOIN computer_checkpoint_runs m ON m.run_id=r.id AND m.computer_id=r.computer_id
 JOIN computer_checkpoints c ON c.id=m.checkpoint_id AND c.computer_id=m.computer_id
 JOIN computer_instances source ON source.id=c.source_computer_instance_id
 JOIN worker_hosts host ON host.id=source.worker_host_id
)
SELECT jsonb_build_object(
 'run', (SELECT jsonb_build_object('id',r.id,'status',r.status,'parent_run_id',r.parent_run_id,'computer_id',r.computer_id,
 'attempt_number',r.current_attempt_number,'current_run_lease_id',r.current_run_lease_id,
 'created_at',r.created_at,'started_at',r.started_at,'terminal_at',r.terminal_at) FROM target r),
 'leases', COALESCE((SELECT jsonb_agg(to_jsonb(l) || jsonb_build_object(
 'was_ready_before_attempt', l.ready_at IS NOT NULL AND l.ready_at<=l.attempt_created_at,
 'was_allocated_after_attempt', l.allocated_at>=l.attempt_created_at,
 'inferred_path', CASE WHEN l.has_checkpoint_resume THEN 'checkpoint_resume'
 WHEN l.has_live_wait THEN 'resident_live_wait'
 WHEN l.ready_at IS NOT NULL AND l.ready_at<=l.attempt_created_at THEN 'prepared_instance_claim'
 ELSE 'cold_instance_allocation' END) ORDER BY l.lease_sequence) FROM leases l),'[]'::jsonb),
 'waits', COALESCE((SELECT jsonb_agg(jsonb_build_object('id',w.id,'kind',w.kind,'condition_status',w.condition_status,'suspension_status',w.suspension_status,'token_id',w.token_id,'child_run_id',w.child_run_id,'current_run_lease_id',w.current_run_lease_id,'prior_run_lease_id',w.prior_run_lease_id,'suspend_checkpoint_id',w.suspend_checkpoint_id) ORDER BY w.created_at,w.id) FROM run_waits w JOIN target r ON r.id=w.run_id AND r.current_attempt_number=w.attempt_number),'[]'::jsonb),
 'checkpoints', COALESCE((SELECT jsonb_agg(to_jsonb(c) ORDER BY c.created_at,c.id) FROM checkpoints c),'[]'::jsonb));
