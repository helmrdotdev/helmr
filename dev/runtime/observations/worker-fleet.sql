, current_hosts AS (
 SELECT DISTINCT ON (h.worker_group_id,h.resource_id) h.*,g.name AS worker_group,g.region_id
 FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id,input
 WHERE g.region_id=args->>'region'
 ORDER BY h.worker_group_id,h.resource_id,
 (h.status IN ('registering','active','draining')) DESC,h.created_at DESC,h.id DESC
)
SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'id',h.id,'resource_id',h.resource_id,'worker_group',h.worker_group,'region',h.region_id,
 'status',h.status,'current_epoch',h.current_epoch,'vm_platform_id',h.vm_platform_id,
 'arch',v.arch,'contract',v.contract,'descriptor_digest',v.descriptor_digest,
 'kernel_digest',v.kernel_digest,'initramfs_digest',v.initramfs_digest,'rootfs_digest',v.rootfs_digest,
 'epoch_cpu_millis',h.epoch_cpu_millis,'epoch_memory_bytes',h.epoch_memory_bytes,
 'epoch_guest_ephemeral_disk_bytes',h.epoch_guest_ephemeral_disk_bytes,'max_vm_slots',h.max_vm_slots,
 'observation_age_seconds',extract(epoch FROM now()-h.observed_at),
 'run_paused_reason',h.run_paused_reason,'vm_paused_reason',h.vm_paused_reason,
 'created_at',h.created_at,'activated_at',h.activated_at,'observed_at',h.observed_at
) ORDER BY h.worker_group,h.resource_id),'[]'::jsonb)
FROM current_hosts h LEFT JOIN vm_platforms v ON v.id=h.vm_platform_id;
