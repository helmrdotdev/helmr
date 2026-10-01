, target AS (SELECT c.* FROM computers c,input WHERE c.id=(args->>'computer_id')::uuid)
SELECT jsonb_build_object(
 'computer', (SELECT jsonb_build_object('id',id,'status',status,'writer_generation',writer_generation) FROM target),
 'instances', COALESCE((SELECT jsonb_agg(jsonb_build_object(
 'id',i.id,'worker_host_id',i.worker_host_id,'worker_epoch',i.worker_epoch,
 'observed_state',i.observed_state,'reclaimed_at',i.reclaimed_at,
 'source_checkpoint_id',i.source_checkpoint_id,'source_disk_version_id',i.source_disk_version_id,
 'ready_at',i.ready_at,'mount_state',i.mount_state) ORDER BY i.allocated_at,i.id)
 FROM computer_instances i JOIN target c ON c.id=i.computer_id),'[]'::jsonb),
 'checkpoints', COALESCE((SELECT jsonb_agg(jsonb_build_object(
 'id',p.id,'status',p.status,'source_computer_instance_id',p.source_computer_instance_id,
 'private_computer_disk_version_id',p.private_computer_disk_version_id,
 'ready_at',p.ready_at,'resume_computer_instance_id',p.resume_computer_instance_id,
 'resume_committed_at',p.resume_committed_at) ORDER BY p.created_at,p.id)
 FROM computer_checkpoints p JOIN target c ON c.id=p.computer_id),'[]'::jsonb),
 'commands', COALESCE((SELECT jsonb_agg(jsonb_build_object(
 'id',cmd.id,'status',cmd.status,'computer_instance_id',cmd.computer_instance_id,
 'exit_code',cmd.exit_code) ORDER BY cmd.created_at,cmd.id)
 FROM computer_commands cmd JOIN target c ON c.id=cmd.computer_id),'[]'::jsonb));
