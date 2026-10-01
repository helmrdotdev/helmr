, live_workers AS (
    SELECT worker_groups.id AS worker_group_id,
           worker_groups.primary_pool_id,
           worker_pools.id AS worker_pool_id,
           worker_hosts.id AS worker_host_id,
           worker_hosts.current_epoch AS worker_epoch,
           worker_hosts.vm_platform_id,
           vm_platforms.arch,
           vm_platforms.contract,
           worker_hosts.per_vm_cpu_millis,
           worker_hosts.per_vm_memory_bytes,
           worker_hosts.per_vm_guest_ephemeral_disk_bytes,
           worker_hosts.max_vm_slots,
           worker_hosts.max_vm_starts,
           worker_hosts.run_paused_reason,
           worker_hosts.vm_paused_reason,
           worker_hosts.epoch_cpu_millis,
           worker_hosts.epoch_memory_bytes,
           worker_hosts.epoch_guest_ephemeral_disk_bytes
      FROM worker_groups
      JOIN worker_hosts
        ON worker_hosts.worker_group_id = worker_groups.id
       AND worker_hosts.status = 'active'
       AND worker_hosts.current_epoch IS NOT NULL
      JOIN worker_pools
        ON worker_pools.id = worker_hosts.worker_pool_id
       AND worker_pools.worker_group_id = worker_hosts.worker_group_id
       AND worker_pools.status = 'active'
      JOIN vm_platforms
        ON vm_platforms.id = worker_hosts.vm_platform_id
     WHERE worker_groups.region_id = (SELECT args->>'region' FROM input)
       AND worker_groups.status = 'active'
       AND worker_hosts.observed_at >= transaction_timestamp()
           - (SELECT (args->>'max_age_seconds')::bigint FROM input) * interval '1 second'
     ORDER BY worker_hosts.id

), usage AS (
    SELECT live_workers.worker_host_id,
           COALESCE((SELECT sum(computer_instances.reserved_cpu_millis)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.reclaimed_at IS NULL), 0) AS cpu_millis,
           COALESCE((SELECT sum(computer_instances.reserved_memory_bytes)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.reclaimed_at IS NULL), 0) AS memory_bytes,
           COALESCE((SELECT sum(computer_instances.reserved_guest_ephemeral_disk_bytes)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.reclaimed_at IS NULL), 0) AS guest_ephemeral_disk_bytes,
           COALESCE((SELECT count(*) FROM computer_instances
                      WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND (computer_instances.observed_state IN ('allocated', 'ready')
                             OR (computer_instances.observed_state IN ('failed', 'lost') AND computer_instances.reclaimed_at IS NULL))), 0)::bigint AS vm_slots,
           COALESCE((SELECT count(*) FROM computer_instances
                      WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.observed_state = 'allocated'), 0)::bigint AS instance_starts
      FROM live_workers
)
, bins AS (
SELECT live_workers.worker_group_id,
       live_workers.primary_pool_id,
       live_workers.worker_pool_id,
       live_workers.worker_host_id,
       live_workers.worker_epoch,
       live_workers.vm_platform_id,
       live_workers.arch,
       live_workers.contract,
       live_workers.per_vm_cpu_millis,
       live_workers.per_vm_memory_bytes,
       live_workers.per_vm_guest_ephemeral_disk_bytes,
       GREATEST(live_workers.epoch_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(live_workers.epoch_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(live_workers.epoch_guest_ephemeral_disk_bytes - usage.guest_ephemeral_disk_bytes, 0)::bigint AS available_guest_ephemeral_disk_bytes,
       GREATEST(live_workers.max_vm_slots - usage.vm_slots, 0)::bigint AS available_vm_slots,
       GREATEST(live_workers.max_vm_starts - usage.instance_starts, 0)::bigint AS available_instance_starts,
	       live_workers.run_paused_reason,
	       live_workers.vm_paused_reason,
       COALESCE((
           SELECT array_agg(worker_pool_cpu_shapes.vcpu_count ORDER BY worker_pool_cpu_shapes.vcpu_count)
             FROM worker_pool_cpu_shapes
            WHERE worker_pool_cpu_shapes.worker_pool_id = live_workers.worker_pool_id
       ), ARRAY[]::integer[])::integer[] AS cpu_shape_vcpu_counts,
       COALESCE((
           SELECT array_agg(worker_pool_cpu_shapes.cpu_config_digest ORDER BY worker_pool_cpu_shapes.vcpu_count)
             FROM worker_pool_cpu_shapes
            WHERE worker_pool_cpu_shapes.worker_pool_id = live_workers.worker_pool_id
	       ), ARRAY[]::text[])::text[] AS cpu_shape_config_digests
  FROM live_workers
 JOIN usage USING (worker_host_id)
 ORDER BY live_workers.worker_host_id
)
SELECT COALESCE(jsonb_agg(to_jsonb(bins) ORDER BY worker_host_id),'[]'::jsonb) FROM bins;
