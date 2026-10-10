-- name: ListAllocationPlanningDemand :many
-- Planning counts physical Computer/preparation demand once. Session processes
-- and Commands on a resident Computer consume its existing allocation.
WITH RECURSIVE held AS (
 SELECT environment_id,session_id,scope FROM session_holds WHERE released_at IS NULL
 UNION
 SELECT s.environment_id,s.id,'subtree'::text FROM sessions s JOIN held h ON h.environment_id=s.environment_id AND h.session_id=s.parent_session_id AND h.scope='subtree'
), demand AS (
 SELECT p.environment_id,p.id owner_id,'preparation'::text kind,d.resources,
 NULL::uuid required_worker_group_id,''::text required_vm_platform_id,0::integer required_vm_vcpu_count,
 ''::text required_cpu_config_digest,0::bigint required_cpu_millis,0::bigint required_memory_bytes,
 0::bigint required_scratch_bytes
 FROM computer_preparations p
 JOIN LATERAL (SELECT resources FROM computer_definitions d WHERE d.environment_id=p.environment_id AND d.preparation_spec_id=p.preparation_spec_id ORDER BY deployment_id,definition_key LIMIT 1) d ON true
 WHERE p.status='queued' AND p.worker_host_id IS NULL AND p.deadline_at>statement_timestamp()
 AND NOT EXISTS(SELECT 1 FROM computer_secret_bindings binding JOIN secrets secret ON secret.environment_id=binding.environment_id AND secret.id=binding.secret_id
 WHERE binding.environment_id=p.environment_id AND binding.preparation_spec_id=p.preparation_spec_id AND secret.status='revoked')
 UNION ALL
 SELECT c.environment_id,c.id,'computer',c.resources,h.worker_group_id,COALESCE(cp.vm_platform_id,''),
 COALESCE(source.vm_vcpu_count,0),COALESCE(source.cpu_config_digest,''),COALESCE(source.reserved_cpu_millis,0),
 COALESCE(source.reserved_memory_bytes,0),COALESCE(source.reserved_scratch_bytes,0)
 FROM computers c
 LEFT JOIN computer_checkpoints cp ON cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status IN ('ready','restoring')
 LEFT JOIN computer_leases source ON source.environment_id=cp.environment_id AND source.computer_id=cp.computer_id AND source.epoch=cp.source_lease_epoch
 LEFT JOIN worker_hosts h ON h.id=source.worker_host_id
 WHERE NOT EXISTS(SELECT 1 FROM computer_leases live WHERE live.environment_id=c.environment_id AND live.computer_id=c.id AND live.fenced_at IS NULL) AND (
 c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id)
 AND ((cp.id IS NOT NULL) OR (c.initial_root_id IS NOT NULL AND c.image_id IS NOT NULL AND c.recovery_save_id IS NULL
  AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.initialized_at IS NOT NULL)))
 AND (EXISTS(SELECT 1 FROM computer_commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending')
 OR EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('open','closing')
  AND (cp.id IS NULL OR EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued'))
  AND NOT EXISTS(SELECT 1 FROM held WHERE held.environment_id=s.environment_id AND held.session_id=s.id)))
 )
)
SELECT d.*, (e.max_cpu_millis-usage.cpu)::bigint remaining_cpu_millis,
 (e.max_memory_bytes-usage.memory)::bigint remaining_memory_bytes,
 (e.max_resident_computers-usage.residents)::bigint remaining_residents
FROM demand d JOIN environments e ON e.id=d.environment_id AND e.retired_at IS NULL
JOIN projects project_scope ON project_scope.id=e.project_id AND project_scope.default_region_id=sqlc.arg(region_id)
CROSS JOIN LATERAL (
 SELECT COALESCE(sum(cpu),0) cpu,COALESCE(sum(memory),0) memory,COALESCE(sum(residents),0) residents FROM (
  SELECT reserved_cpu_millis cpu,reserved_memory_bytes memory,1::bigint residents FROM computer_leases WHERE environment_id=e.id AND fenced_at IS NULL
  UNION ALL SELECT reserved_cpu_millis,reserved_memory_bytes,0 FROM computer_preparations WHERE environment_id=e.id AND worker_host_id IS NOT NULL AND fenced_at IS NULL
 ) physical
) usage
WHERE e.admission_rate_per_second IS NOT NULL
 AND e.max_cpu_millis-usage.cpu >= CASE WHEN d.required_cpu_millis>0 THEN d.required_cpu_millis ELSE ((d.resources->>'milliCpu')::bigint+999)/1000*1000 END
 AND e.max_memory_bytes-usage.memory >= CASE WHEN d.required_memory_bytes>0 THEN d.required_memory_bytes ELSE (d.resources->>'memoryMiB')::bigint*1048576 END
 AND (d.kind='preparation' OR e.max_resident_computers-usage.residents>=1)
 AND EXISTS (
  SELECT 1 FROM worker_groups target_group
  JOIN worker_pools pool ON pool.worker_group_id=target_group.id AND pool.status='active'
  JOIN worker_pool_cpu_shapes shape ON shape.worker_pool_id=pool.id
  WHERE target_group.id=sqlc.arg(worker_group_id) AND target_group.region_id=project_scope.default_region_id
   AND CASE WHEN d.required_vm_platform_id<>'' THEN
    d.required_worker_group_id=target_group.id AND pool.vm_platform_id=d.required_vm_platform_id
    AND shape.vcpu_count=d.required_vm_vcpu_count AND shape.cpu_config_digest=d.required_cpu_config_digest
    AND pool.per_vm_cpu_millis>=d.required_cpu_millis AND pool.per_vm_memory_bytes>=d.required_memory_bytes
    AND pool.per_vm_guest_ephemeral_disk_bytes>=d.required_scratch_bytes
   ELSE
    pool.id=target_group.primary_pool_id AND shape.vcpu_count=((d.resources->>'milliCpu')::bigint+999)/1000
    AND pool.per_vm_cpu_millis>=((d.resources->>'milliCpu')::bigint+999)/1000*1000
    AND pool.per_vm_memory_bytes>=(d.resources->>'memoryMiB')::bigint*1048576
   END
 )
ORDER BY d.environment_id,d.kind,d.owner_id LIMIT sqlc.arg(row_limit);

-- name: ListAllocationPlanningChargedPools :many
-- Physical reservations retain their pools independently of the queued demand budget.
SELECT DISTINCT h.worker_pool_id FROM worker_hosts h
WHERE h.worker_group_id=sqlc.arg(worker_group_id) AND (
 EXISTS(SELECT 1 FROM computer_leases l WHERE l.worker_host_id=h.id AND l.fenced_at IS NULL)
 OR EXISTS(SELECT 1 FROM computer_preparations p WHERE p.worker_host_id=h.id AND p.fenced_at IS NULL)
);
