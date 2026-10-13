package db

// AllocationUsageSQL is the physical occupancy relation for both allocation kinds.
// Logical termination or expiry never releases occupancy; only fencing does.
// A lease starts until its guest is ready; an image build starts until its first
// authenticated executor start. Keep each sqlc copy identical.
const AllocationUsageSQL = `SELECT 'computer'::text kind,environment_id,computer_id owner_id,epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 initialized_at IS NULL starting
 FROM computer_leases WHERE fenced_at IS NULL
 UNION ALL
 SELECT 'preparation',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM image_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL`
